package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/loop"
	"overseer/internal/store"
)

func TestEnsureRepoRegistersOnceAndProbes(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	first, err := h.eng.EnsureRepo(ctx, h.repo)
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	second, err := h.eng.EnsureRepo(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("EnsureRepo made two repos for one path: %d then %d", first.ID, second.ID)
	}
	if first.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", first.DefaultBranch)
	}
	if first.OriginURL == "" {
		t.Error("OriginURL not recorded, though the fixture has an origin")
	}
	if first.Slug == "" {
		t.Error("no slug assigned")
	}
}

func TestEnsureRepoRefusesADirectoryThatIsNotARepository(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	if _, err := h.eng.EnsureRepo(context.Background(), t.TempDir()); err == nil {
		t.Fatal("want an error for a directory that is not a git repository")
	}
}

// Registration is a side effect of use, so an existing task file that names a
// path keeps working and the repo list fills itself in.
func TestSubmitRegistersItsRepository(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "Add a thing"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if task.RepoID == 0 {
		t.Fatal("Submit did not link the task to a repository")
	}
	repo, err := h.st.GetRepo(ctx, task.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Path != task.RepoPath {
		t.Errorf("task repo path %q does not match repo %q", task.RepoPath, repo.Path)
	}
}

// The point of registering: the path is typed once, and thereafter the slug
// names it.
func TestSubmitAcceptsARegisteredSlug(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	repo, err := h.eng.EnsureRepo(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	task, err := h.eng.Submit(ctx, BatchTask{Repo: repo.Slug, Goal: "By slug"})
	if err != nil {
		t.Fatalf("Submit by slug: %v", err)
	}
	if task.RepoID != repo.ID {
		t.Errorf("RepoID = %d, want %d", task.RepoID, repo.ID)
	}
	if task.RepoPath != repo.Path {
		t.Errorf("RepoPath = %q, want %q", task.RepoPath, repo.Path)
	}
}

func TestSubmitResolvesSettingsTaskThenRepoThenDaemon(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	h.eng.Cfg.VerifyCommand = "daemon-verify"
	h.eng.Cfg.BlockingSeverity = "critical"
	h.eng.Cfg.TaskCapUSD = 1

	repo, err := h.eng.EnsureRepo(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.SetRepoDefaults(ctx, repo.ID, "repo-verify", "major", 2); err != nil {
		t.Fatalf("SetRepoDefaults: %v", err)
	}

	// Nothing stated: the repository's defaults win over the daemon's.
	inherited, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "Inherits"})
	if err != nil {
		t.Fatal(err)
	}
	if inherited.VerifyCommand != "repo-verify" {
		t.Errorf("VerifyCommand = %q, want repo-verify", inherited.VerifyCommand)
	}
	if inherited.BlockingSeverity != "major" {
		t.Errorf("BlockingSeverity = %q, want major", inherited.BlockingSeverity)
	}
	if inherited.CostCapUSD != 2 {
		t.Errorf("CostCapUSD = %v, want 2", inherited.CostCapUSD)
	}

	// Stated on the task: the task wins over both.
	stated, err := h.eng.Submit(ctx, BatchTask{
		Repo: h.repo, Goal: "States its own",
		Verify: "task-verify", BlockingSeverity: "minor", CostCap: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stated.VerifyCommand != "task-verify" || stated.BlockingSeverity != "minor" || stated.CostCapUSD != 3 {
		t.Errorf("task settings did not win: %+v", stated)
	}

	// With the repository configuring nothing, the daemon default comes back.
	if err := h.eng.SetRepoDefaults(ctx, repo.ID, "", "", 0); err != nil {
		t.Fatal(err)
	}
	fellThrough, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "Falls through"})
	if err != nil {
		t.Fatal(err)
	}
	if fellThrough.VerifyCommand != "daemon-verify" {
		t.Errorf("VerifyCommand = %q, want the daemon default back", fellThrough.VerifyCommand)
	}
	if fellThrough.BlockingSeverity != "critical" || fellThrough.CostCapUSD != 1 {
		t.Errorf("daemon defaults not restored: %+v", fellThrough)
	}
}

// A review's non-blocking findings are exactly the ones the loop chose not to
// act on. Before the backlog they were shown once and thrown away.
func TestNonBlockingFindingsLandOnTheBacklogAndBlockingOnesDoNot(t *testing.T) {
	h := newHarness(t,
		fakeClaude(t, ""),
		fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "A task"})
	if err != nil {
		t.Fatal(err)
	}
	step, err := h.st.StartStep(ctx, store.Step{TaskID: task.ID, Phase: "exec", Agent: "review"})
	if err != nil {
		t.Fatal(err)
	}

	findings := []store.Finding{
		{Severity: "nit", File: "internal/x.go", Line: 12, Summary: "prefer errors.Is here", Blocking: false},
		{Severity: "major", File: "internal/y.go", Line: 3, Summary: "nil deref on the error path", Blocking: true},
	}
	if err := h.st.FinishStep(ctx, step, findings); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.recordBacklogFindings(ctx, &task, findings); err != nil {
		t.Fatalf("recordBacklogFindings: %v", err)
	}

	items, err := h.st.ListBacklog(ctx, task.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("backlog has %d items, want only the non-blocking one: %+v", len(items), items)
	}
	got := items[0]
	if got.Title != "prefer errors.Is here" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Source != store.BacklogReview {
		t.Errorf("Source = %q, want review", got.Source)
	}
	if got.OriginTaskID != task.ID {
		t.Errorf("OriginTaskID = %d, want %d", got.OriginTaskID, task.ID)
	}
	if len(got.Evidence) != 1 || got.Evidence[0] != "internal/x.go:12" {
		t.Errorf("Evidence = %v, want the citation", got.Evidence)
	}
}

// The same nit raised on three tasks is one item seen three times, which is a
// stronger signal than three identical rows.
func TestARepeatedNitBecomesOneItemWithACount(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	var repoID int64
	for i, goal := range []string{"first", "second", "third"} {
		task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: goal})
		if err != nil {
			t.Fatal(err)
		}
		repoID = task.RepoID
		err = h.eng.recordBacklogFindings(ctx, &task, []store.Finding{
			{Severity: "nit", File: "internal/x.go", Line: 10 + i, Summary: "prefer errors.Is here"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	items, err := h.st.ListBacklog(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("backlog has %d items, want 1", len(items))
	}
	if items[0].Seen != 3 {
		t.Errorf("Seen = %d, want 3", items[0].Seen)
	}
}

func TestPromoteBacklogItemCarriesEvidenceAndRepoDefaults(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	repo, err := h.eng.EnsureRepo(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.SetRepoDefaults(ctx, repo.ID, "repo-verify", "major", 5); err != nil {
		t.Fatal(err)
	}

	item, err := h.st.AddBacklogItem(ctx, store.BacklogItem{
		RepoID:       repo.ID,
		Source:       store.BacklogReview,
		Title:        "the outbound HTTP client has no timeout",
		Detail:       "A hung upstream would hang the worker.",
		Evidence:     []string{"internal/fetch/client.go:18"},
		Severity:     "major",
		OriginTaskID: 7,
	})
	if err != nil {
		t.Fatal(err)
	}

	task, err := h.eng.PromoteBacklogItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("PromoteBacklogItem: %v", err)
	}
	if task.Goal != item.Title {
		t.Errorf("Goal = %q, want the item's title", task.Goal)
	}
	if task.VerifyCommand != "repo-verify" || task.BlockingSeverity != "major" || task.CostCapUSD != 5 {
		t.Errorf("promoted task did not inherit the repo's defaults: %+v", task)
	}
	if !strings.Contains(task.Constraints, "internal/fetch/client.go:18") {
		t.Errorf("constraints lost the evidence:\n%s", task.Constraints)
	}
	if !strings.Contains(task.Constraints, "task 7") {
		t.Errorf("constraints lost the provenance:\n%s", task.Constraints)
	}

	back, err := h.st.GetBacklogItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.State != store.BacklogQueued || back.CreatedTaskID != task.ID {
		t.Errorf("item not marked queued: %+v", back)
	}

	// Promoting twice would queue the same work again.
	if _, err := h.eng.PromoteBacklogItem(ctx, item.ID); err == nil {
		t.Error("want an error promoting an item that already became a task")
	}
}

func TestDismissAndReopenABacklogItem(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	repo, err := h.eng.EnsureRepo(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	item, err := h.eng.AddBacklogItem(ctx, repo.ID, "tidy the config loader", "", "nit")
	if err != nil {
		t.Fatalf("AddBacklogItem: %v", err)
	}
	if item.Source != store.BacklogManual {
		t.Errorf("Source = %q, want manual", item.Source)
	}

	if err := h.eng.DismissBacklogItem(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	got, err := h.st.GetBacklogItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.BacklogDismissed {
		t.Errorf("State = %q, want dismissed", got.State)
	}

	if err := h.eng.ReopenBacklogItem(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	got, err = h.st.GetBacklogItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.BacklogOpen {
		t.Errorf("State = %q, want open", got.State)
	}
}

// An item that already became a task must not be dismissed: the work is in
// flight, and marking it "not worth doing" would say the opposite of the truth.
func TestDismissRefusesAnItemAlreadyQueued(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	repo, err := h.eng.EnsureRepo(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	item, err := h.eng.AddBacklogItem(ctx, repo.ID, "something", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.PromoteBacklogItem(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.DismissBacklogItem(ctx, item.ID); err == nil {
		t.Error("want an error dismissing an item that is already a task")
	}
}

func TestAddBacklogItemValidates(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()
	repo, err := h.eng.EnsureRepo(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.eng.AddBacklogItem(ctx, repo.ID, "  ", "", ""); err == nil {
		t.Error("want an error for an item with no title")
	}
	if _, err := h.eng.AddBacklogItem(ctx, repo.ID, "x", "", "catastrophic"); err == nil {
		t.Error("want an error for a severity that is not one of the known ones")
	}
	if _, err := h.eng.AddBacklogItem(ctx, 999, "x", "", ""); err == nil {
		t.Error("want an error for a repository that does not exist")
	}
}

func TestSetRepoDefaultsValidates(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()
	repo, err := h.eng.EnsureRepo(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.SetRepoDefaults(ctx, repo.ID, "", "", -1); err == nil {
		t.Error("want an error for a negative cap")
	}
	if err := h.eng.SetRepoDefaults(ctx, repo.ID, "", "catastrophic", 0); err == nil {
		t.Error("want an error for an unknown severity")
	}
}

func TestArchiveAndUnarchiveARepo(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()
	repo, err := h.eng.EnsureRepo(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ArchiveRepo(ctx, repo.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err := h.st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Archived() {
		t.Fatal("repo not archived")
	}
	if err := h.eng.ArchiveRepo(ctx, repo.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err = h.st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Archived() {
		t.Error("repo still archived after unarchiving")
	}
}

// The step's provider is what makes the reported/metered split possible after
// the fact, so it has to be recorded when the step starts.
func TestStepsRecordTheProviderThatServedThem(t *testing.T) {
	h := newHarness(t,
		fakeClaude(t, `echo 'package main' > added.go`),
		fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "Add a thing"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 {
		t.Fatal("no steps recorded")
	}
	for _, s := range steps {
		if s.Provider == "" {
			t.Errorf("step %d (%s %s) recorded no provider", s.ID, s.Phase, s.Agent)
		}
	}
}

// Accounting leads with agent time and turns because those are always true,
// and keeps subscription-covered usage apart from real metered money.
func TestRepoStatsAddUpAcrossARealRun(t *testing.T) {
	h := newHarness(t,
		fakeClaude(t, `echo 'package main' > added.go`),
		fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "Add a thing"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	stats, err := h.eng.RepoStats(ctx)
	if err != nil {
		t.Fatalf("RepoStats: %v", err)
	}
	got := stats[task.RepoID]
	if got.Tasks != 1 || got.Done != 1 {
		t.Errorf("counts = %+v, want 1 task, 1 done", got)
	}
	if got.Turns < 4 {
		t.Errorf("Turns = %d, want at least the four steps of a converged run", got.Turns)
	}
	// The default providers are the CLIs' own logins, so nothing here is
	// metered — the whole point of the split.
	if got.Metered != 0 {
		t.Errorf("Metered = %v, want 0 for subscription-covered CLI runs", got.Metered)
	}
	if got.Reported <= 0 {
		t.Errorf("Reported = %v, want the fake agents' reported usage", got.Reported)
	}
}

// git resolves a repository by walking up, so `rev-parse` succeeds from any
// subdirectory. Registering one as its own repository means every later git
// command silently operates on the enclosing one — a worktree cut for a task
// under /repo/internal/web would put a branch and a full checkout in /repo.
func TestEnsureRepoRefusesASubdirectoryOfARepository(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	sub := filepath.Join(h.repo, "internal", "web")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := h.eng.EnsureRepo(ctx, sub)
	if err == nil {
		t.Fatal("a subdirectory was registered as its own repository")
	}
	if !strings.Contains(err.Error(), "repository root") {
		t.Errorf("err = %v, want it to point at the repository root", err)
	}

	// The root itself still works, and is what the subdirectory pointed at.
	if _, err := h.eng.EnsureRepo(ctx, h.repo); err != nil {
		t.Fatalf("the repository root was refused: %v", err)
	}
}

// A converged task with nowhere to push is done, not failed. It used to run the
// whole loop, converge, commit, and only then be marked failed — having spent
// its entire budget on work it then reported as lost.
func TestATaskWithNoRemoteFinishesRatherThanFailing(t *testing.T) {
	h := newHarness(t,
		fakeClaude(t, `echo 'package main' > added.go`),
		fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	// A repository with no origin at all.
	repo := t.TempDir()
	run(t, repo, "git", "init", "--initial-branch=main", ".")
	run(t, repo, "git", "config", "user.email", "t@example.com")
	run(t, repo, "git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "initial")

	task, err := h.eng.Submit(ctx, BatchTask{Repo: repo, Goal: "no remote here"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q (err %q), want done", got.State, got.ErrMsg)
	}
	if got.PRURL != "" {
		t.Errorf("PRURL = %q; there was no remote to open one against", got.PRURL)
	}
	if len(h.pr.Calls) != 0 {
		t.Errorf("a pull request was attempted with no remote: %+v", h.pr.Calls)
	}
	// Not an error: the task succeeded. The board says "no remote" by reading
	// done-with-no-PR, not by finding a message here.
	if got.ErrMsg != "" {
		t.Errorf("ErrMsg = %q; a successful task must not carry an error", got.ErrMsg)
	}
	// The work is on the branch, which is the durable record when there is no
	// pull request to carry it.
	if !strings.Contains(gitOut(t, repo, "log", "--format=%s", got.Branch), "overseer:") {
		t.Error("the branch carries no overseer commit")
	}
	if !strings.Contains(gitOut(t, repo, "show", "--name-only", "--format=", got.Branch), "added.go") {
		t.Error("the agent's file is not on the branch")
	}
}
