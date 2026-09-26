package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBobACPScript impersonates `bob acp --trust` for unit tests. Bob's
// handshake reports agentCapabilities.loadSession: true unconditionally
// (verified against 2.0.5), so session/load is always answered rather than
// gated behind a capability flag as mcode's fake is.
func fakeBobACPScript() string {
	return `#!/bin/sh
while IFS= read -r line; do
  if [ -n "$BOB_REQUESTS_FILE" ]; then
    printf '%s\n' "$line" >> "$BOB_REQUESTS_FILE"
  fi
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":true,"sse":true}}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"bob-session-new"}}\n' "$id"
      ;;
    *'"method":"session/load"'*)
      if [ -n "$BOB_SESSION_NOT_FOUND" ]; then
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"session not found"}}\n' "$id"
        exit 0
      fi
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":20}}}\n' "$id"
      sleep 0.05
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"bob-session-new","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Bob completed the task"}}}}\n'
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
      ;;
  esac
done
`
}

func writeFakeBobScript(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bob")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bob: %v", err)
	}
	return bin
}

func TestBobModelSelectionIsRuntimeManaged(t *testing.T) {
	t.Parallel()
	if ModelSelectionSupported("bob") {
		t.Fatal("bob has no -m/--model flag and no session/set_model RPC; model selection must be unsupported")
	}
}

func TestBobListModels(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "invoked")
	bin := writeFakeBobScript(t, "#!/bin/sh\ntouch '"+marker+"'\nexit 0\n")

	cat, err := ListModels(context.Background(), "bob", Command{Path: bin})
	if err != nil {
		t.Fatalf("bob ListModels should not error, got: %v", err)
	}
	if len(cat.Models) != 0 {
		t.Fatalf("bob ListModels should return empty catalog, got %d models", len(cat.Models))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("bob ListModels executed the CLI; it must return an empty catalog without spawning a discovery subprocess")
	}
}

func TestBobFreshSessionUsesACPAndForwardsMCP(t *testing.T) {
	t.Parallel()
	bin := writeFakeBobScript(t, fakeBobACPScript())
	reqFile := filepath.Join(t.TempDir(), "requests.txt")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	b, err := New("bob", Config{
		ExecutablePath: bin,
		Logger:         logger,
		Env:            map[string]string{"BOB_REQUESTS_FILE": reqFile},
	})
	if err != nil {
		t.Fatalf("New(bob) error: %v", err)
	}

	session, err := b.Execute(context.Background(), "ship it", ExecOptions{
		Cwd:       t.TempDir(),
		McpConfig: []byte(`{"mcpServers":{"docs":{"command":"docs-server","args":["--stdio"]}}}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var text strings.Builder
	for message := range session.Messages {
		if message.Type == MessageText {
			text.WriteString(message.Content)
		}
	}
	result := <-session.Result
	if result.Status != "completed" || result.SessionID != "bob-session-new" {
		t.Fatalf("result = %+v", result)
	}
	if result.Output != "Bob completed the task" {
		t.Fatalf("result output = %q, want final Bob message", result.Output)
	}
	if !strings.Contains(text.String(), "Bob completed the task") {
		t.Fatalf("streamed text = %q", text.String())
	}

	raw, err := os.ReadFile(reqFile)
	if err != nil {
		t.Fatalf("read requests: %v", err)
	}
	requests := string(raw)
	for _, want := range []string{`"method":"session/new"`, `"mcpServers"`, `"docs-server"`, `"method":"session/prompt"`, `"text":"ship it"`} {
		if !strings.Contains(requests, want) {
			t.Fatalf("requests missing %s:\n%s", want, requests)
		}
	}
}

// TestBobArgsAlwaysIncludeTrust pins the non-negotiable requirement that
// --trust reaches argv on every launch, not merely the display-only launch
// header: Bob refuses to create a run context for an untrusted directory,
// and every Multica task directory is untrusted from Bob's perspective.
func TestBobArgsAlwaysIncludeTrust(t *testing.T) {
	t.Parallel()
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" > "%s"
while IFS= read -r line; do
  id=$(printf '%%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"sessionId":"bob-trust-check"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
      ;;
  esac
done
`, argsFile)
	bin := writeFakeBobScript(t, script)

	b, err := New("bob", Config{ExecutablePath: bin, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New(bob) error: %v", err)
	}
	session, err := b.Execute(context.Background(), "test prompt", ExecOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range session.Messages {
	}
	<-session.Result

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if !strings.Contains(string(args), "--trust") {
		t.Fatalf("bob args = %q, want --trust present", string(args))
	}
}

// TestBobBlockedArgsCannotRemoveTrust verifies that a custom_args attempt to
// strip --trust via the blocked-args filter is a no-op: --trust is always
// injected by Execute and is itself in bobBlockedArgs, so it cannot be
// overridden either way.
func TestBobBlockedArgsCannotRemoveTrust(t *testing.T) {
	t.Parallel()
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" > "%s"
while IFS= read -r line; do
  id=$(printf '%%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"sessionId":"bob-blocked-check"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
      ;;
  esac
done
`, argsFile)
	bin := writeFakeBobScript(t, script)

	b, err := New("bob", Config{ExecutablePath: bin, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New(bob) error: %v", err)
	}
	session, err := b.Execute(context.Background(), "test prompt", ExecOptions{
		Cwd:        t.TempDir(),
		CustomArgs: []string{"chat", "--auto-approve", "--accept-license"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range session.Messages {
	}
	<-session.Result

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	got := string(args)
	if strings.Contains(got, "chat") || strings.Contains(got, "--auto-approve") {
		t.Fatalf("bob args = %q, want chat/--auto-approve stripped by bobBlockedArgs", got)
	}
	if !strings.Contains(got, "--accept-license") {
		t.Fatalf("bob args = %q, want --accept-license reachable through custom_args", got)
	}
	if strings.Count(got, "--trust") != 1 {
		t.Fatalf("bob args = %q, want exactly one --trust", got)
	}
}

// TestBobUsesSessionLoadUnconditionally verifies that resume always attempts
// session/load with no capability gate beforehand — unlike mcode, which
// hard-rejects resume when agentCapabilities.loadSession is false.
func TestBobUsesSessionLoadUnconditionally(t *testing.T) {
	t.Parallel()
	bin := writeFakeBobScript(t, fakeBobACPScript())
	reqFile := filepath.Join(t.TempDir(), "requests.txt")

	b, err := New("bob", Config{
		ExecutablePath: bin,
		Logger:         slog.Default(),
		Env:            map[string]string{"BOB_REQUESTS_FILE": reqFile},
	})
	if err != nil {
		t.Fatalf("New(bob) error: %v", err)
	}
	session, err := b.Execute(context.Background(), "continue", ExecOptions{
		Cwd:             t.TempDir(),
		ResumeSessionID: "old-bob-session",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "completed" || result.SessionID != "old-bob-session" {
		t.Fatalf("result = %+v", result)
	}
	if result.ResumeRejected {
		t.Fatal("expected ResumeRejected=false on successful load")
	}

	raw, err := os.ReadFile(reqFile)
	if err != nil {
		t.Fatalf("read requests: %v", err)
	}
	if !strings.Contains(string(raw), `"method":"session/load"`) {
		t.Fatalf("expected session/load on resume, got requests:\n%s", raw)
	}
}

func TestBobSessionLoadNotFoundRejectsResume(t *testing.T) {
	t.Parallel()
	bin := writeFakeBobScript(t, fakeBobACPScript())

	b, err := New("bob", Config{
		ExecutablePath: bin,
		Logger:         slog.Default(),
		Env:            map[string]string{"BOB_SESSION_NOT_FOUND": "1"},
	})
	if err != nil {
		t.Fatalf("New(bob) error: %v", err)
	}
	session, err := b.Execute(context.Background(), "continue", ExecOptions{
		Cwd:             t.TempDir(),
		ResumeSessionID: "ses_gone",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "failed" {
		t.Fatalf("expected failed, got status=%q error=%q", result.Status, result.Error)
	}
	if !result.ResumeRejected {
		t.Fatal("expected ResumeRejected=true on session not found")
	}
}

func TestBobBackendUsage(t *testing.T) {
	t.Parallel()
	bin := writeFakeBobScript(t, fakeBobACPScript())

	b, err := New("bob", Config{ExecutablePath: bin, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New(bob) error: %v", err)
	}
	session, err := b.Execute(context.Background(), "test prompt", ExecOptions{
		Cwd:   t.TempDir(),
		Model: "must-not-be-reported",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Usage == nil {
		t.Fatal("expected usage in result")
	}
	if _, ok := result.Usage["must-not-be-reported"]; ok {
		t.Fatal("opts.Model must not be used as usage attribution key for bob")
	}
	usage, ok := result.Usage["unknown"]
	if !ok {
		t.Fatalf("expected usage entry for model 'unknown', got %+v", result.Usage)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 20 {
		t.Fatalf("usage = %+v, want InputTokens=10 OutputTokens=20", usage)
	}
}
