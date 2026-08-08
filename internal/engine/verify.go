package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"overseer/internal/agent"
	"overseer/internal/loop"
	"overseer/internal/sandbox"
	"overseer/internal/store"
)

// runVerify executes the task's verify command in its worktree.
//
// A non-zero exit becomes a synthetic Verdict, so the failure travels the same
// path as a Codex finding: it is rendered into the agent's next turn, stored,
// shown on the dashboard, and counted for oscillation detection. Nothing
// downstream needs to know this finding came from a command rather than a
// reviewer.
func (e *Engine) runVerify(ctx context.Context, task *store.Task) (*loop.Outcome, error) {
	command := task.VerifyCommand
	if command == "" {
		// Should be unreachable: the state machine skips the gate when no
		// command is set. Treat it as a pass rather than wedging the task.
		passed := agent.Verdict{Verdict: "approved"}
		return &loop.Outcome{Verdict: &passed}, nil
	}

	transcript := filepath.Join(e.runDir(*task),
		fmt.Sprintf("exec-%d-verify.jsonl", task.Iteration))
	if err := sandbox.EnsureDirs(e.runDir(*task)); err != nil {
		return nil, err
	}

	step, err := e.Store.StartStep(ctx, store.Step{
		TaskID: task.ID, Phase: "exec", Iteration: task.Iteration,
		Agent: "verify", TranscriptPath: transcript,
	})
	if err != nil {
		return nil, err
	}
	e.notify(task.ID)

	// Opened before the command runs, so output is streamed rather than
	// accumulated in memory. A transcript that cannot be opened is logged and
	// the run proceeds: losing the log is not a reason to fail the task.
	tf, err := os.OpenFile(transcript, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "overseer: task %d: open verify transcript: %v\n", task.ID, err)
	}
	if tf != nil {
		// One header line keeps the dashboard's transcript view uniform; the
		// rest of the file is the command's raw output, not JSONL.
		fmt.Fprintf(tf, "{\"type\":\"overseer.verify\",\"command\":%q}\n", command)
	}

	var sink io.Writer
	if tf != nil {
		sink = tf
	}
	res := e.execVerify(ctx, *task, command, sink)

	if tf != nil {
		fmt.Fprintf(tf, "\n{\"type\":\"overseer.verify.result\",\"exit_code\":%d}\n", res.ExitCode)
		tf.Close()
	}

	step.ExitCode = res.ExitCode
	if res.ExitCode == 0 {
		step.Verdict = "approved"
		if err := e.Store.FinishStep(ctx, step, nil); err != nil {
			return nil, err
		}
		e.notify(task.ID)
		// An explicit approved verdict, not a bare Outcome. A verify outcome
		// must always carry a verdict, because afterVerify reads a nil one as
		// "the harness could not produce a result" and fails the task —
		// otherwise `true` exiting zero would fail the task it just passed.
		passed := agent.Verdict{Verdict: "approved"}
		return &loop.Outcome{Verdict: &passed}, nil
	}

	findings := VerifyFindings(command, res.ExitCode, res.Output)
	verdict := agent.Verdict{Verdict: "changes_requested", Findings: findings}
	step.Verdict = verdict.Verdict

	var stored []store.Finding
	for _, f := range findings {
		stored = append(stored, store.Finding{
			Severity: string(f.Severity),
			Summary:  f.Summary,
			Detail:   f.Detail,
			// Always blocking: severity is critical, which passes every
			// threshold.
			Blocking: true,
		})
	}
	if err := e.Store.FinishStep(ctx, step, stored); err != nil {
		return nil, err
	}
	e.notify(task.ID)
	return &loop.Outcome{Verdict: &verdict}, nil
}

// VerifyResult is one verify command execution.
type VerifyResult struct {
	Command  string
	ExitCode int
	Output   string
}

// execVerify runs the command under the same sandbox and timeout as an agent,
// streaming its combined output to transcript (which may be nil) while
// retaining only a bounded tail for the finding.
//
// There are deliberately no retries. A verify failure is a result, not a
// transport error, and guessing which failures are infrastructural from their
// output would silently retry genuine test failures whose messages happen to
// mention a network.
func (e *Engine) execVerify(ctx context.Context, task store.Task, command string, transcript io.Writer) VerifyResult {
	timeout := e.Cfg.StepTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin, args := "/bin/sh", []string{"-c", command}
	if e.Sandbox != nil {
		// The same spec Claude gets: the worktree must be writable, since
		// building and testing produce artefacts.
		//
		// Required mounts must exist before Wrap: bubblewrap aborts on a
		// missing --bind source. When runVerify calls this, runAgent already
		// prepared this state for the plan/exec turn that preceded the
		// verify step; execVerify prepares it again itself (idempotently) so
		// it is also safe to call directly, as the tests do.
		if err := sandbox.EnsureDirs(e.runDir(task)); err != nil {
			return VerifyResult{Command: command, ExitCode: -1,
				Output: fmt.Sprintf("overseer: prepare sandbox: %v", err)}
		}
		if err := e.prepareAgentState(task, "claude"); err != nil {
			return VerifyResult{Command: command, ExitCode: -1,
				Output: fmt.Sprintf("overseer: prepare sandbox: %v", err)}
		}
		// Writable: the verify command builds and tests, which produces
		// artefacts in the worktree.
		bin, args = e.Sandbox.Wrap(bin, args, e.sandboxSpec(task, "claude", true))
	}

	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Dir = task.WorktreeDir
	cmd.Stdin = strings.NewReader("")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Output is streamed to the transcript and only a bounded tail is kept in
	// memory. CombinedOutput would buffer everything until the command exits
	// or the timeout fires, so one chatty or looping command could exhaust the
	// daemon's memory and take every concurrent task down with it. Truncating
	// afterwards does not help: the allocation has already happened.
	tail := &tailWriter{max: maxVerifyOutput}
	var sink io.Writer = tail
	if transcript != nil {
		sink = io.MultiWriter(&limitedWriter{w: transcript, remaining: maxTranscriptBytes}, tail)
	}
	cmd.Stdout = sink
	cmd.Stderr = sink

	done := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-done:
		}
	}()

	err := cmd.Run()
	close(done)

	res := VerifyResult{Command: command, Output: tail.String()}
	if runCtx.Err() != nil {
		res.ExitCode = -1
		res.Output += fmt.Sprintf("\noverseer: verify timed out after %s\n", timeout)
		return res
	}
	if err != nil {
		res.ExitCode = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		}
	}
	return res
}

// maxTranscriptBytes caps what one verify run may write to disk. A command
// stuck in a print loop would otherwise fill the filesystem.
const maxTranscriptBytes = 32 << 20

// tailWriter retains only the last max bytes written to it, so unbounded
// command output costs bounded memory.
type tailWriter struct {
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n >= w.max {
		w.buf = append(w.buf[:0], p[n-w.max:]...)
		return n, nil
	}
	if len(w.buf)+n > w.max {
		drop := len(w.buf) + n - w.max
		// copy semantics handle the overlap correctly.
		w.buf = append(w.buf[:0], w.buf[drop:]...)
	}
	w.buf = append(w.buf, p...)
	return n, nil
}

// String returns the retained tail.
func (w *tailWriter) String() string { return string(w.buf) }

// limitedWriter stops writing after remaining bytes and notes the truncation
// once, rather than failing the command.
type limitedWriter struct {
	w         io.Writer
	remaining int64
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		if !w.truncated {
			w.truncated = true
			fmt.Fprintf(w.w, "\noverseer: output truncated\n")
		}
		// Report success: the command is not at fault, and an error here
		// would surface as a spurious failure.
		return len(p), nil
	}
	toWrite := p
	if int64(len(p)) > w.remaining {
		toWrite = p[:w.remaining]
	}
	n, err := w.w.Write(toWrite)
	w.remaining -= int64(n)
	if err != nil {
		return n, err
	}
	return len(p), nil
}

// failureLine matches the lines worth fingerprinting: test failures and
// compiler errors, rather than progress chatter.
var failureLine = regexp.MustCompile(`(?i)(^|\s)(---\s+FAIL|FAIL[:\s]|FAILED|panic:|error:|Error:|assert|expected)`)

// digits collapses numbers so timings, line offsets and addresses do not make
// every run look different.
var digits = regexp.MustCompile(`\d+`)

// tempPaths collapses per-run temporary directories.
var tempPaths = regexp.MustCompile(`/tmp/[^\s:)]+`)

// NormalizeFailureOutput reduces command output to a stable set of failure
// lines.
//
// This exists for oscillation detection. Fingerprinting raw output would make
// every failure unique — timings and temp paths differ each run — so a task
// stuck on one broken test would never be recognised as stuck and would burn
// the entire iteration budget instead of escalating in three rounds.
func NormalizeFailureOutput(output string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !failureLine.MatchString(line) {
			continue
		}
		norm := tempPaths.ReplaceAllString(line, "/tmp/X")
		norm = digits.ReplaceAllString(norm, "N")
		norm = strings.Join(strings.Fields(norm), " ")
		if seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, norm)
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// maxVerifyOutput bounds how much command output is fed back to the agent.
const maxVerifyOutput = 8000

// VerifyFindings turns a failed verify run into findings.
//
// The summary carries two things with different jobs: a normalised form, which
// keeps the fingerprint stable so oscillation detection works, and the tail of
// the real output, which is what the agent actually needs to fix the problem.
func VerifyFindings(command string, exitCode int, output string) []agent.Finding {
	normalized := NormalizeFailureOutput(output)
	if len(normalized) == 0 {
		// Nothing recognisable: fall back to the exit code, which is at least
		// stable across runs.
		normalized = []string{fmt.Sprintf("exited %d", exitCode)}
	}

	tail := output
	if len(tail) > maxVerifyOutput {
		tail = "..." + tail[len(tail)-maxVerifyOutput:]
	}

	// Summary holds only stable content, because Verdict.Fingerprint hashes
	// it. The raw output goes in Detail, which the fingerprint ignores —
	// embedding it here instead would make every failure hash differently and
	// silently disable the oscillation detection this normalisation exists to
	// enable.
	summary := fmt.Sprintf("`%s` failed (exit %d). Failures:\n%s",
		command, exitCode, strings.Join(normalized, "\n"))

	// Critical so it blocks at every configured threshold: a failing build
	// must not be waved through because the operator relaxed the review bar.
	return []agent.Finding{{
		Severity: agent.SevCritical,
		Summary:  summary,
		Detail:   strings.TrimSpace(tail),
	}}
}
