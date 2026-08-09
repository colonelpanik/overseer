package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"overseer/internal/config"
	"overseer/internal/store"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
//
// Safe for the volumes here — one repository and one or two backlog rows, far
// inside a pipe's buffer. A command that printed more than 64KB before this
// read would deadlock, so do not reach for it to test cmdLogs.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = saved
	w.Close()

	out, readErr := io.ReadAll(r)
	r.Close()
	if runErr != nil {
		t.Fatalf("the command failed: %v", runErr)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(out)
}

// seedBacklog puts one item on one repository's backlog and closes the store,
// because cmdBacklog opens the database itself.
func seedBacklog(t *testing.T, item store.BacklogItem) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatal(err)
	}
	item.RepoID = repo.ID
	if item.Source == "" {
		item.Source = store.BacklogAnalysis
	}
	if _, err := st.AddBacklogItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBacklogCommandListsTheSubjectNotTheWholeGoal(t *testing.T) {
	const title = "Add a cached projection of the rack inventory query. " +
		"It recomputes the whole join on every request."
	cfg := seedBacklog(t, store.BacklogItem{
		Subject: "Cache the rack inventory query", Title: title,
	})

	out := captureStdout(t, func() error { return cmdBacklog(cfg, "") })
	if !strings.Contains(out, "Cache the rack inventory query") {
		t.Errorf("the table does not lead with the subject:\n%s", out)
	}
	if strings.Contains(out, "recomputes the whole join") {
		t.Errorf("the table still prints the whole goal:\n%s", out)
	}
}

func TestBacklogCommandDerivesAHeadlineForAnOlderItem(t *testing.T) {
	// Filed before the column existed, and never raised again to fill it in.
	cfg := seedBacklog(t, store.BacklogItem{
		Title: "Add a cached projection of the rack inventory query. It recomputes the join.",
	})
	out := captureStdout(t, func() error { return cmdBacklog(cfg, "") })
	if !strings.Contains(out, "Add a cached projection of the rack inventory query") {
		t.Errorf("the table shows no headline for an item with no subject:\n%s", out)
	}
	if strings.Contains(out, "It recomputes the join") {
		t.Errorf("the table still prints the whole title:\n%s", out)
	}
}

func TestBacklogCommandTruncatesOnACharacterBoundary(t *testing.T) {
	// The column caps at 64 and a headline may be 72 runes, so this really does
	// truncate. Slicing bytes would cut a three-byte character in half and put
	// a replacement glyph in the table.
	cfg := seedBacklog(t, store.BacklogItem{
		Subject: strings.Repeat("日", 70), Title: "whatever the goal was",
	})
	out := captureStdout(t, func() error { return cmdBacklog(cfg, "") })
	if !utf8.ValidString(out) {
		t.Error("the backlog table contains invalid UTF-8")
	}
	if !strings.Contains(out, "...") {
		t.Errorf("a 70-rune headline should have been elided:\n%s", out)
	}
}
