package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"overseer/internal/store"
)

func TestBoardShowsThePauseBanner(t *testing.T) {
	s, st := newTestServer(t)
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "t", RepoPath: "/r", Goal: "g", State: "queued",
	}); err != nil {
		t.Fatal(err)
	}
	s.eng.Pause("codex is not authenticated: not logged in")

	body := get(t, s, "/").Body.String()
	for _, want := range []string{"not authenticated", "Resume"} {
		if !strings.Contains(body, want) {
			t.Errorf("board missing %q while paused", want)
		}
	}
}

func TestBoardHasNoBannerWhenRunning(t *testing.T) {
	s, _ := newTestServer(t)
	if strings.Contains(get(t, s, "/").Body.String(), "Resume run") {
		t.Error("banner shown while not paused")
	}
}

func TestPostResumeClearsThePause(t *testing.T) {
	s, _ := newTestServer(t)
	s.eng.Pause("no credentials")

	if rec := post(t, s, "/resume", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if s.eng.PauseReason() != "" {
		t.Error("POST /resume did not clear the pause")
	}
}
