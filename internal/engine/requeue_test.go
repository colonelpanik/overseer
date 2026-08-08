package engine

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"overseer/internal/agent"
	"overseer/internal/store"
)

// readyProposalWith seeds a reviewed analysis with the given rows.
func readyProposalWith(t *testing.T, h *harness, rows []store.ProposalTask) store.Proposal {
	t.Helper()
	ctx := context.Background()
	p, err := h.st.CreateProposal(ctx, store.Proposal{
		RepoPath: h.repo, State: store.ProposalReady, MaxTasks: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.ReplaceProposalTasks(ctx, p.ID, rows); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQueueingSomeNowLeavesTheRestForLater(t *testing.T) {
	// Queueing three of twelve and coming back next week for the rest is the
	// normal way to use a long proposal. An analysis that closed itself after
	// the first pass would throw the other nine away.
	h := newHarness(t, "true", "true")
	ctx := context.Background()
	p := readyProposalWith(t, h, []store.ProposalTask{
		{Key: "a", Goal: "First thing", Severity: "any", Selected: true},
		{Key: "b", Goal: "Second thing", Severity: "any", Selected: false},
		{Key: "c", Goal: "Third thing", Severity: "any", Selected: false},
	})

	created, err := h.eng.QueueProposal(ctx, p.ID)
	if err != nil {
		t.Fatalf("QueueProposal: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %d, want the one selected row", len(created))
	}

	got, err := h.st.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.ProposalReady {
		t.Errorf("state = %q, want it still reviewable while rows remain", got.State)
	}

	// Come back for another one.
	rows, err := h.st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows[1].Selected = true
	if err := h.st.SaveProposalTask(ctx, rows[1]); err != nil {
		t.Fatal(err)
	}
	again, err := h.eng.QueueProposal(ctx, p.ID)
	if err != nil {
		t.Fatalf("second QueueProposal: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("created = %d, want only the newly selected row", len(again))
	}
	if again[0].ID == created[0].ID {
		t.Error("the second pass re-created the task from the first")
	}

	tasks, err := h.st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Errorf("tasks = %d, want no duplicates", len(tasks))
	}
}

func TestQueueingEverythingFinishesTheAnalysis(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()
	p := readyProposalWith(t, h, []store.ProposalTask{
		{Key: "a", Goal: "Only thing", Severity: "any", Selected: true},
	})

	if _, err := h.eng.QueueProposal(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := h.st.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.ProposalQueued {
		t.Errorf("state = %q, want queued once nothing is left", got.State)
	}
	// And it is no longer offerable.
	if _, err := h.eng.QueueProposal(ctx, p.ID); err == nil {
		t.Error("a finished analysis should not be queueable again")
	}
}

func TestRequeueLinksToTheTaskQueuedEarlier(t *testing.T) {
	// A task queued today that depends on one queued last week must attach to
	// the task that already exists. Wiring only within the current call would
	// silently drop the dependency.
	h := newHarness(t, "true", "true")
	ctx := context.Background()
	p := readyProposalWith(t, h, []store.ProposalTask{
		{Key: "first", Goal: "Runs first", Severity: "any", Selected: true},
		{Key: "second", Goal: "Runs after", Severity: "any",
			DependsOn: []string{"first"}, Selected: false},
	})

	firstPass, err := h.eng.QueueProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := h.st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows[1].Selected = true
	if err := h.st.SaveProposalTask(ctx, rows[1]); err != nil {
		t.Fatal(err)
	}
	secondPass, err := h.eng.QueueProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	deps, err := h.st.TaskDeps(ctx, secondPass[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0] != firstPass[0].ID {
		t.Errorf("deps = %v, want the task queued on the earlier pass (%d)",
			deps, firstPass[0].ID)
	}
}

func TestQueueRefusesWhenNothingIsLeftToQueue(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()
	p := readyProposalWith(t, h, []store.ProposalTask{
		{Key: "a", Goal: "Already done", Severity: "any", Selected: true, CreatedTaskID: 99},
	})

	_, err := h.eng.QueueProposal(ctx, p.ID)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "already been queued") {
		t.Errorf("error = %v, want it to say the work is done", err)
	}
}

func TestRecoverParksAnAnalysisStrandedByARestart(t *testing.T) {
	// An analysis is a goroutine, not a claimable task, so nothing picks it
	// back up. Left alone the wizard shows a spinner that never resolves.
	h := newHarness(t, "true", "true")
	ctx := context.Background()
	for _, state := range []string{store.ProposalAnalysing, store.ProposalCloning} {
		if _, err := h.st.CreateProposal(ctx, store.Proposal{
			RepoPath: h.repo, State: state, MaxTasks: 12,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// One that must be left alone.
	ready, err := h.st.CreateProposal(ctx, store.Proposal{
		RepoPath: h.repo, State: store.ProposalReady, MaxTasks: 12,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := h.eng.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	all, err := h.st.AllProposals(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		switch p.ID {
		case ready.ID:
			if p.State != store.ProposalReady {
				t.Errorf("a reviewed proposal was disturbed: %q", p.State)
			}
		default:
			if p.State != store.ProposalFailed {
				t.Errorf("proposal %d = %q, want failed", p.ID, p.State)
			}
			if !strings.Contains(p.ErrMsg, "restarted") {
				t.Errorf("proposal %d err = %q, want it to explain why", p.ID, p.ErrMsg)
			}
		}
	}
}

func TestAllProposalsCountsWhatHasBeenActedOn(t *testing.T) {
	// The gap between proposed and queued is the whole reason a finished
	// analysis is worth keeping.
	h := newHarness(t, "true", "true")
	ctx := context.Background()
	p := readyProposalWith(t, h, []store.ProposalTask{
		{Key: "a", Goal: "Done", Severity: "any", Selected: true, CreatedTaskID: 7},
		{Key: "b", Goal: "Left", Severity: "any", Selected: true},
		{Key: "c", Goal: "Also left", Severity: "any", Selected: false},
	})

	all, err := h.st.AllProposals(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("proposals = %d, want 1", len(all))
	}
	if all[0].ID != p.ID || all[0].Tasks != 3 || all[0].Queued != 1 {
		t.Errorf("history = %+v, want 1 of 3 queued", all[0])
	}
	// The embedded proposal is intact, not just its counts.
	if all[0].RepoPath != h.repo {
		t.Errorf("RepoPath = %q, want the proposal's own fields carried through", all[0].RepoPath)
	}
}

func TestAllProposalsKeepsFinishedAndDiscardedOnes(t *testing.T) {
	// ListProposals hides them because they are not asking for anything; the
	// history keeps them because they are the record of what was looked at.
	h := newHarness(t, "true", "true")
	ctx := context.Background()
	for _, state := range []string{
		store.ProposalQueued, store.ProposalDiscarded, store.ProposalReady,
	} {
		if _, err := h.st.CreateProposal(ctx, store.Proposal{
			RepoPath: h.repo, State: state, MaxTasks: 12,
		}); err != nil {
			t.Fatal(err)
		}
	}

	active, err := h.st.ListProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Errorf("active = %d, want only the reviewable one", len(active))
	}
	all, err := h.st.AllProposals(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("history = %d, want all three", len(all))
	}
}

func TestARunningAgentReportsProgressWhileItRuns(t *testing.T) {
	// The wizard says "this page updates itself as the analysis runs", and the
	// page only reloads when the engine notifies. Before this, a run was
	// silent from start to finish: one notification when it began, the next
	// when it ended, and a live pane frozen in between for however many
	// minutes the turn took.
	h := newHarness(t, fakeAnalyst(t, twoTaskProposal), "true")
	ctx := context.Background()

	var mu sync.Mutex
	var duringRun int
	analysing := make(chan struct{}, 1)
	h.eng.OnChange = func(int64) {
		p, err := h.st.GetProposal(ctx, 1)
		if err != nil || p.State != store.ProposalAnalysing {
			return
		}
		mu.Lock()
		duringRun++
		mu.Unlock()
		select {
		case analysing <- struct{}{}:
		default:
		}
	}

	p, err := h.eng.StartProposal(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ConfigureProposal(ctx, p.ID, nil, "", 12, ""); err != nil {
		t.Fatal(err)
	}
	waitForProposal(t, h, p.ID, store.ProposalReady, store.ProposalFailed)

	mu.Lock()
	got := duringRun
	mu.Unlock()
	// The state change into "analysing" is one; the transcript path being
	// recorded is another. What matters is that the hook fires at all while
	// the state is analysing — a run that only notified on entry and exit
	// would leave the live pane stale for the whole turn.
	if got < 2 {
		t.Errorf("notifications while analysing = %d, want the run to report progress", got)
	}
}

func TestProgressNotifierThrottles(t *testing.T) {
	// The page reloads on every notification, so an unthrottled hook would
	// reload it once per line of agent output.
	h := newHarness(t, "true", "true")
	var n atomic.Int64
	h.eng.OnChange = func(int64) { n.Add(1) }

	notify := h.eng.progressNotifier(1)
	for i := 0; i < 500; i++ {
		notify(agent.Event{})
	}
	if got := n.Load(); got != 1 {
		t.Errorf("notifications = %d for 500 events in a burst, want 1", got)
	}
}
