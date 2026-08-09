package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/config"
	"overseer/internal/store"
)

// fakeArchitectBin writes a `claude` stand-in that emits one reply as
// stream-json, which is where the engine reads the architect's turn from. The
// reply must contain no quotes or backslashes: it is embedded in the JSON
// unescaped.
//
// printf, not echo: /bin/sh is dash on most Linux, whose echo expands backslash
// escapes and would corrupt the JSON.
func fakeArchitectBin(t *testing.T, reply string) string {
	t.Helper()
	return writeBin(t, `
printf '%s\n' '{"type":"system","subtype":"init","session_id":"arch-sess"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"`+reply+`"}]},"session_id":"arch-sess"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"arch-sess","total_cost_usd":0.30,"usage":{"input_tokens":100,"output_tokens":50}}'
`)
}

// failingArchitectBin writes a `claude` stand-in that dies with msg on stderr.
// Keep msg clear of the phrases agent.IsAuthFailure matches, or the engine
// pauses the whole run as a side effect.
func failingArchitectBin(t *testing.T, msg string) string {
	t.Helper()
	return writeBin(t, "\necho '"+msg+"' >&2\nexit 1\n")
}

func writeBin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newConfig(t *testing.T, claudeBin string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ClaudeBin = claudeBin
	return cfg
}

// The command must not exit until the architect's opening reply is in the
// database.
//
// The store is opened here, AFTER cmdNew returned and therefore after its
// deferred Close ran. A reply written by a goroutine that outlived the command
// would have hit a closed database, and this row would not exist — which is the
// whole point of the change, expressed as an assertion.
func TestNewWaitsForTheOpeningReplyBeforeItPrintsTheURL(t *testing.T) {
	cfg := newConfig(t, fakeArchitectBin(t, "Two questions before I sketch this."))
	path := filepath.Join(t.TempDir(), "syncer")

	out := captureStdout(t, func() error {
		return cmdNew(cfg, path, "a CLI that syncs S3 buckets, Go, no dependencies")
	})
	if !strings.Contains(out, path) {
		t.Errorf("the output does not say where the project went:\n%s", out)
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	props, err := st.ListProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 {
		t.Fatalf("got %d proposals, want 1", len(props))
	}
	turns, err := st.ArchitectTurns(ctx, props[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turns after the command exited, want the brief and the reply", len(turns))
	}
	if turns[1].Speaker != store.SpeakerArchitect || !strings.Contains(turns[1].Body, "Two questions") {
		t.Errorf("second turn = %s: %q, want the architect's reply", turns[1].Speaker, turns[1].Body)
	}

	// And the URL points at the conversation that has the reply in it.
	want := fmt.Sprintf("http://%s/?wizard=%d", cfg.ListenAddr, props[0].ID)
	if !strings.Contains(out, want) {
		t.Errorf("the output does not carry %q:\n%s", want, out)
	}
}

// Pointing the operator at a conversation with nothing in it is the thing this
// command exists to stop doing, so a turn that produced no reply is reported as
// a failure and prints no wizard URL.
func TestNewReportsAnArchitectThatCouldNotReply(t *testing.T) {
	cfg := newConfig(t, failingArchitectBin(t, "the architect fell over"))
	path := filepath.Join(t.TempDir(), "syncer")

	out, err := captureOutput(t, func() error {
		return cmdNew(cfg, path, "a CLI that syncs S3 buckets")
	})
	if err == nil {
		t.Fatal("cmdNew returned nil after the architect never replied")
	}
	if !strings.Contains(err.Error(), "the architect fell over") {
		t.Errorf("err = %v, want it to carry the agent's own failure", err)
	}
	if strings.Contains(out, "wizard=") {
		t.Errorf("a wizard URL was printed for a conversation with no reply in it:\n%s", out)
	}
	// The project really was created before the conversation opened, and saying
	// so is both true and the only way the operator finds it again.
	if !strings.Contains(out, path) {
		t.Errorf("the output does not say the project was created:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr != nil {
		t.Errorf("the project was not created: %v", statErr)
	}
}
