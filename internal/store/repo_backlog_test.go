package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testRepo(t *testing.T, s *Store, path string) Repo {
	t.Helper()
	r, err := s.UpsertRepo(context.Background(), Repo{Path: path})
	if err != nil {
		t.Fatalf("UpsertRepo(%s): %v", path, err)
	}
	return r
}

// The point of the fingerprint: a nit the reviewer raises on three separate
// tasks is one thing to fix, seen three times.
func TestAddBacklogItemDedupesByFingerprint(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")

	for i := 0; i < 3; i++ {
		if _, err := s.AddBacklogItem(ctx, BacklogItem{
			RepoID: repo.ID,
			Source: BacklogReview,
			Title:  "csvutil.Write ignores the error from Flush",
		}); err != nil {
			t.Fatalf("AddBacklogItem %d: %v", i, err)
		}
	}

	items, err := s.ListBacklog(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("ListBacklog = %d items, want 1", len(items))
	}
	if items[0].Seen != 3 {
		t.Errorf("Seen = %d, want 3", items[0].Seen)
	}
}

// Line numbers move as a file is edited. The same complaint about a line that
// has shifted is still the same complaint.
func TestFingerprintIgnoresVolatileDetail(t *testing.T) {
	a := Fingerprint("csvutil.go:42 — Flush error ignored")
	b := Fingerprint("csvutil.go:118 — Flush error ignored")
	if a != b {
		t.Errorf("a moved line changed the fingerprint:\n%q\n%q", a, b)
	}
	if Fingerprint("Flush error ignored") == Fingerprint("Close error ignored") {
		t.Error("two different complaints share a fingerprint")
	}
}

// An operator who dismissed something should not have to dismiss it again on
// every review that notices it.
func TestRepeatDoesNotResurrectADismissedItem(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")

	item, err := s.AddBacklogItem(ctx, BacklogItem{
		RepoID: repo.ID, Source: BacklogReview, Title: "prefer errors.Is here",
	})
	if err != nil {
		t.Fatal(err)
	}
	item.State = BacklogDismissed
	if err := s.SaveBacklogItem(ctx, item); err != nil {
		t.Fatal(err)
	}

	again, err := s.AddBacklogItem(ctx, BacklogItem{
		RepoID: repo.ID, Source: BacklogReview, Title: "prefer errors.Is here",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.State != BacklogDismissed {
		t.Errorf("State = %q, want it to stay dismissed", again.State)
	}
	if again.Seen != 2 {
		t.Errorf("Seen = %d, want 2 — a dismissed item that keeps coming back is worth seeing", again.Seen)
	}
}

// The same complaint about two different repositories is two items: the whole
// point of a per-repo backlog is that a nit about one repo means nothing in
// another.
func TestBacklogIsScopedPerRepo(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := testRepo(t, s, "/src/widget")
	b := testRepo(t, s, "/src/gadget")

	for _, repo := range []Repo{a, b} {
		if _, err := s.AddBacklogItem(ctx, BacklogItem{
			RepoID: repo.ID, Source: BacklogReview, Title: "same nit",
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, repo := range []Repo{a, b} {
		items, err := s.ListBacklog(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Errorf("repo %s has %d items, want 1", repo.Slug, len(items))
		}
	}
}

func TestBacklogItemRoundTripsEvidence(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")

	written, err := s.AddBacklogItem(ctx, BacklogItem{
		RepoID:       repo.ID,
		Source:       BacklogAnalysis,
		Title:        "no timeout on the outbound HTTP client",
		Detail:       "http.DefaultClient has no timeout, so a hung upstream hangs the worker.",
		Evidence:     []string{"internal/fetch/client.go:18", "internal/fetch/client.go:44"},
		Severity:     "high",
		OriginTaskID: 7,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetBacklogItem(ctx, written.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Evidence) != 2 || got.Evidence[1] != "internal/fetch/client.go:44" {
		t.Errorf("Evidence = %v, want both citations", got.Evidence)
	}
	if got.Severity != "high" || got.Source != BacklogAnalysis || got.OriginTaskID != 7 {
		t.Errorf("fields lost in round trip: %+v", got)
	}
	if got.State != BacklogOpen {
		t.Errorf("State = %q, want open by default", got.State)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("timestamps not set")
	}
}

func TestAddBacklogItemRejectsIncompleteItems(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")

	if _, err := s.AddBacklogItem(ctx, BacklogItem{Source: BacklogManual, Title: "x"}); err == nil {
		t.Error("want an error for an item with no repository")
	}
	if _, err := s.AddBacklogItem(ctx, BacklogItem{RepoID: repo.ID, Title: "   "}); err == nil {
		t.Error("want an error for an item with no title")
	}
}

func TestListBacklogPutsOpenItemsFirst(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")

	for _, spec := range []struct{ title, state string }{
		{"dismissed one", BacklogDismissed},
		{"queued one", BacklogQueued},
		{"open one", BacklogOpen},
	} {
		item, err := s.AddBacklogItem(ctx, BacklogItem{
			RepoID: repo.ID, Source: BacklogManual, Title: spec.title,
		})
		if err != nil {
			t.Fatal(err)
		}
		item.State = spec.state
		if err := s.SaveBacklogItem(ctx, item); err != nil {
			t.Fatal(err)
		}
	}

	items, err := s.ListBacklog(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	if items[0].State != BacklogOpen || items[2].State != BacklogDismissed {
		t.Errorf("order = %q, %q, %q; want open first and dismissed last",
			items[0].State, items[1].State, items[2].State)
	}
}

func TestOpenBacklogCountsOnlyCountsOpen(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")

	if _, err := s.AddBacklogItem(ctx, BacklogItem{RepoID: repo.ID, Source: BacklogManual, Title: "still open"}); err != nil {
		t.Fatal(err)
	}
	done, err := s.AddBacklogItem(ctx, BacklogItem{RepoID: repo.ID, Source: BacklogManual, Title: "already queued"})
	if err != nil {
		t.Fatal(err)
	}
	done.State = BacklogQueued
	done.CreatedTaskID = 12
	if err := s.SaveBacklogItem(ctx, done); err != nil {
		t.Fatal(err)
	}

	counts, err := s.OpenBacklogCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[repo.ID] != 1 {
		t.Errorf("open count = %d, want 1", counts[repo.ID])
	}

	back, err := s.GetBacklogItem(ctx, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.CreatedTaskID != 12 {
		t.Errorf("CreatedTaskID = %d, want 12", back.CreatedTaskID)
	}
}

func TestBacklogMissesReturnErrNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.GetBacklogItem(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetBacklogItem err = %v, want ErrNotFound", err)
	}
	if err := s.SaveBacklogItem(ctx, BacklogItem{ID: 404}); !errors.Is(err, ErrNotFound) {
		t.Errorf("SaveBacklogItem err = %v, want ErrNotFound", err)
	}
}

// Deleting a repository takes its backlog with it, rather than leaving items
// pointing at nothing.
func TestBacklogCascadesWithItsRepo(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")
	if _, err := s.AddBacklogItem(ctx, BacklogItem{RepoID: repo.ID, Source: BacklogManual, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, repo.ID); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListBacklog(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("%d orphaned backlog items survived their repo", len(items))
	}
}

func TestSaveBacklogItemBumpsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")
	item, err := s.AddBacklogItem(ctx, BacklogItem{RepoID: repo.ID, Source: BacklogManual, Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	item.Title = "x, revised"
	if err := s.SaveBacklogItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBacklogItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "x, revised" {
		t.Errorf("Title = %q, want the revision", got.Title)
	}
	if got.UpdatedAt.Before(got.CreatedAt.Add(-time.Second)) {
		t.Errorf("UpdatedAt %v predates CreatedAt %v", got.UpdatedAt, got.CreatedAt)
	}
}
