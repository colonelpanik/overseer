package store

import (
	"context"
	"errors"
	"testing"
)

func mkProposal(t *testing.T, st *Store, state string) Proposal {
	t.Helper()
	p, err := st.CreateProposal(context.Background(), Proposal{
		RepoPath: "/repo", State: state, Model: "claude-sonnet-5",
		Focus: []string{"tech debt", "test coverage"}, Notes: "leave vendor alone",
		Detected: "Go · go test ./...",
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProposalRoundTrip(t *testing.T) {
	st := depStore(t)
	ctx := context.Background()
	p := mkProposal(t, st, ProposalDraft)

	got, err := st.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Focus) != 2 || got.Focus[0] != "tech debt" {
		t.Errorf("focus = %v, want it preserved across the newline encoding", got.Focus)
	}
	if got.Notes != "leave vendor alone" || got.Detected != "Go · go test ./..." {
		t.Errorf("proposal = %+v", got)
	}
	if got.MaxTasks != 12 {
		t.Errorf("MaxTasks = %d, want the default of 12", got.MaxTasks)
	}

	got.State = ProposalReady
	got.CostUSD = 0.41
	got.Focus = nil
	if err := st.SaveProposal(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, err := st.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.State != ProposalReady || again.CostUSD != 0.41 {
		t.Errorf("proposal = %+v", again)
	}
	if len(again.Focus) != 0 {
		t.Errorf("focus = %v, want it cleared", again.Focus)
	}
}

func TestGetProposalReportsNotFound(t *testing.T) {
	st := depStore(t)
	_, err := st.GetProposal(context.Background(), 404)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListProposalsHidesFinishedOnes(t *testing.T) {
	// Queued and discarded proposals are history. Leaving them on the list
	// would mean the wizard offers to reopen work already done.
	st := depStore(t)
	mkProposal(t, st, ProposalDraft)
	mkProposal(t, st, ProposalQueued)
	mkProposal(t, st, ProposalDiscarded)
	mkProposal(t, st, ProposalFailed)

	got, err := st.ListProposals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("proposals = %d, want the draft and the failed one", len(got))
	}
	for _, p := range got {
		if p.State == ProposalQueued || p.State == ProposalDiscarded {
			t.Errorf("%s should be hidden", p.State)
		}
	}
}

func TestReplaceProposalTasksSwapsTheWholeList(t *testing.T) {
	st := depStore(t)
	ctx := context.Background()
	p := mkProposal(t, st, ProposalReady)

	first := []ProposalTask{
		{Key: "a", Goal: "First", Constraints: []string{"x", "y"},
			Verify: "go test ./...", Severity: "any", CostCap: 8, Selected: true,
			Evidence: []string{"a.go:1", "b.go:2"}},
		{Key: "b", Goal: "Second", Severity: "major",
			DependsOn: []string{"a"}, Selected: true},
	}
	if err := st.ReplaceProposalTasks(ctx, p.ID, first); err != nil {
		t.Fatal(err)
	}
	got, err := st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("tasks = %d, want 2", len(got))
	}
	if got[0].Ord != 0 || got[1].Ord != 1 {
		t.Errorf("ordering = %d, %d", got[0].Ord, got[1].Ord)
	}
	if len(got[0].Constraints) != 2 || len(got[0].Evidence) != 2 {
		t.Errorf("list fields lost their items: %+v", got[0])
	}
	if len(got[1].DependsOn) != 1 || got[1].DependsOn[0] != "a" {
		t.Errorf("depends_on = %v", got[1].DependsOn)
	}

	// A regenerate replaces rather than appends, or the operator would review
	// a mixture of two different analyses.
	if err := st.ReplaceProposalTasks(ctx, p.ID, []ProposalTask{
		{Key: "c", Goal: "Only one now", Severity: "any", Selected: true},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "c" {
		t.Errorf("tasks = %+v, want only the new list", got)
	}
}

func TestSaveProposalTaskEditsTheFieldsTheReviewStepOffers(t *testing.T) {
	st := depStore(t)
	ctx := context.Background()
	p := mkProposal(t, st, ProposalReady)
	if err := st.ReplaceProposalTasks(ctx, p.ID, []ProposalTask{
		{Key: "a", Goal: "Before", Severity: "any", Selected: true},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	row := rows[0]
	row.Goal = "After"
	row.Verify = "make test"
	row.Severity = "major"
	row.CostCap = 15
	row.Selected = false
	if err := st.SaveProposalTask(ctx, row); err != nil {
		t.Fatal(err)
	}

	rows, err = st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := rows[0]
	if got.Goal != "After" || got.Verify != "make test" || got.Severity != "major" ||
		got.CostCap != 15 || got.Selected {
		t.Errorf("task = %+v", got)
	}
	// The key is what dependencies resolve through, so the review step does
	// not get to rewrite it.
	if got.Key != "a" {
		t.Errorf("key = %q, want it untouched", got.Key)
	}
}

func TestGetProposalTaskRefusesARowFromAnotherProposal(t *testing.T) {
	// This is the authorisation check behind the edit route: a hand-edited URL
	// must not reach a row belonging to a different analysis.
	st := depStore(t)
	ctx := context.Background()
	mine := mkProposal(t, st, ProposalReady)
	theirs := mkProposal(t, st, ProposalReady)

	if err := st.ReplaceProposalTasks(ctx, theirs.ID, []ProposalTask{
		{Key: "a", Goal: "Theirs", Severity: "any", Selected: true},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ProposalTasks(ctx, theirs.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.GetProposalTask(ctx, mine.ID, rows[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for a row of another proposal", err)
	}
	if _, err := st.GetProposalTask(ctx, theirs.ID, rows[0].ID); err != nil {
		t.Errorf("the owning proposal should reach its own row: %v", err)
	}
}

func TestDeletingAProposalTakesItsTasksWithIt(t *testing.T) {
	st := depStore(t)
	ctx := context.Background()
	p := mkProposal(t, st, ProposalReady)
	if err := st.ReplaceProposalTasks(ctx, p.ID, []ProposalTask{
		{Key: "a", Goal: "g", Severity: "any", Selected: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM proposals WHERE id = ?`, p.ID); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proposal_tasks WHERE proposal_id = ?`, p.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("orphaned rows = %d, want the cascade to have removed them", n)
	}
}

func TestProposalSpendTotalsEveryAnalysis(t *testing.T) {
	// The dashboard's run spend includes this, so an analysis that cost money
	// is not invisible.
	st := depStore(t)
	ctx := context.Background()
	if got, err := st.ProposalSpend(ctx); err != nil || got != 0 {
		t.Fatalf("spend = %v, %v; want 0 with no proposals", got, err)
	}

	for _, cost := range []float64{0.25, 0.41} {
		p := mkProposal(t, st, ProposalReady)
		p.CostUSD = cost
		if err := st.SaveProposal(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ProposalSpend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got < 0.65 || got > 0.67 {
		t.Errorf("spend = %v, want about 0.66", got)
	}
}
