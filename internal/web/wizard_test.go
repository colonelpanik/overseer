package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"overseer/internal/store"
)

// readyProposal seeds a proposal sitting on the review step.
func readyProposal(t *testing.T, st *store.Store) store.Proposal {
	t.Helper()
	ctx := context.Background()
	p, err := st.CreateProposal(ctx, store.Proposal{
		RepoPath: "/home/kal/code/dc-planner", State: store.ProposalReady,
		Model: "claude-sonnet-5", MaxTasks: 12, CostUSD: 0.41,
		Detected: "Go · go test ./...",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceProposalTasks(ctx, p.ID, []store.ProposalTask{
		{Key: "wal", Goal: "Enable WAL mode on the store connection.",
			Verify: "go test ./...", Severity: "any", CostCap: 8, Selected: true,
			Rationale: "store.go opens without a busy timeout",
			Evidence:  []string{"internal/store/store.go:33"}},
		{Key: "readme", Goal: "Rewrite the README.", Severity: "minor", Selected: false},
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWizardOpensOnTheSourceStepWithNothingCreated(t *testing.T) {
	// Creating a proposal row the moment somebody opens the wizard would
	// litter the database with abandoned drafts.
	s, st := newTestServer(t)
	body := get(t, s, "/?wizard=-1").Body.String()
	for _, want := range []string{"Analyse a repository", "Repository already on this machine", "Clone a repository"} {
		if !strings.Contains(body, want) {
			t.Errorf("source step missing %q", want)
		}
	}
	ps, err := st.ListProposals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Errorf("proposals = %+v, want none until a source is chosen", ps)
	}
}

func TestWizardShowsTheStepMatchingTheProposalState(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	draft, err := st.CreateProposal(ctx, store.Proposal{
		RepoPath: "/repo", State: store.ProposalDraft, MaxTasks: 12,
		Detected: "Go · go test ./...",
	})
	if err != nil {
		t.Fatal(err)
	}
	focus := get(t, s, "/?wizard=1").Body.String()
	if !strings.Contains(focus, "What should it look for?") {
		t.Error("a draft proposal should show the focus step")
	}
	if !strings.Contains(focus, "Go · go test ./...") {
		t.Error("the focus step should show what the probe detected")
	}
	if !strings.Contains(focus, "/analyse/1/focus") {
		t.Error("the focus step needs its route")
	}

	draft.State = store.ProposalAnalysing
	if err := st.SaveProposal(ctx, draft); err != nil {
		t.Fatal(err)
	}
	running := get(t, s, "/?wizard=1").Body.String()
	if !strings.Contains(running, "Reading the repository") {
		t.Error("an analysing proposal should show the run step")
	}
	if !strings.Contains(running, "read-only") {
		t.Error("the run step should say the repository cannot be written to")
	}
}

func TestWizardReviewStepListsProposalsWithTheirEvidence(t *testing.T) {
	s, st := newTestServer(t)
	readyProposal(t, st)

	body := get(t, s, "/?wizard=1").Body.String()
	for _, want := range []string{
		"Enable WAL mode on the store connection.",
		"store.go opens without a busy timeout",
		"internal/store/store.go:33",
		"verify: go test ./...",
		"Queue 1 task",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("review step missing %q", want)
		}
	}
	// One of the two is deselected, so the count has to say one.
	if strings.Contains(body, "Queue 2 tasks") {
		t.Error("the queue button should count only the selected rows")
	}
}

func TestWizardClosesForAProposalThatIsGone(t *testing.T) {
	// A stale bookmark, or a proposal queued in another tab, must not fail
	// the whole page.
	s, st := newTestServer(t)
	ctx := context.Background()
	p, err := st.CreateProposal(ctx, store.Proposal{
		RepoPath: "/repo", State: store.ProposalQueued, MaxTasks: 12,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/?wizard=999", "/?wizard=1"} {
		rec := get(t, s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "Analyse a repository") {
			t.Errorf("%s: the wizard should be closed", path)
		}
	}
	_ = p
}

func TestPostAnalyseNeedsExactlyOneSource(t *testing.T) {
	s, _ := newTestServer(t)
	for name, form := range map[string]url.Values{
		"neither": {},
		"both":    {"repo": {"/tmp"}, "url": {"https://example.test/x.git"}},
	} {
		if rec := post(t, s, "/analyse", form); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestPostAnalyseStartsAProposalForALocalRepo(t *testing.T) {
	s, st := newTestServer(t)
	repo := initRepo(t)

	rec := post(t, s, "/analyse", url.Values{"repo": {repo}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	// The redirect has to reopen the wizard on the proposal just created, or
	// the operator lands on a board with their work nowhere in sight.
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "wizard=1") {
		t.Errorf("Location = %q, want the wizard reopened", loc)
	}
	ps, err := st.ListProposals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].State != store.ProposalDraft {
		t.Errorf("proposals = %+v", ps)
	}
}

func TestPostAnalyseRefusesATransportItWillNotFetch(t *testing.T) {
	s, _ := newTestServer(t)
	// ext:: runs the command named in the URL; the daemon must never fetch it.
	rec := post(t, s, "/analyse", url.Values{"url": {"ext::id"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "https://") {
		t.Errorf("body = %q, want it to explain which transports are allowed", rec.Body.String())
	}
}

func TestPostAnalyseFocusValidatesTheBudget(t *testing.T) {
	s, st := newTestServer(t)
	if _, err := st.CreateProposal(context.Background(), store.Proposal{
		RepoPath: "/repo", State: store.ProposalDraft, MaxTasks: 12,
	}); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		"not a number": "twelve",
		"zero":         "0",
		"absurd":       "500",
	} {
		rec := post(t, s, "/analyse/1/focus", url.Values{"max_tasks": {v}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestPostAnalyseTaskTogglesAndEdits(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	p := readyProposal(t, st)
	rows, err := st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	if rec := post(t, s, "/analyse/1/task/1", url.Values{"action": {"toggle"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("toggle: status = %d: %s", rec.Code, rec.Body.String())
	}
	after, err := st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].Selected == rows[0].Selected {
		t.Error("toggle did not change the selection")
	}

	rec := post(t, s, "/analyse/1/task/1", url.Values{
		"action": {"save"}, "goal": {"Edited goal"}, "verify": {"make test"},
		"severity": {"major"}, "cost_cap": {"20"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d: %s", rec.Code, rec.Body.String())
	}
	after, err = st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].Goal != "Edited goal" || after[0].Verify != "make test" ||
		after[0].Severity != "major" || after[0].CostCap != 20 {
		t.Errorf("task = %+v", after[0])
	}
}

func TestPostAnalyseTaskRejectsBadInput(t *testing.T) {
	s, st := newTestServer(t)
	readyProposal(t, st)

	cases := map[string]url.Values{
		"empty goal":     {"action": {"save"}, "goal": {"   "}},
		"negative cap":   {"action": {"save"}, "goal": {"g"}, "cost_cap": {"-1"}},
		"cap not number": {"action": {"save"}, "goal": {"g"}, "cost_cap": {"lots"}},
		"unknown action": {"action": {"delete"}},
	}
	for name, form := range cases {
		if rec := post(t, s, "/analyse/1/task/1", form); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestPostAnalyseTaskCannotReachAnotherProposalsRow(t *testing.T) {
	s, st := newTestServer(t)
	readyProposal(t, st) // proposal 1, tasks 1 and 2
	other := readyProposal(t, st)

	// Task 1 belongs to proposal 1, so addressing it through proposal 2 must
	// not work however the URL was arrived at.
	rec := post(t, s, "/analyse/2/task/1", url.Values{"action": {"toggle"}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	_ = other
}

func TestPostAnalyseQueueCreatesTasksAndLandsOnOne(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	repo := initRepo(t)

	p, err := st.CreateProposal(ctx, store.Proposal{
		RepoPath: repo, State: store.ProposalReady, MaxTasks: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceProposalTasks(ctx, p.ID, []store.ProposalTask{
		{Key: "a", Goal: "Enable WAL mode", Verify: "go test ./...",
			Severity: "major", CostCap: 8, Selected: true},
		{Key: "b", Goal: "Not this one", Severity: "any", Selected: false},
	}); err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/analyse/1/queue", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/task/") {
		t.Errorf("Location = %q, want the task just queued", loc)
	}

	tasks, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want only the selected one", len(tasks))
	}
	if tasks[0].VerifyCommand != "go test ./..." || tasks[0].BlockingSeverity != "major" ||
		tasks[0].CostCapUSD != 8 {
		t.Errorf("task = %+v", tasks[0])
	}
}

func TestPostAnalyseDiscardClosesTheWizard(t *testing.T) {
	s, st := newTestServer(t)
	readyProposal(t, st)

	rec := post(t, s, "/analyse/1/discard", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want the board", loc)
	}
	ps, err := st.ListProposals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Errorf("proposals = %+v, want the discarded one hidden", ps)
	}
}

func TestEveryWizardRouteRejectsACrossSiteRequest(t *testing.T) {
	// The wizard spends money and clones URLs. A page open in the same browser
	// must not be able to drive any of it.
	s, st := newTestServer(t)
	readyProposal(t, st)

	routes := []string{
		"/analyse", "/analyse/1/focus", "/analyse/1/regenerate",
		"/analyse/1/task/1", "/analyse/1/queue", "/analyse/1/discard",
	}
	for _, path := range routes {
		// What a hostile page's form post actually looks like: the browser
		// attaches Sec-Fetch-Site itself and the page cannot forge it.
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = s.cfg.ListenAddr
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: cross-site status = %d, want 403", path, rec.Code)
		}

		// And DNS rebinding: a name the attacker controls resolving to
		// loopback still cannot forge the Host header.
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "evil.test:7777"
		rec = httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: rebound Host status = %d, want 403", path, rec.Code)
		}
	}
}

func TestAnalysisSpendCountsTowardsTheRunTotal(t *testing.T) {
	// An analysis costs real money and belongs to no task. Leaving it out
	// would make the headline figure quietly wrong.
	s, st := newTestServer(t)
	ctx := context.Background()
	p, err := st.CreateProposal(ctx, store.Proposal{
		RepoPath: "/repo", State: store.ProposalReady, MaxTasks: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.CostUSD = 1.25
	if err := st.SaveProposal(ctx, p); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, "$1.25") {
		t.Error("the header's run spend should include what analyses have cost")
	}
}

func TestWizardStateSurvivesTheReloadTheDaemonTriggers(t *testing.T) {
	// The page reloads on every state event. The wizard is in the URL for
	// exactly this reason, so a task changing underneath must not close it.
	s, st := newTestServer(t)
	readyProposal(t, st)
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "unrelated", RepoPath: "/r", Goal: "Something else", State: "executing",
	}); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/?wizard=1&sel=1&filter=running").Body.String()
	if !strings.Contains(body, "Analyse a repository") {
		t.Error("the wizard should still be open")
	}
	if !strings.Contains(body, "Enable WAL mode on the store connection.") {
		t.Error("the wizard should still show its proposals")
	}
}
