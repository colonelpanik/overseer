package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"overseer/internal/store"
)

// fakeAnalyst emits a stream-json run whose final assistant message is the
// given proposal JSON, which is where ParseProposal reads it from.
func fakeAnalyst(t *testing.T, proposalJSON string) string {
	t.Helper()
	return writeScript(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"analysis-sess"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"`+
		strings.ReplaceAll(proposalJSON, `"`, `\"`)+`"}]},"session_id":"analysis-sess"}'
echo '{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.25,"usage":{"input_tokens":900,"output_tokens":300}}'
`)
}

const twoTaskProposal = `{"tasks":[` +
	`{"key":"wal","goal":"Enable WAL mode on the store connection.","constraints":["Follow repo_*.go"],` +
	`"verify":"go test ./...","blocking_severity":"any","cost_cap":8,"depends_on":[],` +
	`"rationale":"store.go opens without a busy timeout","evidence":["internal/store/store.go:33"]},` +
	`{"key":"schema","goal":"Validate config.yaml at startup.","constraints":[],` +
	`"verify":"go test ./...","blocking_severity":"major","cost_cap":0,"depends_on":["wal"],` +
	`"rationale":"config is unmarshalled unchecked","evidence":["internal/config/config.go:97"]}]}`

// waitForProposal polls until the proposal reaches one of want, which is how
// the analysis reports back: it runs in its own goroutine so the wizard's POST
// can return immediately.
func waitForProposal(t *testing.T, h *harness, id int64, want ...string) store.Proposal {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		p, err := h.st.GetProposal(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if p.State == w {
				return p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	p, _ := h.st.GetProposal(context.Background(), id)
	t.Fatalf("proposal stayed %s (%s), want one of %v", p.State, p.ErrMsg, want)
	return store.Proposal{}
}

func TestStartProposalProbesTheRepository(t *testing.T) {
	h := newHarness(t, "true", "true")
	// A go.mod makes the probe report the toolchain and its test command, so
	// the operator can see the wizard understood the repo before paying.
	if err := os.WriteFile(filepath.Join(h.repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := h.eng.StartProposal(context.Background(), h.repo)
	if err != nil {
		t.Fatalf("StartProposal: %v", err)
	}
	if p.State != store.ProposalDraft {
		t.Errorf("state = %q, want draft", p.State)
	}
	if !strings.Contains(p.Detected, "Go") || !strings.Contains(p.Detected, "go test ./...") {
		t.Errorf("detected = %q, want the Go toolchain and its test command", p.Detected)
	}
	if p.Model != h.eng.Cfg.AnalysisModel {
		t.Errorf("model = %q, want the configured analysis model", p.Model)
	}
}

func TestStartProposalRejectsSomethingThatIsNotARepository(t *testing.T) {
	h := newHarness(t, "true", "true")
	if _, err := h.eng.StartProposal(context.Background(), t.TempDir()); err == nil {
		t.Error("a directory that is not a git repository should be refused")
	}
	if _, err := h.eng.StartProposal(context.Background(), "  "); err == nil {
		t.Error("an empty path should be refused")
	}
}

func TestAnalyseStoresTheProposedTasks(t *testing.T) {
	h := newHarness(t, fakeAnalyst(t, twoTaskProposal), "true")
	ctx := context.Background()

	p, err := h.eng.StartProposal(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ConfigureProposal(ctx, p.ID, []string{"tech debt"}, "no vendored code", 12, ""); err != nil {
		t.Fatal(err)
	}
	done := waitForProposal(t, h, p.ID, store.ProposalReady, store.ProposalFailed)
	if done.State != store.ProposalReady {
		t.Fatalf("state = %s: %s", done.State, done.ErrMsg)
	}
	if done.CostUSD != 0.25 {
		t.Errorf("cost = %v, want the run's cost recorded on the proposal", done.CostUSD)
	}
	if done.TranscriptPath == "" {
		t.Error("the transcript path should be recorded so the live pane can read it")
	}

	rows, err := h.st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Key != "wal" || rows[0].Verify != "go test ./..." || rows[0].CostCap != 8 {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if len(rows[0].Constraints) != 1 {
		t.Errorf("constraints = %v, want the one the analysis gave", rows[0].Constraints)
	}
	if rows[1].Severity != "major" || len(rows[1].DependsOn) != 1 || rows[1].DependsOn[0] != "wal" {
		t.Errorf("row 1 = %+v", rows[1])
	}
	// Everything starts selected: deselecting three of twelve is less work
	// than picking nine.
	for _, r := range rows {
		if !r.Selected {
			t.Errorf("row %q should start selected", r.Key)
		}
	}
}

func TestAnalyseFailsLoudlyOnAnUnusableResponse(t *testing.T) {
	// Two attempts, then failure. A partially understood list is worse than
	// none, because the operator would review a list that silently lost items.
	h := newHarness(t, fakeAnalyst(t, `{"tasks":`), "true")
	ctx := context.Background()

	p, err := h.eng.StartProposal(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ConfigureProposal(ctx, p.ID, nil, "", 12, ""); err != nil {
		t.Fatal(err)
	}
	done := waitForProposal(t, h, p.ID, store.ProposalFailed, store.ProposalReady)
	if done.State != store.ProposalFailed {
		t.Fatalf("state = %s, want failed", done.State)
	}
	if !strings.Contains(done.ErrMsg, "parse proposal") {
		t.Errorf("err = %q, want the parse failure explained", done.ErrMsg)
	}
	// Both attempts were paid for, so both must be counted.
	if done.CostUSD != 0.50 {
		t.Errorf("cost = %v, want both attempts counted", done.CostUSD)
	}
}

func TestConfigureProposalRejectsAnAbsurdBudget(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()
	p, err := h.eng.StartProposal(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, -1, 41} {
		if err := h.eng.ConfigureProposal(ctx, p.ID, nil, "", n, ""); err == nil {
			t.Errorf("max tasks %d should be refused", n)
		}
	}
}

func TestQueueProposalCreatesTasksAndWiresDependenciesByID(t *testing.T) {
	h := newHarness(t, fakeAnalyst(t, twoTaskProposal), "true")
	ctx := context.Background()

	p, err := h.eng.StartProposal(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ConfigureProposal(ctx, p.ID, nil, "", 12, ""); err != nil {
		t.Fatal(err)
	}
	waitForProposal(t, h, p.ID, store.ProposalReady)

	created, err := h.eng.QueueProposal(ctx, p.ID)
	if err != nil {
		t.Fatalf("QueueProposal: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("created = %d, want 2", len(created))
	}
	if created[0].VerifyCommand != "go test ./..." || created[0].CostCapUSD != 8 {
		t.Errorf("task 0 = %+v", created[0])
	}
	if created[1].BlockingSeverity != "major" {
		t.Errorf("task 1 severity = %q, want major", created[1].BlockingSeverity)
	}
	if created[0].Constraints == "" {
		t.Error("constraints should have reached the task")
	}

	deps, err := h.st.TaskDeps(ctx, created[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0] != created[0].ID {
		t.Errorf("deps = %v, want [%d]", deps, created[0].ID)
	}

	done, err := h.st.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != store.ProposalQueued {
		t.Errorf("state = %q, want queued", done.State)
	}
}

func TestQueueProposalResolvesDependenciesEvenWhenTheSlugCollides(t *testing.T) {
	// Submit suffixes a slug on collision, so the slug a proposal predicted is
	// not necessarily the slug the task gets. Wiring by name would attach the
	// dependency to the pre-existing task instead of the new one.
	h := newHarness(t, fakeAnalyst(t, twoTaskProposal), "true")
	ctx := context.Background()

	// Queue a task whose goal produces the same slug the first proposed task
	// will want.
	first, err := h.eng.Submit(ctx, BatchTask{
		Repo: h.repo, Goal: "Enable WAL mode on the store connection.",
	})
	if err != nil {
		t.Fatal(err)
	}

	p, err := h.eng.StartProposal(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ConfigureProposal(ctx, p.ID, nil, "", 12, ""); err != nil {
		t.Fatal(err)
	}
	waitForProposal(t, h, p.ID, store.ProposalReady)

	created, err := h.eng.QueueProposal(ctx, p.ID)
	if err != nil {
		t.Fatalf("QueueProposal: %v", err)
	}
	if created[0].Slug == first.Slug {
		t.Fatalf("the new task reused the existing slug %q", first.Slug)
	}
	deps, err := h.st.TaskDeps(ctx, created[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0] != created[0].ID {
		t.Errorf("deps = %v, want the newly created task %d, not the pre-existing %d",
			deps, created[0].ID, first.ID)
	}
}

func TestQueueProposalDropsADependencyOnADeselectedTask(t *testing.T) {
	// The operator said they did not want that task. Refusing the whole batch
	// over it would be the tool arguing with them.
	h := newHarness(t, fakeAnalyst(t, twoTaskProposal), "true")
	ctx := context.Background()

	p, err := h.eng.StartProposal(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ConfigureProposal(ctx, p.ID, nil, "", 12, ""); err != nil {
		t.Fatal(err)
	}
	waitForProposal(t, h, p.ID, store.ProposalReady)

	rows, err := h.st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows[0].Selected = false
	if err := h.st.SaveProposalTask(ctx, rows[0]); err != nil {
		t.Fatal(err)
	}

	created, err := h.eng.QueueProposal(ctx, p.ID)
	if err != nil {
		t.Fatalf("QueueProposal: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %d, want only the selected task", len(created))
	}
	deps, err := h.st.TaskDeps(ctx, created[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("deps = %v, want none: the task it depended on was dropped", deps)
	}
}

func TestQueueProposalRefusesWhenNothingIsSelectedOrItIsNotReady(t *testing.T) {
	h := newHarness(t, fakeAnalyst(t, twoTaskProposal), "true")
	ctx := context.Background()

	p, err := h.eng.StartProposal(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.QueueProposal(ctx, p.ID); err == nil {
		t.Error("a draft proposal should not be queueable")
	}

	if err := h.eng.ConfigureProposal(ctx, p.ID, nil, "", 12, ""); err != nil {
		t.Fatal(err)
	}
	waitForProposal(t, h, p.ID, store.ProposalReady)

	rows, err := h.st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		r.Selected = false
		if err := h.st.SaveProposalTask(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.eng.QueueProposal(ctx, p.ID); err == nil {
		t.Error("queueing nothing should be refused")
	}
}

func TestRegenerateReplacesTheListAndAccumulatesSpend(t *testing.T) {
	h := newHarness(t, fakeAnalyst(t, twoTaskProposal), "true")
	ctx := context.Background()

	p, err := h.eng.StartProposal(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ConfigureProposal(ctx, p.ID, nil, "", 12, ""); err != nil {
		t.Fatal(err)
	}
	waitForProposal(t, h, p.ID, store.ProposalReady)

	if err := h.eng.RegenerateProposal(ctx, p.ID, "fewer docs tasks"); err != nil {
		t.Fatal(err)
	}
	// The state has to leave ready and come back, or the wizard would show a
	// stale list while the new analysis runs.
	done := waitForProposal(t, h, p.ID, store.ProposalReady)
	if done.CostUSD <= 0.25 {
		t.Errorf("cost = %v, want both runs counted", done.CostUSD)
	}
	rows, err := h.st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("rows = %d, want the list replaced rather than appended", len(rows))
	}
}

func TestImportProposalRefusesATransportItWillNotFetch(t *testing.T) {
	h := newHarness(t, "true", "true")
	if _, err := h.eng.ImportProposal(context.Background(), "ext::sh -c id"); err == nil {
		t.Fatal("expected an error")
	}
	// Nothing should have been recorded: a proposal naming a URL overseer
	// would never fetch is a row that can only ever fail.
	ps, err := h.st.ListProposals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Errorf("proposals = %+v, want none", ps)
	}
}

func TestDiscardProposalTakesItOffTheList(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()
	p, err := h.eng.StartProposal(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.DiscardProposal(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	ps, err := h.st.ListProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Errorf("proposals = %+v, want the discarded one hidden", ps)
	}
}

func TestAnalysisSandboxMountsTheRepositoryReadOnly(t *testing.T) {
	// The whole promise of the wizard is that it reads. A writable mount would
	// let an analysis leave a branch, a stash or an edit behind in a
	// repository the operator only asked it to look at.
	h := newHarness(t, "true", "true")
	runDir := filepath.Join(h.eng.Cfg.ProposalsDir(), "1")
	spec := h.eng.analysisSandboxSpec(h.repo, runDir, "claude")

	var sawRepo, sawRun bool
	for _, m := range spec.Mounts {
		if m.Src == h.repo {
			sawRepo = true
			if m.Write {
				t.Error("the repository is mounted writable")
			}
		}
		if m.Src == runDir {
			sawRun = true
			if !m.Write {
				t.Error("the run directory must be writable for the transcript")
			}
		}
		// No path inside the repository may be writable either, which is how a
		// narrower mount could reopen the hole.
		if m.Write && strings.HasPrefix(m.Src, h.repo+string(filepath.Separator)) {
			t.Errorf("%s is writable and inside the repository", m.Src)
		}
	}
	if !sawRepo {
		t.Error("the repository is not mounted at all")
	}
	if !sawRun {
		t.Error("the run directory is not mounted")
	}
	if spec.WorkDir != h.repo {
		t.Errorf("WorkDir = %q, want the repository", spec.WorkDir)
	}
}

func TestAnalysisSandboxLayersTheRealAgentConfigReadOnly(t *testing.T) {
	// The analysis uses the same inverted ~/.claude layout as a task: a
	// per-run writable directory with the real config mounted read-only on
	// top, so nothing absent can be planted and run unsandboxed later.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	h := newHarness(t, "true", "true")
	runDir := filepath.Join(h.eng.Cfg.ProposalsDir(), "1")
	spec := h.eng.analysisSandboxSpec(h.repo, runDir, "claude")

	realClaude := filepath.Join(home, ".claude")
	for _, m := range spec.Mounts {
		if m.Src == realClaude || strings.HasPrefix(m.Src, realClaude+string(filepath.Separator)) {
			if m.Write {
				t.Errorf("%s is mounted writable", m.Src)
			}
		}
	}
	var standIn bool
	for _, m := range spec.Mounts {
		if m.Dest == realClaude && m.Write && strings.HasPrefix(m.Src, runDir) {
			standIn = true
		}
	}
	if !standIn {
		t.Error("the per-run state directory is not standing in for ~/.claude")
	}
}
