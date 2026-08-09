package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/store"
)

// writeScript and waitFor mirror the helpers in internal/engine's tests. Go
// gives test helpers no way across a package boundary short of an exported
// testing package, which is more machinery than sixteen lines are worth.
func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// newConfig points a command at a throwaway data directory and a fake agent.
//
// The sandbox is off, unlike config.Default's "auto", because these tests watch
// the agent process from outside: under bwrap the pid overseer holds is bwrap's
// rather than the fixture shell's, and the sandbox hides every path the test
// owns. What is under test here is the command's signal handling.
func newConfig(t *testing.T, claudeBin string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ClaudeBin = claudeBin
	cfg.CodexBin = "true"
	cfg.Sandbox = "off"
	cfg.AnalysisTimeout = 30 * time.Second
	return cfg
}

// conversation reopens the database the command owned and returns the turns of
// the one conversation in it. Reopening is the point: it can only see what the
// command actually committed and closed behind it.
func conversation(t *testing.T, cfg config.Config) []store.ArchitectTurn {
	t.Helper()
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	proposals, err := st.ListProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 {
		t.Fatalf("got %d proposal(s), want the one the command created", len(proposals))
	}
	turns, err := st.ArchitectTurns(ctx, proposals[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return turns
}

// agent.Runner puts the agent in a process group of its own, so the terminal's
// Ctrl-C is delivered to overseer and to nothing else. Cancelling this context
// is the only thing that reaches the agent — which is why every command that
// starts one needs it.
func TestInterruptibleCancelsOnSIGINT(t *testing.T) {
	ctx, stop := interruptible()
	defer stop()

	// Sent only after the handler is installed, so this cannot race the default
	// disposition and take the test binary down with it.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not cancel the context")
	}
}

// The URL the command prints has to lead somewhere. Returning before the
// opening turn landed pointed the operator at a conversation containing nothing
// but their own brief, with the goroutine that would have filled it in killed
// by the process exiting.
func TestNewWaitsForTheOpeningReply(t *testing.T) {
	cfg := newConfig(t, writeScript(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"arch-sess"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Two questions."}]},"session_id":"arch-sess"}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"arch-sess","total_cost_usd":0.3,"usage":{"input_tokens":10,"output_tokens":5}}'
`))
	project := filepath.Join(t.TempDir(), "thing")

	if err := cmdNew(context.Background(), cfg, project, "a CLI that syncs S3 buckets"); err != nil {
		t.Fatalf("cmdNew: %v", err)
	}

	turns := conversation(t, cfg)
	if len(turns) != 2 {
		t.Fatalf("got %d turn(s), want the brief and the reply", len(turns))
	}
	if turns[1].Speaker != store.SpeakerArchitect {
		t.Errorf("second turn = %s, want the architect", turns[1].Speaker)
	}
	if !strings.Contains(turns[1].Body, "Two questions") {
		t.Errorf("architect said %q", turns[1].Body)
	}
}

// The whole defect, end to end. Interrupting `overseer new` must kill the
// agent's process group, record the interruption, and return — so the deferred
// st.Close() runs — rather than exiting the process and leaving the agent
// running with nothing written down.
func TestNewInterruptedKillsTheAgentAndRecordsTheFailure(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "agent.pid")
	cfg := newConfig(t, writeScript(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"arch-sess"}'
echo $$ > `+pidFile+`
# Hang until the process group is killed.
while true; do sleep 0.05; done
`))
	project := filepath.Join(t.TempDir(), "thing")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmdNew(ctx, cfg, project, "a thing") }()

	var pid int
	waitFor(t, "the architect to start", func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 0
	})

	cancel()

	// Returning at all is the proof that the deferred cleanup ran: the command
	// unwound rather than the process being killed out from under it.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an interrupted `overseer new` reported success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cmdNew did not return after its context was cancelled; " +
			"the deferred store cleanup would never have run")
	}

	// Nothing but the cancelled context could have killed it: it is in a
	// process group of its own, so it never saw a SIGINT.
	waitFor(t, "the agent process to be gone", func() bool {
		return syscall.Kill(pid, 0) != nil
	})

	turns := conversation(t, cfg)
	if len(turns) != 2 {
		t.Fatalf("got %d turn(s), want the brief and a recorded interruption", len(turns))
	}
	if !strings.Contains(turns[1].ErrMsg, "interrupted") {
		t.Errorf("the interrupted turn recorded %q", turns[1].ErrMsg)
	}
}

// Ported from the branch that added StartDesignAndWait, where the conversation
// URL was deliberately withheld on a failed turn. That is now the opposite: the
// proposal exists whatever became of the turn, it holds the failure as a turn of
// its own, and the operator's next move is to open it — so the URL is the useful
// half of the message, not a lie about having succeeded.
func TestNewReportsAnArchitectThatCouldNotReply(t *testing.T) {
	cfg := newConfig(t, writeScript(t, "claude", `
echo 'the architect fell over' >&2
exit 1
`))
	project := filepath.Join(t.TempDir(), "syncer")

	out, err := captureOutput(t, func() error {
		return cmdNew(context.Background(), cfg, project, "a CLI that syncs S3 buckets")
	})
	if err == nil {
		t.Fatal("cmdNew returned nil after the architect never replied")
	}
	if !strings.Contains(err.Error(), "the architect fell over") {
		t.Errorf("err = %v, want it to carry the agent's own failure", err)
	}
	if !strings.Contains(out, "wizard=") {
		t.Errorf("a failed turn must still say where the conversation is:\n%s", out)
	}
	// The project really was created before the conversation opened, and saying
	// so is both true and the only way the operator finds it again.
	if !strings.Contains(out, project) {
		t.Errorf("the output does not say the project was created:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(project, ".git")); statErr != nil {
		t.Errorf("the project was not created: %v", statErr)
	}
	// The failure is on the record as a turn, which is what keeps the
	// conversation continuable rather than wedged.
	turns := conversation(t, cfg)
	if len(turns) != 2 || turns[1].ErrMsg == "" {
		t.Errorf("the failure was not recorded as a turn: %+v", turns)
	}
}
