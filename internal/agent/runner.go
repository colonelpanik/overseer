package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Result summarises one completed agent invocation.
type Result struct {
	SessionID      string
	FinalText      string
	ExitCode       int
	CostUSD        float64
	InputTokens    int
	OutputTokens   int
	ErrMsg         string
	TranscriptPath string
	// Retryable is true when ErrMsg describes a transient condition. The
	// engine retries these without spending an iteration.
	Retryable bool
}

// RunSpec is one invocation's parameters.
type RunSpec struct {
	Args           []string
	Dir            string
	TranscriptPath string
	Timeout        time.Duration
	// OnEvent, if set, is called for every parsed event as it arrives. It
	// runs on the reader goroutine, so it must not block.
	OnEvent func(Event)
	// Attempt is the 1-based retry attempt, written into the transcript as a
	// marker so a retried step's history stays readable.
	Attempt int
}

// Runner executes one agent CLI and normalises its output.
type Runner struct {
	Name  string
	Bin   string
	Parse func([]byte) (Event, error)
}

// NewClaudeRunner returns a Runner for the Claude CLI.
func NewClaudeRunner(bin string) *Runner {
	return &Runner{Name: "claude", Bin: bin, Parse: ParseClaudeLine}
}

// NewCodexRunner returns a Runner for the Codex CLI.
func NewCodexRunner(bin string) *Runner {
	return &Runner{Name: "codex", Bin: bin, Parse: ParseCodexLine}
}

// Run executes the agent, streaming its stdout through the parser while
// writing every line verbatim to the transcript file.
//
// A failing agent is reported in Result.ErrMsg, not as an error. Run's error
// return is reserved for failures of the harness itself, such as being
// unable to create the transcript file.
func (r *Runner) Run(ctx context.Context, spec RunSpec) (Result, error) {
	res := Result{TranscriptPath: spec.TranscriptPath}

	if err := os.MkdirAll(filepath.Dir(spec.TranscriptPath), 0o755); err != nil {
		return res, fmt.Errorf("create transcript dir: %w", err)
	}
	// Appended, not truncated: a retried step reuses the same path, and
	// os.Create would discard the failed attempt that caused the retry —
	// exactly the output needed to understand why it failed.
	tf, err := os.OpenFile(spec.TranscriptPath,
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return res, fmt.Errorf("open transcript: %w", err)
	}
	defer tf.Close()

	// A marker delimits attempts within the one transcript. Both parsers
	// classify an unknown type as EventOther, so this is inert to them.
	if spec.Attempt > 0 {
		fmt.Fprintf(tf, "{\"type\":\"overseer.attempt\",\"attempt\":%d,\"agent\":%q}\n",
			spec.Attempt, r.Name)
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(r.Bin, spec.Args...)
	cmd.Dir = spec.Dir
	// Stdin must be an immediately-EOF reader: codex exec appends piped
	// stdin to the prompt and blocks waiting for EOF.
	cmd.Stdin = strings.NewReader("")
	// A dedicated process group lets us kill the agent's children too; an
	// agent that spawned a test runner would otherwise survive.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		// A missing, non-executable, or mis-sandboxed binary is an agent
		// problem, not a harness problem, so it is reported in the Result.
		// Returning an error here instead would leave the engine unable to
		// close the step or transition the task, and the scheduler would
		// re-claim the still-active task every poll — an infinite retry that
		// accumulates a "running" step row each time.
		res.ExitCode = -1
		res.ErrMsg = fmt.Sprintf("start %s: %v", r.Bin, err)
		return res, nil
	}

	// Kill the whole process group when the context expires.
	killed := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-killed:
		}
	}()

	streamErr := r.consume(stdout, tf, spec.OnEvent, &res)
	waitErr := cmd.Wait()
	close(killed)

	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
		}
	}

	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		res.ErrMsg = fmt.Sprintf("step timeout after %s", timeout)
	case res.ErrMsg != "":
		// An error already reported inside the stream wins: it is more
		// specific than the exit status.
	case streamErr != nil:
		res.ErrMsg = streamErr.Error()
	case waitErr != nil:
		res.ErrMsg = fmt.Sprintf("%s: %v: %s", r.Bin, waitErr,
			truncate(strings.TrimSpace(stderr.String()), 500))
	}
	res.Retryable = res.ErrMsg != "" && IsRetryable(res.ErrMsg)
	return res, nil
}

// consume reads JSONL from stdout, mirrors it to the transcript, and folds
// each event into res.
func (r *Runner) consume(stdout io.Reader, transcript io.Writer, onEvent func(Event), res *Result) error {
	sc := bufio.NewScanner(stdout)
	// Agent lines can be large: a single init event lists every tool.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	// A transcript write failure is recorded and reported, but must not stop
	// the loop: returning early would leave stdout unread, and once the pipe
	// buffer filled the agent would block forever. Wait could then not return
	// until the step timeout killed it, disguising an immediate disk failure
	// as a thirty-minute hang.
	var writeErr error

	for sc.Scan() {
		line := sc.Bytes()
		if writeErr == nil {
			if _, err := transcript.Write(append(append([]byte{}, line...), '\n')); err != nil {
				writeErr = fmt.Errorf("write transcript: %w", err)
			}
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		ev, err := r.Parse(line)
		if err != nil {
			// A line we cannot parse is not fatal to the run; the verdict
			// is read from the final message, which is validated
			// separately.
			continue
		}
		switch ev.Kind {
		case EventInit:
			if ev.SessionID != "" {
				res.SessionID = ev.SessionID
			}
		case EventMessage:
			if ev.Text != "" {
				res.FinalText = ev.Text
			}
		case EventResult:
			res.CostUSD += ev.CostUSD
			res.InputTokens += ev.InputTokens
			res.OutputTokens += ev.OutputTokens
			if ev.ErrMsg != "" && res.ErrMsg == "" {
				res.ErrMsg = ev.ErrMsg
			}
		case EventError, EventRateLimit:
			if ev.ErrMsg != "" && res.ErrMsg == "" {
				res.ErrMsg = ev.ErrMsg
			}
		}
		if ev.SessionID != "" && res.SessionID == "" {
			res.SessionID = ev.SessionID
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read agent output: %w", err)
	}
	// Reported only after the pipe is fully drained.
	return writeErr
}

// retryableMarkers are substrings that indicate a transient failure.
var retryableMarkers = []string{
	"rate limit", "rate_limit", "429",
	"500", "502", "503", "504",
	"connection reset", "connection refused", "broken pipe",
	"timeout", "deadline exceeded", "temporarily unavailable",
	"overloaded", "eof",
}

// IsRetryable reports whether an agent error message describes a transient
// condition worth retrying. Authentication and usage errors are not
// retryable: repeating them wastes time and money.
func IsRetryable(msg string) bool {
	if msg == "" {
		return false
	}
	l := strings.ToLower(msg)
	for _, m := range retryableMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
