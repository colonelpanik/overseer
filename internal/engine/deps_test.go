package engine

import (
	"context"
	"strings"
	"testing"
)

func TestParseBatchAcceptsCapsAndDependencies(t *testing.T) {
	b, err := ParseBatch([]byte(`
tasks:
  - repo: /tmp/a
    goal: First
    cost_cap: 12.5
  - repo: /tmp/b
    goal: Second
    depends_on: [first]
`))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if b.Tasks[0].CostCap != 12.5 {
		t.Errorf("CostCap = %v, want 12.5", b.Tasks[0].CostCap)
	}
	if len(b.Tasks[1].DependsOn) != 1 || b.Tasks[1].DependsOn[0] != "first" {
		t.Errorf("DependsOn = %v, want [first]", b.Tasks[1].DependsOn)
	}
}

func TestParseBatchRejectsANegativeCap(t *testing.T) {
	_, err := ParseBatch([]byte("tasks:\n  - repo: /tmp/x\n    goal: g\n    cost_cap: -1\n"))
	if err == nil {
		t.Fatal("expected an error for a negative cost_cap")
	}
}

func TestSubmitBatchLinksADependencyBySlug(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()

	created, err := h.eng.SubmitBatch(ctx, Batch{Tasks: []BatchTask{
		{Repo: h.repo, Goal: "Enable WAL mode"},
		{Repo: h.repo, Goal: "Validate the config", DependsOn: []string{"enable-wal-mode"}},
	}})
	if err != nil {
		t.Fatalf("SubmitBatch: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("created = %d, want 2", len(created))
	}

	deps, err := h.st.TaskDeps(ctx, created[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0] != created[0].ID {
		t.Fatalf("deps = %v, want [%d]", deps, created[0].ID)
	}

	// The dependent must not be handed to a worker until the dependency is
	// done, or the ordering the task file asked for means nothing.
	claimable, err := h.st.ClaimableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimable) != 1 || claimable[0].ID != created[0].ID {
		t.Errorf("claimable = %+v, want only the dependency", claimable)
	}
}

func TestSubmitRejectsAnUnknownDependency(t *testing.T) {
	// Queueing a task whose stated precondition does not exist would leave it
	// waiting forever on nothing.
	h := newHarness(t, "true", "true")
	_, err := h.eng.Submit(context.Background(), BatchTask{
		Repo: h.repo, Goal: "Second", DependsOn: []string{"never-submitted"},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "never-submitted") {
		t.Errorf("error = %v, want it to name the missing dependency", err)
	}
	// Nothing should have been created.
	tasks, err := h.st.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("tasks = %+v, want none after a rejected submit", tasks)
	}
}

func TestSubmitFallsBackToTheDaemonTaskCap(t *testing.T) {
	h := newHarness(t, "true", "true")
	h.eng.Cfg.TaskCapUSD = 6

	withOwn, err := h.eng.Submit(context.Background(), BatchTask{
		Repo: h.repo, Goal: "Has its own cap", CostCap: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := h.eng.Submit(context.Background(), BatchTask{
		Repo: h.repo, Goal: "Takes the default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if withOwn.CostCapUSD != 20 {
		t.Errorf("explicit cap = %v, want 20", withOwn.CostCapUSD)
	}
	if inherited.CostCapUSD != 6 {
		t.Errorf("inherited cap = %v, want the daemon's 6", inherited.CostCapUSD)
	}
}

func TestRaiseCapAndSetBlockingSeverityPersist(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()
	task := h.submit(t, "A task")

	if err := h.eng.RaiseCap(ctx, task.ID, 15); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.SetBlockingSeverity(ctx, task.ID, "major"); err != nil {
		t.Fatal(err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CostCapUSD != 15 || got.BlockingSeverity != "major" {
		t.Errorf("task = %+v, want cap 15 and severity major", got)
	}

	if err := h.eng.RaiseCap(ctx, task.ID, -1); err == nil {
		t.Error("a negative cap should be rejected")
	}
	if err := h.eng.SetBlockingSeverity(ctx, task.ID, "sometimes"); err == nil {
		t.Error("an unknown severity should be rejected")
	}
}

func TestReleaseDepsUnblocksATaskWhoseDependencyFailed(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()

	dead := h.submit(t, "Will fail")
	dead.State = "failed"
	if err := h.st.SaveTask(ctx, dead); err != nil {
		t.Fatal(err)
	}
	waiter := h.submit(t, "Waits on it")
	if err := h.st.SetTaskDeps(ctx, waiter.ID, []int64{dead.ID}); err != nil {
		t.Fatal(err)
	}

	if claimable, _ := h.st.ClaimableTasks(ctx); len(claimable) != 0 {
		t.Fatalf("claimable = %+v, want none while the dependency is failed", claimable)
	}
	if err := h.eng.ReleaseDeps(ctx, waiter.ID); err != nil {
		t.Fatal(err)
	}
	claimable, err := h.st.ClaimableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimable) != 1 || claimable[0].ID != waiter.ID {
		t.Errorf("claimable = %+v, want the released task", claimable)
	}
}
