package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// bobBlockedArgs protect the ACP stdio transport and the trust gate the
// daemon relies on. `acp`, `chat`, `run`, and `mcp` are Bob's other top-level
// subcommands — allowing a profile to swap in one of them would replace the
// JSON-RPC transport with an interactive or argv-only mode Multica cannot
// drive. `-p`/`--prompt` and `-r`/`--resume` belong to `bob run`, not `bob
// acp`, but are blocked defensively since they overlap with values the
// daemon sets itself over the ACP protocol (the prompt text and the resume
// session id). `--trust` is blocked so custom_args cannot remove the literal
// the daemon always appends — see the comment in Execute. `--disable-mcp`
// would silently break MCP wiring behind a flag a user might not expect to
// interact with the daemon's mcp_config. `-f`/`--format` and `-h`/`--help`
// would swap or short-circuit the transport entirely.
//
// --accept-license, --team-id, --disable-subagents, and --disable-tool-groups
// are deliberately NOT blocked: they are reachable through custom_args. In
// particular, Multica never adds --accept-license itself — see Execute.
var bobBlockedArgs = map[string]blockedArgMode{
	"acp":            blockedStandalone,
	"chat":           blockedStandalone,
	"run":            blockedStandalone,
	"mcp":            blockedStandalone,
	"-p":             blockedWithValue,
	"--prompt":       blockedWithValue,
	"-r":             blockedWithValue,
	"--resume":       blockedWithValue,
	"--auto-approve": blockedStandalone,
	"--trust":        blockedStandalone,
	"--disable-mcp":  blockedStandalone,
	"-f":             blockedWithValue,
	"--format":       blockedWithValue,
	"-h":             blockedStandalone,
	"--help":         blockedStandalone,
}

// bobBackend runs IBM Bob as an ACP agent server via `bob acp --trust`.
// Bob owns its Runtime, Session, permission, and tool-approval loops; this
// adapter only maps the shared ACP stream into Multica's Backend contract.
//
// Unlike mcode, Bob's ACP handshake reports agentCapabilities.loadSession:
// true unconditionally, so resume always attempts session/load with no
// capability gate beforehand — closer to how qwenpaw treats session/load.
//
// --trust is always appended to argv (not merely shown in the launch
// header): Bob refuses to create a run context for an untrusted directory,
// and every directory Multica hands to a task is untrusted from Bob's
// perspective since each task gets a fresh worktree. Multica never adds
// --accept-license on the operator's behalf; that remains a manual opt-in
// via custom_args, and an operator who has not accepted the license will see
// Bob's own createRunContext error surface through providerErr.
//
// Model selection is unsupported: Bob has no -m/--model flag and its ACP
// server does not implement session/set_model ("Model selection is not
// supported; use modes instead"), so ExecOptions.Model is ignored — see
// ModelSelectionSupported.
type bobBackend struct {
	cfg Config
}

var bobReaderDrainGrace = 2 * time.Second

var errBobProcessExited = errors.New("bob process exited")

type bobMessageStream struct {
	ch     chan Message
	mu     sync.Mutex
	closed bool
}

func newBobMessageStream(size int) *bobMessageStream {
	return &bobMessageStream{ch: make(chan Message, size)}
}

func (s *bobMessageStream) send(message Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	trySend(s.ch, message)
}

func (s *bobMessageStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

func (b *bobBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "bob"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("bob executable not found at %q: %w", execPath, err)
	}

	mcpServers, err := buildACPMcpServers(opts.McpConfig, b.cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("bob: invalid mcp_config: %w", err)
	}

	runCtx, cancel := runContext(ctx, opts.Timeout)

	// --trust is mandatory here, not just in the display-only launch header:
	// without it Bob's approval.autoApprovalEnabled stays false for an
	// untrusted directory and createRunContext throws before any session
	// exists. It is blocked in bobBlockedArgs so custom_args cannot drop it.
	args := []string{"acp", "--trust"}
	args = append(args, filterCustomArgs(opts.ExtraArgs, bobBlockedArgs, b.cfg.Logger)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, bobBlockedArgs, b.cfg.Logger)...)
	cmd := b.cfg.commandAt(execPath).exec(runCtx, args...)
	hideAgentWindow(cmd)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			signalProcessGroup(cmd, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = bobReaderDrainGrace
	b.cfg.logAgentCommand(cmd, newAgentCommandLogArgs(args, trustAgentCommandPositional(0, "acp")))
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildEnv(b.cfg.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("bob stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("bob stdin pipe: %w", err)
	}
	providerErr := newACPProviderErrorSniffer("bob")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("bob stderr pipe: %w", err)
	}
	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		cancel()
		return nil, fmt.Errorf("start bob: %w", err)
	}

	stderrSink := io.MultiWriter(newLogWriter(b.cfg.Logger, "[bob:stderr] "), providerErr)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderrSink, stderr)
	}()

	b.cfg.Logger.Info("bob acp started", "pid", cmd.Process.Pid, "cwd", opts.Cwd)
	msgStream := newBobMessageStream(256)
	resCh := make(chan Result, 1)
	var deliverable acpDeliverableTracker
	var streamingCurrentTurn atomic.Bool
	promptDone := make(chan hermesPromptResult, 1)
	activity := make(chan struct{}, 1)

	c := &hermesClient{
		cfg:          b.cfg,
		stdin:        stdin,
		pending:      make(map[int]*pendingRPC),
		pendingTools: make(map[string]*pendingToolCall),
		acceptNotification: func(string) bool {
			return streamingCurrentTurn.Load()
		},
		onActivity: func() {
			select {
			case activity <- struct{}{}:
			default:
			}
		},
		onMessage: func(message Message) {
			if !streamingCurrentTurn.Load() {
				return
			}
			deliverable.observe(message)
			msgStream.send(message)
		},
		onPromptDone: func(result hermesPromptResult) {
			if !streamingCurrentTurn.Load() {
				return
			}
			select {
			case promptDone <- result:
			default:
			}
		},
	}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := newAgentStreamScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				c.handleLine(line)
			}
		}
		c.closeAllPending(errBobProcessExited)
	}()

	go func() {
		defer msgStream.close()
		defer close(resCh)
		defer func() {
			_ = stdin.Close()
			// Cancellation must remain reachable before Wait. Bob or a tool
			// descendant may ignore stdin EOF, so waiting first can deadlock every
			// early-return path, including a rejected session resume.
			cancel()
			_ = cmd.Wait()
			releaseProcessGroup(cmd)
		}()

		startTime := time.Now()
		finalStatus := "completed"
		var finalError string
		var sessionID string
		var resumeRejected bool

		initResult, err := c.request(runCtx, "initialize", map[string]any{
			"protocolVersion": 1,
			"clientInfo": map[string]any{
				"name":    "multica-agent-sdk",
				"version": "0.2.0",
			},
			"clientCapabilities": map[string]any{},
		})
		if err != nil {
			finalStatus, finalError = bobRequestFailure(runCtx, opts.Timeout, "initialize", err)
			resCh <- Result{Status: finalStatus, Error: finalError, DurationMs: time.Since(startTime).Milliseconds()}
			return
		}

		mcpServers = filterACPMcpServersByCapability(
			mcpServers,
			extractACPMcpCapabilities(initResult),
			"bob",
			b.cfg,
		)
		cwd := opts.Cwd
		if cwd == "" {
			cwd = "."
		}

		if opts.ResumeSessionID != "" {
			// Bob reports agentCapabilities.loadSession: true unconditionally, so
			// resume always attempts session/load — unlike mcode, there is no
			// capability check to gate this on.
			result, loadErr := c.request(runCtx, "session/load", map[string]any{
				"cwd":        cwd,
				"sessionId":  opts.ResumeSessionID,
				"mcpServers": mcpServers,
			})
			if loadErr != nil {
				finalStatus, finalError = bobRequestFailure(runCtx, opts.Timeout, "session/load", loadErr)
				if finalStatus == "failed" && isACPSessionNotFound(loadErr) {
					resumeRejected = true
				}
				resCh <- Result{
					Status:         finalStatus,
					Error:          finalError,
					DurationMs:     time.Since(startTime).Milliseconds(),
					ResumeRejected: resumeRejected,
				}
				return
			}
			sessionID, _ = resolveResumedSessionID(opts.ResumeSessionID, result)
		} else {
			result, newErr := c.request(runCtx, "session/new", map[string]any{
				"cwd":        cwd,
				"mcpServers": mcpServers,
			})
			if newErr != nil {
				finalStatus, finalError = bobRequestFailure(runCtx, opts.Timeout, "session/new", newErr)
				resCh <- Result{Status: finalStatus, Error: finalError, DurationMs: time.Since(startTime).Milliseconds()}
				return
			}
			sessionID = extractACPSessionID(result)
			if sessionID == "" {
				resCh <- Result{
					Status:     "failed",
					Error:      "bob session/new returned no session ID",
					DurationMs: time.Since(startTime).Milliseconds(),
				}
				return
			}
		}

		c.sessionID = sessionID
		if opts.SystemPrompt != "" {
			b.cfg.Logger.Debug("bob ignoring ExecOptions.SystemPrompt; using cwd-scoped AGENTS.md", "cwd", opts.Cwd)
		}
		streamingCurrentTurn.Store(true)
		_, err = c.request(runCtx, "session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt": []map[string]any{
				{"type": "text", "text": prompt},
			},
		})
		if err != nil {
			finalStatus, finalError = bobRequestFailure(runCtx, opts.Timeout, "session/prompt", err)
			if opts.ResumeSessionID != "" && isACPSessionNotFound(err) {
				resumeRejected = true
				sessionID = ""
			}
		} else {
			select {
			case result := <-promptDone:
				c.mergeUsage(result.usage)
				if result.stopReason == "cancelled" {
					finalStatus = "aborted"
					finalError = "execution cancelled"
				}
			default:
			}
			waitForACPNotificationQuiescence(runCtx, activity, readerDone, acpNotificationQuietTime, bobReaderDrainGrace)
		}
		streamingCurrentTurn.Store(false)

		duration := time.Since(startTime)
		_ = stdin.Close()
		cancel()
		<-readerDone
		<-stderrDone

		finalOutput, fullOutput := deliverable.result()
		finalStatus, finalError = promoteACPResultOnProviderError(
			finalStatus,
			finalError,
			fullOutput,
			providerErr,
		)
		var usage map[string]TokenUsage
		if accumulated := c.accumulatedUsage(); acpUsagePresent(accumulated) {
			// Model selection is unsupported (see ModelSelectionSupported), so the
			// backend never sends opts.Model to Bob. Attribute usage to "unknown"
			// rather than a model that was never applied.
			usage = map[string]TokenUsage{"unknown": accumulated}
		}
		resCh <- Result{
			Status:         finalStatus,
			Output:         finalOutput,
			Error:          finalError,
			DurationMs:     duration.Milliseconds(),
			SessionID:      sessionID,
			ResumeRejected: resumeRejected,
			Usage:          usage,
		}
	}()

	return &Session{Messages: msgStream.ch, Result: resCh}, nil
}

func bobRequestFailure(ctx context.Context, timeout time.Duration, operation string, err error) (string, string) {
	if ctx.Err() == context.DeadlineExceeded {
		return "timeout", fmt.Sprintf("bob timed out during %s after %s", operation, timeout)
	}
	if ctx.Err() == context.Canceled {
		return "aborted", "execution cancelled"
	}
	return "failed", fmt.Sprintf("bob %s failed: %v", operation, err)
}
