package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeFakeAgent creates an executable script that prints body to stdout.
func writeFakeAgent(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake agent scripts require a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-agent")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunCapturesSessionResultAndTranscript(t *testing.T) {
	bin := writeFakeAgent(t, `
cat <<'EOF'
{"type":"system","subtype":"init","session_id":"sess-42"}
{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"session_id":"sess-42"}
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-42","total_cost_usd":0.5,"usage":{"input_tokens":10,"output_tokens":3}}
EOF`)
	transcript := filepath.Join(t.TempDir(), "t.jsonl")

	r := NewClaudeRunner(bin)
	res, err := r.Run(context.Background(), RunSpec{
		Args:           []string{"ignored"},
		Dir:            t.TempDir(),
		TranscriptPath: transcript,
		Timeout:        30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SessionID != "sess-42" {
		t.Errorf("SessionID = %q, want sess-42", res.SessionID)
	}
	if res.FinalText != "done" {
		t.Errorf("FinalText = %q, want done", res.FinalText)
	}
	if res.CostUSD != 0.5 || res.InputTokens != 10 || res.OutputTokens != 3 {
		t.Errorf("usage not captured: %+v", res)
	}
	if res.ErrMsg != "" {
		t.Errorf("ErrMsg = %q, want empty", res.ErrMsg)
	}

	raw, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("transcript not written: %v", err)
	}
	if strings.Count(strings.TrimSpace(string(raw)), "\n") != 2 {
		t.Errorf("transcript should hold 3 lines, got:\n%s", raw)
	}
}

func TestRunUsesLastAgentMessageAsFinalText(t *testing.T) {
	bin := writeFakeAgent(t, `
cat <<'EOF'
{"type":"item.completed","item":{"type":"agent_message","text":"first"}}
{"type":"item.completed","item":{"type":"reasoning","text":"noise"}}
{"type":"item.completed","item":{"type":"agent_message","text":"second"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}
EOF`)
	r := NewCodexRunner(bin)
	res, err := r.Run(context.Background(), RunSpec{
		Args: []string{"x"}, Dir: t.TempDir(),
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Timeout:        30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "second" {
		t.Errorf("FinalText = %q, want second", res.FinalText)
	}
}

func TestRunNonZeroExitPopulatesErrMsg(t *testing.T) {
	bin := writeFakeAgent(t, `echo '{"type":"turn.failed","error":{"message":"boom"}}'
exit 1`)
	r := NewCodexRunner(bin)
	res, err := r.Run(context.Background(), RunSpec{
		Args: []string{"x"}, Dir: t.TempDir(),
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Timeout:        30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run should report failure in Result, not error: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0, want non-zero")
	}
	if !strings.Contains(res.ErrMsg, "boom") {
		t.Errorf("ErrMsg = %q, want it to include the stream error", res.ErrMsg)
	}
}

func TestRunTimeoutKillsProcessAndReportsRetryable(t *testing.T) {
	bin := writeFakeAgent(t, `sleep 30`)
	r := NewCodexRunner(bin)

	start := time.Now()
	res, err := r.Run(context.Background(), RunSpec{
		Args: []string{"x"}, Dir: t.TempDir(),
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Timeout:        300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %v; the process was not killed promptly", elapsed)
	}
	if !strings.Contains(res.ErrMsg, "timeout") {
		t.Errorf("ErrMsg = %q, want it to mention timeout", res.ErrMsg)
	}
	if !res.Retryable {
		t.Error("a timeout must be Retryable")
	}
}

func TestRunClosesStdinSoCodexDoesNotHang(t *testing.T) {
	// codex exec appends piped stdin to the prompt and waits for EOF. If
	// the runner leaves stdin open, this read blocks forever.
	bin := writeFakeAgent(t, `cat > /dev/null
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`)
	r := NewCodexRunner(bin)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := r.Run(context.Background(), RunSpec{
			Args: []string{"x"}, Dir: t.TempDir(),
			TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
			Timeout:        10 * time.Second,
		}); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run blocked: stdin was not closed")
	}
}

func TestRunInvokesOnEventForEachLine(t *testing.T) {
	bin := writeFakeAgent(t, `
cat <<'EOF'
{"type":"system","subtype":"init","session_id":"s"}
{"type":"assistant","message":{"content":[{"type":"text","text":"a"}]},"session_id":"s"}
EOF`)
	var seen []EventKind
	r := NewClaudeRunner(bin)
	if _, err := r.Run(context.Background(), RunSpec{
		Args: []string{"x"}, Dir: t.TempDir(),
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Timeout:        30 * time.Second,
		OnEvent:        func(e Event) { seen = append(seen, e.Kind) },
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != EventInit || seen[1] != EventMessage {
		t.Errorf("OnEvent kinds = %v, want [init message]", seen)
	}
}

func TestRunMissingBinaryIsAFailedResultNotAnError(t *testing.T) {
	// If this returned an error instead, the engine could not close the step
	// or fail the task, and the scheduler would re-claim it forever.
	r := NewClaudeRunner(filepath.Join(t.TempDir(), "does-not-exist"))
	res, err := r.Run(context.Background(), RunSpec{
		Args: []string{"-p", "hi"}, Dir: t.TempDir(),
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Timeout:        30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run returned a harness error: %v", err)
	}
	if res.ErrMsg == "" {
		t.Fatal("ErrMsg is empty; the start failure was not reported")
	}
	if res.Retryable {
		t.Error("a missing binary must not be retryable")
	}
}

func TestRunNonExecutableBinaryIsAFailedResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntrue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewClaudeRunner(path).Run(context.Background(), RunSpec{
		Args: []string{"x"}, Dir: t.TempDir(),
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Timeout:        30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run returned a harness error: %v", err)
	}
	if res.ErrMsg == "" {
		t.Error("a permission-denied start must be reported in ErrMsg")
	}
}

func TestIsRetryable(t *testing.T) {
	retryable := []string{
		"rate limit exceeded",
		"429 Too Many Requests",
		"connection reset by peer",
		"502 Bad Gateway",
		"context deadline exceeded",
		"step timeout after 30m",
	}
	for _, m := range retryable {
		if !IsRetryable(m) {
			t.Errorf("IsRetryable(%q) = false, want true", m)
		}
	}
	fatal := []string{
		"not logged in",
		"invalid_json_schema",
		"unknown flag: --nope",
		"",
	}
	for _, m := range fatal {
		if IsRetryable(m) {
			t.Errorf("IsRetryable(%q) = true, want false", m)
		}
	}
}
