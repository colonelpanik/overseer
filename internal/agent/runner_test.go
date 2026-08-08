package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"overseer/internal/sandbox"
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

// hangSeconds is how long the fake agents below sleep. These tests distinguish
// "the runner killed the process on its deadline" from "the runner waited for
// the process to finish on its own", so any bound comfortably under this value
// proves the point without measuring how loaded the machine is.
const hangSeconds = 30

func TestRunTimeoutKillsProcessAndReportsRetryable(t *testing.T) {
	bin := writeFakeAgent(t, fmt.Sprintf("sleep %d", hangSeconds))
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
	// Deliberately generous, and deliberately derived from the sleep rather
	// than being a round number. Pinning this near the 300ms deadline would
	// test the scheduler, not the runner — this package's tests run alongside
	// packages that spawn bubblewrap and git.
	if elapsed := time.Since(start); elapsed > (hangSeconds/2)*time.Second {
		t.Errorf("timeout took %v; the process was not killed on its deadline", elapsed)
	}
	if !strings.Contains(res.ErrMsg, "timeout") {
		t.Errorf("ErrMsg = %q, want it to mention timeout", res.ErrMsg)
	}
	if !res.Retryable {
		t.Error("a timeout must be Retryable")
	}
}

func TestRunStreamErrorSurvivesAndIsNotRetryableEvenAfterATimeout(t *testing.T) {
	// The agent reports a specific, non-retryable failure and then hangs.
	// The generic timeout classification must not clobber it: doing so
	// would turn a non-retryable auth failure into something the engine
	// retries three times for nothing.
	//
	// This test has to get the agent's line out BEFORE the deadline fires, so
	// the deadline must be long enough that process startup cannot lose the
	// race. It was originally 300ms, and that was the source of a real flake:
	// measured over 60 samples under 3x CPU oversubscription, time-to-first-
	// output of a fake agent was median 15.7ms, p95 47.1ms, worst 231.5ms.
	// A 300ms deadline is 1.3x that worst case, which is why it passed almost
	// always and failed exactly when the machine was loaded — this package's
	// tests run beside ones that spawn bubblewrap and git. Losing the race
	// makes ErrMsg the generic timeout, failing both assertions below for a
	// reason unrelated to what is under test.
	//
	// 2s is ~9x the worst case measured under deliberately pathological load.
	// The marker file below turns any remaining environment failure into a
	// clear diagnostic instead of a confusing assertion mismatch.
	ready := filepath.Join(t.TempDir(), "wrote")
	bin := writeFakeAgent(t, fmt.Sprintf(
		`echo '{"type":"error","message":"not logged in"}'
touch %s
sleep %d`, ready, hangSeconds))
	r := NewCodexRunner(bin)

	res, err := r.Run(context.Background(), RunSpec{
		Args: []string{"x"}, Dir: t.TempDir(),
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Timeout:        2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, statErr := os.Stat(ready); statErr != nil {
		t.Fatalf("the fake agent never got far enough to emit its error line "+
			"within the deadline, so this test proved nothing: %v", statErr)
	}
	if res.ErrMsg != "not logged in" {
		t.Errorf("ErrMsg = %q, want the stream error to survive the timeout", res.ErrMsg)
	}
	if res.Retryable {
		t.Error("a non-retryable stream error must not become Retryable just because the process later timed out")
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
			// Well above the wall below, so a run that finishes normally is
			// never mistaken for a hang and a genuine hang is still caught.
			Timeout: 120 * time.Second,
		}); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	// This is a deadlock detector, not a latency budget: the distinction it
	// draws is "returns" versus "never returns". A tight bound here buys
	// nothing and costs a flake on a loaded machine.
	select {
	case <-done:
	case <-time.After(60 * time.Second):
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

func TestRunAppliesTheSandboxWrapper(t *testing.T) {
	// A stub wrapper proves the runner routes through it without needing
	// bwrap on the test host.
	bin := writeFakeAgent(t, `echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s","total_cost_usd":0,"usage":{"input_tokens":1,"output_tokens":1}}'`)
	r := NewClaudeRunner("this-binary-does-not-exist")

	var n int
	stub := stubWrapper{bin: bin, n: &n}
	res, err := r.Run(context.Background(), RunSpec{
		Args:           []string{"-p", "hi"},
		Dir:            t.TempDir(),
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Timeout:        30 * time.Second,
		Sandbox:        stub,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ErrMsg != "" {
		t.Fatalf("ErrMsg = %q; the wrapper's binary should have run", res.ErrMsg)
	}
	if stub.calls() == 0 {
		t.Error("the runner did not consult the sandbox wrapper")
	}
}

type stubWrapper struct {
	bin string
	n   *int
}

func (s stubWrapper) Wrap(_ string, args []string, _ sandbox.Spec) (string, []string) {
	if s.n != nil {
		*s.n++
	}
	return s.bin, args
}
func (stubWrapper) Name() string { return "stub" }
func (s stubWrapper) calls() int {
	if s.n == nil {
		return 0
	}
	return *s.n
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
		// Bare digit and short-word markers must not match inside longer
		// words or numbers: a token count, a version string, a byte count,
		// and "eof" hiding inside an unrelated word.
		"invalid_json_schema: expected 500 max tokens field",
		"unknown flag: --nope (see docs at v1.503.0)",
		"wrote 3429 bytes to disk before failing schema validation",
		"schema violation: geoff is not a valid enum value",
	}
	for _, m := range fatal {
		if IsRetryable(m) {
			t.Errorf("IsRetryable(%q) = true, want false", m)
		}
	}
}
