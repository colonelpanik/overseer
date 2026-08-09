package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"overseer/internal/store"
)

func TestDesignScreensRender(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	// The first screen, before anything exists.
	body := get(t, s, "/?wizard=-2").Body.String()
	for _, want := range []string{"Design it together", "new_path", "Start designing", `name="brief"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the design first screen is missing %q", want)
		}
	}

	// The conversation.
	p, err := st.CreateProposal(ctx, store.Proposal{
		Kind: store.ProposalCreate, State: store.ProposalDesigning,
		Notes: "a CLI that syncs S3 buckets", RepoPath: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range []store.ArchitectTurn{
		{ProposalID: p.ID, Speaker: store.SpeakerOperator, Body: "a CLI that syncs S3 buckets"},
		{ProposalID: p.ID, Speaker: store.SpeakerArchitect, Body: "One way or two? And does it need to resume?", CostUSD: 0.3},
	} {
		if _, err := st.AddArchitectTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?wizard=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("wizard -> %d", rec.Code)
	}
	body = rec.Body.String()
	for _, want := range []string{
		"syncs S3 buckets", "One way or two", `id="reply"`,
		"Accept the design", "turn mine", "turn theirs",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the conversation is missing %q", want)
		}
	}
}

// Everything that changes state goes through the same-origin check.
func TestEveryDesignRouteRequiresSameOrigin(t *testing.T) {
	s, st := newTestServer(t)
	p, err := st.CreateProposal(context.Background(), store.Proposal{
		Kind: store.ProposalCreate, State: store.ProposalDesigning,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(p.ID, 10)
	for _, path := range []string{"/design", "/design/" + id + "/say", "/design/" + id + "/accept"} {
		rec := crossSitePost(t, s, path, url.Values{"brief": {"x"}, "message": {"x"}})
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s from another site = %d, want 403", path, rec.Code)
		}
	}
}

// The dashboard reloads on every state event, and the architect notifies every
// couple of seconds while it answers. Without a guard, a reply box would be
// wiped mid-sentence — so the guard has to exist and has to look at the box.
func TestTheReloadGuardProtectsEveryComposeBox(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	body := get(t, s, "/").Body.String()

	if !strings.Contains(body, "holdsTyping") {
		t.Fatal("no reload guard; typing a reply would be destroyed every two seconds")
	}
	// Guarded, not removed: everything else still reloads.
	if !strings.Contains(body, "if (!holdsTyping()) location.reload();") {
		t.Error("the change event no longer reloads at all")
	}
	// The contract between the script and the markup is a class, not one id,
	// so a second conversation cannot be left unprotected by omission.
	if !strings.Contains(body, `querySelectorAll(".holds-typing")`) {
		t.Error("the guard no longer selects compose boxes by class")
	}

	// And every surface that has one actually carries it. Asserting on the
	// rendered page rather than the template source is what makes this catch
	// the real regression — a new conversation that forgot the class — while
	// surviving any rewrite of the script itself.
	p, err := st.CreateProposal(ctx, store.Proposal{
		Kind: store.ProposalCreate, State: store.ProposalDesigning, Notes: "a thing",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateChat(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}

	// Matched against the textarea itself, not anywhere on the page: the guard
	// script names the class too, so a plain substring check would pass on a
	// page whose box had lost it.
	marked := regexp.MustCompile(`<textarea[^>]*class="[^"]*holds-typing`)
	for _, page := range []string{
		"/?wizard=" + itoa(p.ID),
		"/?overlay=chat&repo=" + itoa(repo.ID),
	} {
		if !marked.MatchString(get(t, s, page).Body.String()) {
			t.Errorf("%s has a compose box the reload guard will wipe", page)
		}
	}
}

// A design against an existing repository is a redesign, and must not offer to
// create anything.
func TestDesigningAnExistingRepoDoesNotCreateAProject(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	dir := initRepo(t)

	rec := post(t, s, "/design", url.Values{
		"repo": {dir}, "brief": {"make the storage layer resumable"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	props, err := st.ListProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 {
		t.Fatalf("got %d proposals, want 1", len(props))
	}
	if props[0].Kind != store.ProposalAnalyse {
		t.Errorf("Kind = %q; a design against an existing repository is not a new project", props[0].Kind)
	}
	if props[0].RepoPath != dir {
		t.Errorf("RepoPath = %q, want %q", props[0].RepoPath, dir)
	}
	// The brief is the first turn, so the conversation reads from the top.
	turns, err := st.ArchitectTurns(ctx, props[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || turns[0].Speaker != store.SpeakerOperator {
		t.Error("the brief was not recorded as the operator's first turn")
	}
}

// A new project is created before the conversation starts, so the architect has
// somewhere real to work and the operator can see where it went.
func TestDesigningANewProjectCreatesTheRepository(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "new-thing")

	rec := post(t, s, "/design", url.Values{
		"new_path": {path}, "brief": {"a CLI that syncs S3 buckets"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		t.Fatalf("the project was not created: %v", err)
	}
	props, err := st.ListProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props[0].Kind != store.ProposalCreate {
		t.Fatalf("proposals = %+v, want one create", props)
	}
	if props[0].RepoPath != path {
		t.Errorf("RepoPath = %q, want %q", props[0].RepoPath, path)
	}
	// The brief, plus whatever the background reply turned into. Without this
	// the test races its own TempDir cleanup.
	waitForArchitectTurns(t, st, props[0].ID, 2)
}
