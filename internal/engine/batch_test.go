package engine

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/loop"
	"overseer/internal/worktree"
)

func TestParseBatch(t *testing.T) {
	raw := []byte(`
tasks:
  - repo: /home/kal/code/dc-planner
    goal: |
      Add CSV export to the rack inventory view.
    constraints:
      - Server-rendered, no new JS dependencies
  - repo: /home/kal/code/clanker
    goal: Replace the retry logic with a shared backoff helper.
    blocking_severity: major
`)
	b, err := ParseBatch(raw)
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if len(b.Tasks) != 2 {
		t.Fatalf("Tasks = %d, want 2", len(b.Tasks))
	}
	if !strings.Contains(b.Tasks[0].Goal, "CSV export") {
		t.Errorf("goal 0 = %q", b.Tasks[0].Goal)
	}
	if len(b.Tasks[0].Constraints) != 1 {
		t.Errorf("constraints 0 = %v", b.Tasks[0].Constraints)
	}
	if b.Tasks[1].BlockingSeverity != "major" {
		t.Errorf("severity 1 = %q, want major", b.Tasks[1].BlockingSeverity)
	}
	if b.Tasks[0].BlockingSeverity != "" {
		t.Errorf("severity 0 = %q, want empty so the daemon default applies", b.Tasks[0].BlockingSeverity)
	}
}

func TestParseBatchRejectsMissingFields(t *testing.T) {
	for name, raw := range map[string]string{
		"no repo":      "tasks:\n  - goal: do a thing\n",
		"no goal":      "tasks:\n  - repo: /tmp/x\n",
		"no tasks":     "tasks: []\n",
		"bad severity": "tasks:\n  - repo: /tmp/x\n    goal: g\n    blocking_severity: huge\n",
	} {
		if _, err := ParseBatch([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseBatchReportsEmptyDocumentAsNoTasks(t *testing.T) {
	// gopkg.in/yaml.v3 decodes a document with no YAML content at all to
	// io.EOF rather than a nil error with a zero-value Batch, unlike
	// "tasks: []" which decodes cleanly and is only caught by the
	// len(b.Tasks) == 0 check. ParseBatch has a guard translating that EOF
	// into the same "no tasks" error; without it this must not crash or be
	// silently accepted.
	for name, raw := range map[string]string{
		"completely empty":             "",
		"whitespace only (no content)": "   \n\n   \n",
	} {
		_, err := ParseBatch([]byte(raw))
		if err == nil {
			t.Errorf("%s: expected an error, got none", name)
			continue
		}
		if !strings.Contains(err.Error(), "no tasks") {
			t.Errorf("%s: err = %q, want it to mention \"no tasks\"", name, err)
		}
	}
}

func TestParseBatchRejectsMaxParallel(t *testing.T) {
	// max_parallel is daemon config; accepting it in a batch would let a
	// second submit change a run already in flight.
	raw := []byte("max_parallel: 9\ntasks:\n  - repo: /tmp/x\n    goal: g\n")
	if _, err := ParseBatch(raw); err == nil {
		t.Fatal("expected an error naming max_parallel")
	}
}

func TestSubmitCreatesQueuedTaskWithUniqueSlug(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	first, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "Add CSV export"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if first.State != "queued" {
		t.Errorf("State = %q, want queued", first.State)
	}
	if first.Slug != "add-csv-export" {
		t.Errorf("Slug = %q", first.Slug)
	}
	if first.BlockingSeverity != "any" {
		t.Errorf("BlockingSeverity = %q, want the daemon default", first.BlockingSeverity)
	}

	second, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "Add CSV export"})
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if second.Slug == first.Slug {
		t.Errorf("both tasks got slug %q; slugs must be unique", second.Slug)
	}
}

func TestSubmitJoinsConstraints(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task, err := h.eng.Submit(context.Background(), BatchTask{
		Repo: h.repo, Goal: "g", Constraints: []string{"one", "two"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(task.Constraints, want) {
			t.Errorf("Constraints = %q, want it to include %q", task.Constraints, want)
		}
	}
}

func TestSubmitRejectsNonRepoPath(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	if _, err := h.eng.Submit(context.Background(), BatchTask{
		Repo: t.TempDir(), Goal: "g",
	}); err == nil {
		t.Fatal("expected an error for a path that is not a git repository")
	}
}

// Abandon used to refuse a task a worker owned, because a write from the
// handler would be overwritten by that worker's next SaveTask. Refusing was
// honest but left the operator unable to stop a running task at all. The
// request is now lodged with the worker, which applies it from the copy it is
// actually holding — so the write cannot be lost, and the task really stops.
func TestAbandonReachesATaskAWorkerCurrentlyOwns(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "long running task"})
	if err != nil {
		t.Fatal(err)
	}
	_, ctrl, ok := h.eng.claim(ctx, task.ID)
	if !ok {
		t.Fatal("claim failed on a fresh task")
	}
	defer h.eng.release(task.ID, ctrl)

	if err := h.eng.Abandon(ctx, task.ID, StopOpts{}); err != nil {
		t.Fatalf("Abandon on an owned task: %v", err)
	}

	// Lodged, not written: the worker owns the row, and it applies the request
	// when it next reaches a boundary.
	req, ok := stopRequested(ctrl)
	if !ok {
		t.Fatal("no request reached the worker")
	}
	if req.Kind != StopAbandon {
		t.Errorf("Kind = %q, want abandon", req.Kind)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loop.IsTerminal(got.State) {
		t.Errorf("State = %q; the handler wrote it directly instead of letting the owner do it", got.State)
	}
}

// TestAbandonStillWorksOnAnIdleTask guards against the guard itself becoming
// too broad: a task with no worker actively driving it (the common case —
// escalated, or simply not yet claimed) must still be abandonable.
func TestAbandonStillWorksOnAnIdleTask(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "idle task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.Abandon(ctx, task.ID, StopOpts{}); err != nil {
		t.Fatalf("Abandon on an idle task: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	// abandoned, not failed: failed means something went wrong, and an
	// operator's decision reading the same way makes the board's one urgent
	// signal mean two different things.
	if got.State != string(loop.StateAbandoned) {
		t.Errorf("State = %q, want abandoned", got.State)
	}
}

func TestSubmitDerivesASubjectFromAWordyGoal(t *testing.T) {
	// A task file and the dashboard's form never see a model. The task still
	// needs a title, and the branch name still needs to be readable.
	h := newHarness(t, "true", "true")
	task, err := h.eng.Submit(context.Background(), BatchTask{
		Repo: h.repo,
		Goal: "Add a cached projection of the rack inventory query. The view " +
			"recomputes the whole join on every request, which will not hold.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Subject != "Add a cached projection of the rack inventory query" {
		t.Errorf("Subject = %q, want the goal's first sentence", task.Subject)
	}
	// The slug is NOT derived from the subject here. Nobody supplied one, so it
	// stays exactly what it has always been — see the batch test below for why
	// that matters.
	if task.Slug != worktree.Slugify(task.Goal) {
		t.Errorf("Slug = %q, want the goal-derived slug %q",
			task.Slug, worktree.Slugify(task.Goal))
	}
}

func TestABatchFileWithALongGoalKeepsTheSlugItsDependsOnNames(t *testing.T) {
	// The compatibility guarantee for every task file already written. A slug
	// is what a depends_on refers to, it has always been derived from the goal,
	// and the README says so — so a task that gives no subject must get the
	// same slug it got before subjects existed, or the dependency in somebody's
	// file stops resolving.
	h := newHarness(t, "true", "true")
	long := "Add a cached projection of the rack inventory query. The view " +
		"recomputes the whole join on every request, which will not hold."

	created, err := h.eng.SubmitBatch(context.Background(), Batch{Tasks: []BatchTask{
		{Repo: h.repo, Goal: long},
		{Repo: h.repo, Goal: "Add a column picker.",
			DependsOn: []string{worktree.Slugify(long)}},
	}})
	if err != nil {
		t.Fatalf("SubmitBatch: %v", err)
	}
	if created[0].Slug != worktree.Slugify(long) {
		t.Errorf("Slug = %q, want the goal-derived slug %q an existing file names",
			created[0].Slug, worktree.Slugify(long))
	}
	deps, err := h.st.TaskDeps(context.Background(), created[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0] != created[0].ID {
		t.Errorf("deps = %v, want [%d] — the depends_on no longer resolves",
			deps, created[0].ID)
	}
}

func TestSubmitPrefersASubjectItWasGiven(t *testing.T) {
	h := newHarness(t, "true", "true")
	task, err := h.eng.Submit(context.Background(), BatchTask{
		Repo:    h.repo,
		Subject: "Cache the inventory join",
		Goal:    "Add a cached projection of the rack inventory query. And more.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Subject != "Cache the inventory join" {
		t.Errorf("Subject = %q, want the submitted subject", task.Subject)
	}
	// A supplied subject does drive the slug: nothing can already refer to this
	// task by name, and a branch called overseer/cache-the-inventory-join beats
	// sixty characters of a paragraph.
	if task.Slug != "cache-the-inventory-join" {
		t.Errorf("Slug = %q, want the submitted subject's slug", task.Slug)
	}
}

func TestSubmitSlugsFromTheTidiedSubjectNotTheRawOne(t *testing.T) {
	// A subject that arrives needing repair — a newline and a second sentence.
	// The board shows the repaired line, so the branch has to be built from the
	// same string, or the two disagree about what this task is called.
	h := newHarness(t, "true", "true")
	task, err := h.eng.Submit(context.Background(), BatchTask{
		Repo:    h.repo,
		Subject: "Cache the inventory join.\nAlso add a test.",
		Goal:    "Add a cached projection of the rack inventory query.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Subject != "Cache the inventory join. Also add a test" {
		t.Errorf("Subject = %q, want the tidied subject", task.Subject)
	}
	if task.Slug != worktree.Slugify(task.Subject) {
		t.Errorf("Slug = %q, want %q — the slug and the subject must agree",
			task.Slug, worktree.Slugify(task.Subject))
	}
}

func TestParseBatchReadsASubject(t *testing.T) {
	b, err := ParseBatch([]byte("tasks:\n  - repo: /tmp/r\n    subject: Cache the join\n    goal: Do the long thing.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Tasks[0].Subject != "Cache the join" {
		t.Errorf("Subject = %q, want the file's subject", b.Tasks[0].Subject)
	}
}

func TestRestartWithANewGoalRederivesTheSubject(t *testing.T) {
	// "Restart it, but this time..." replaces the goal. A subject describing
	// the goal it no longer has would leave the board naming work nobody asked
	// for.
	h := newHarness(t, "true", "true")
	ctx := context.Background()
	task, err := h.eng.Submit(ctx, BatchTask{
		Repo: h.repo, Subject: "The old subject", Goal: "The old goal.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.Restart(ctx, task.ID, RestartOpts{
		Goal: "Rewrite the rack inventory projection instead. Leave the schema alone.",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "Rewrite the rack inventory projection instead" {
		t.Errorf("Subject = %q, want it re-derived from the new goal", got.Subject)
	}
}
