package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/store"
)

// fakeArchitect replies with the given text, and echoes the prompt it was given
// into the run directory so a test can assert what actually reached the agent.
func fakeArchitect(t *testing.T, reply string) string {
	t.Helper()
	// Backslashes first, then quotes: the reply is itself JSON, so its own
	// escapes have to survive being embedded in the transcript's JSON.
	escaped := strings.ReplaceAll(reply, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	// printf, not echo: /bin/sh is dash on most Linux, whose echo expands
	// backslash escapes — so a JSON string containing \n would arrive with a
	// real newline in it and fail to parse.
	return writeScript(t, "claude", `
printf '%s\n' '{"type":"system","subtype":"init","session_id":"arch-sess"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"`+escaped+`"}]},"session_id":"arch-sess"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"arch-sess","total_cost_usd":0.30,"usage":{"input_tokens":100,"output_tokens":50}}'
`)
}

func waitForTurns(t *testing.T, h *harness, id int64, n int) []store.ArchitectTurn {
	t.Helper()
	var turns []store.ArchitectTurn
	waitFor(t, "the conversation to reach the next turn", func() bool {
		var err error
		turns, err = h.st.ArchitectTurns(context.Background(), id)
		return err == nil && len(turns) >= n
	})
	return turns
}

// The brief is the operator's first turn, so the conversation reads as one from
// the top rather than opening with a reply to something invisible.
func TestStartDesignOpensWithTheBriefAndAReply(t *testing.T) {
	h := newHarness(t, fakeArchitect(t, "Two questions before I sketch this."), "true")
	ctx := context.Background()

	p, err := h.eng.StartDesign(ctx, "", "a CLI that syncs S3 buckets, Go, no dependencies", false)
	if err != nil {
		t.Fatalf("StartDesign: %v", err)
	}
	if p.Kind != store.ProposalCreate {
		t.Errorf("Kind = %q, want create for a design with no repository", p.Kind)
	}
	if p.State != store.ProposalDesigning {
		t.Errorf("State = %q, want designing", p.State)
	}

	turns := waitForTurns(t, h, p.ID, 2)
	if turns[0].Speaker != store.SpeakerOperator || !strings.Contains(turns[0].Body, "S3") {
		t.Errorf("first turn = %s: %q, want the operator's brief", turns[0].Speaker, turns[0].Body)
	}
	if turns[1].Speaker != store.SpeakerArchitect {
		t.Errorf("second turn = %s, want the architect", turns[1].Speaker)
	}
	if !strings.Contains(turns[1].Body, "Two questions") {
		t.Errorf("architect said %q", turns[1].Body)
	}
	// A conversation costs real turns, and the wizard says so.
	if turns[1].CostUSD == 0 {
		t.Error("the architect's turn recorded no usage")
	}
}

// Each turn resumes the last rather than starting over, which is what makes it
// a conversation instead of a series of unrelated questions.
func TestSayResumesTheSession(t *testing.T) {
	h := newHarness(t, fakeArchitect(t, "Understood."), "true")
	ctx := context.Background()

	p, err := h.eng.StartDesign(ctx, "", "a thing", false)
	if err != nil {
		t.Fatal(err)
	}
	waitForTurns(t, h, p.ID, 2)

	got, err := h.st.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ArchitectSession == "" {
		t.Fatal("no session recorded; the next turn would start over")
	}

	if err := h.eng.Say(ctx, p.ID, "one-way sync, and it needs resume"); err != nil {
		t.Fatalf("Say: %v", err)
	}
	turns := waitForTurns(t, h, p.ID, 4)
	if turns[2].Speaker != store.SpeakerOperator || !strings.Contains(turns[2].Body, "one-way") {
		t.Errorf("third turn = %s: %q", turns[2].Speaker, turns[2].Body)
	}
	if turns[3].Speaker != store.SpeakerArchitect {
		t.Errorf("fourth turn = %s, want the architect", turns[3].Speaker)
	}
}

// Two turns interleaved into one session would make the transcript a record of
// something that did not happen in that order.
func TestSayRefusesWhileAReplyIsInFlight(t *testing.T) {
	h := newHarness(t, blockingClaude(t), "true")
	ctx := context.Background()

	p, err := h.eng.StartDesign(ctx, "", "a thing", false)
	if err != nil {
		t.Fatal(err)
	}
	waitForTurns(t, h, p.ID, 1)

	if err := h.eng.Say(ctx, p.ID, "and another thing"); err == nil {
		t.Error("Say was accepted while the architect was still replying")
	}
}

// A failed turn is recorded as a turn, not as a failed proposal: losing an hour
// of design to one timed-out reply would be the worst possible failure here.
func TestAFailedTurnKeepsTheConversation(t *testing.T) {
	h := newHarness(t, writeScript(t, "claude", "exit 1"), "true")
	ctx := context.Background()

	p, err := h.eng.StartDesign(ctx, "", "a thing", false)
	if err != nil {
		t.Fatal(err)
	}
	turns := waitForTurns(t, h, p.ID, 2)
	if turns[1].ErrMsg == "" {
		t.Error("the failure was not recorded on the turn")
	}

	got, err := h.st.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.ProposalDesigning {
		t.Errorf("State = %q; one bad turn ended the whole conversation", got.State)
	}
}

const designReply = `{"design":"# S3 sync\n\nOne binary. Resumable.","tasks":[` +
	`{"key":"scaffold","goal":"Scaffold the module and the CLI entry point.","constraints":["Go, no third-party dependencies"],` +
	`"verify":"go test ./...","blocking_severity":"any","cost_cap":0,"depends_on":[],"rationale":"Everything else assumes it","evidence":[]},` +
	`{"key":"walk","goal":"Walk a local tree and emit its files.","constraints":[],` +
	`"verify":"go test ./...","blocking_severity":"any","cost_cap":0,"depends_on":["scaffold"],"rationale":"The source side","evidence":[]}]}`

// Accepting ends the conversation with both artefacts, and the task list goes
// through the same validation an analysis's does.
func TestAcceptProducesTheDesignAndTheTasks(t *testing.T) {
	h := newHarness(t, fakeArchitect(t, designReply), "true")
	ctx := context.Background()

	// An existing repository, so this is a redesign and no scaffolding follows.
	p, err := h.eng.StartDesign(ctx, h.repo, "make the storage layer resumable", false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != store.ProposalAnalyse {
		t.Errorf("Kind = %q, want analyse for a design against an existing repository", p.Kind)
	}
	waitForTurns(t, h, p.ID, 2)

	if err := h.eng.Accept(ctx, p.ID); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	got := waitForProposal(t, h, p.ID, store.ProposalReady, store.ProposalFailed)
	if got.State != store.ProposalReady {
		t.Fatalf("State = %q (%s), want ready", got.State, got.ErrMsg)
	}
	if !strings.Contains(got.Design, "S3 sync") {
		t.Errorf("Design = %q, want the architect's document", got.Design)
	}

	rows, err := h.st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d tasks, want 2", len(rows))
	}
	// The ordering the design depends on survives into the rows.
	if len(rows[1].DependsOn) != 1 || rows[1].DependsOn[0] != "scaffold" {
		t.Errorf("DependsOn = %v, want [scaffold]", rows[1].DependsOn)
	}
}

// An architect that proposes tasks without saying what it decided has produced
// something nobody can review.
func TestAcceptRefusesTasksWithoutADesign(t *testing.T) {
	h := newHarness(t, fakeArchitect(t, `{"tasks":[]}`), "true")
	ctx := context.Background()

	p, err := h.eng.StartDesign(ctx, h.repo, "a change", false)
	if err != nil {
		t.Fatal(err)
	}
	waitForTurns(t, h, p.ID, 2)
	if err := h.eng.Accept(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	got := waitForProposal(t, h, p.ID, store.ProposalFailed, store.ProposalReady)
	if got.State != store.ProposalFailed {
		t.Errorf("State = %q, want failed", got.State)
	}
}

// The design conversation must not be able to leave anything behind in a
// repository the operator only asked it to think about.
func TestTheArchitectCannotWriteToAnExistingRepository(t *testing.T) {
	h := newHarness(t, "true", "true")
	spec := h.eng.analysisSandboxSpec(h.repo, t.TempDir(), "claude")
	for _, m := range spec.Mounts {
		if m.Write && strings.HasPrefix(h.repo, m.Dest) {
			t.Errorf("the architect has write access to %s", m.Dest)
		}
	}
}

// The one place a non-task turn gets a writable repository, and it must
// actually be writable or the scaffold cannot write anything.
func TestTheScaffoldSandboxIsWritable(t *testing.T) {
	h := newHarness(t, "true", "true")
	spec := h.eng.scaffoldSandboxSpec(h.repo, t.TempDir(), "claude")

	// Mounts apply in order, so the last one covering the path wins.
	writable := false
	for _, m := range spec.Mounts {
		if m.Dest == h.repo {
			writable = m.Write
		}
	}
	if !writable {
		t.Error("the scaffold sandbox has the repository read-only; it could not write the project")
	}
}

func TestCreateProjectMakesAValidRepository(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "new-thing")
	repo, err := h.eng.CreateProject(ctx, path)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if repo.Path != path {
		t.Errorf("Path = %q, want %q", repo.Path, path)
	}
	// The empty initial commit is what lets every existing worktree path work
	// untouched: without a HEAD, resolving the default branch fails and
	// `git worktree add` cannot check out an unborn ref.
	if repo.DefaultBranch == "" {
		t.Error("no default branch; a task against this would fail at setup")
	}
	if !strings.Contains(gitOut(t, path, "log", "--format=%s"), "new project") {
		t.Error("no initial commit")
	}

	// The proof that the commit was the point: a task can get a worktree.
	task, err := h.eng.Submit(ctx, BatchTask{Repo: path, Goal: "first task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.setupWorktree(ctx, &task); err != nil {
		t.Fatalf("a task against a freshly created project could not get a worktree: %v", err)
	}
}

func TestCreateProjectRefusesSomewhereAlreadyTaken(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()

	// An existing repository.
	if _, err := h.eng.CreateProject(ctx, h.repo); err == nil {
		t.Error("a new project was created over an existing repository")
	}
	// Inside one, which git would resolve to the parent.
	inside := filepath.Join(h.repo, "sub", "project")
	if _, err := h.eng.CreateProject(ctx, inside); err == nil {
		t.Error("a new project was created inside an existing repository")
	}
	// A directory with something in it.
	occupied := t.TempDir()
	if err := os.WriteFile(filepath.Join(occupied, "notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.CreateProject(ctx, occupied); err == nil {
		t.Error("a new project was created in a directory that already had something in it")
	}
	// An empty directory is fine — that is the common case.
	if _, err := h.eng.CreateProject(ctx, t.TempDir()); err != nil {
		t.Errorf("an empty directory was refused: %v", err)
	}
}

// The load-bearing assertion of the entire design.
//
// A scaffold task would leave its work on an unmerged branch, so every
// dependent task would branch from a still-empty default branch and build on
// nothing. Scaffolding outside the loop is what makes a feature task ordinary —
// and this proves it: the task's worktree contains the scaffold.
func TestAFeatureTaskBranchesFromTheScaffold(t *testing.T) {
	scaffolder := writeScript(t, "claude", `
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1"}'
mkdir -p cmd/sync
echo 'module example.test/sync' > go.mod
echo 'package main' > cmd/sync/main.go
echo '# sync' > README.md
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","total_cost_usd":0.40}'
`)
	h := newHarness(t, scaffolder, "true")
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "syncer")
	repo, err := h.eng.CreateProject(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	p, err := h.st.CreateProposal(ctx, store.Proposal{
		Kind: store.ProposalCreate, State: store.ProposalScaffolding,
		RepoID: repo.ID, RepoPath: repo.Path,
		Design: "# sync\n\nOne binary.",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.eng.scaffold(ctx, p.ID)

	got, err := h.st.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.ProposalReady {
		t.Fatalf("State = %q (%s), want ready", got.State, got.ErrMsg)
	}

	// The scaffold is on the default branch, not a side branch.
	log := gitOut(t, path, "log", "--format=%s", "main")
	if !strings.Contains(log, "overseer: scaffold") {
		t.Fatalf("the scaffold is not on main:\n%s", log)
	}
	// The design landed with it, where every later task and reviewer can read it.
	if !strings.Contains(gitOut(t, path, "show", "--name-only", "--format=", "main"), "DESIGN.md") {
		t.Error("DESIGN.md was not committed with the scaffold")
	}
	// The probe re-ran, so the wizard can now show a toolchain it could not
	// have known about when the repository was empty.
	if !strings.Contains(got.Detected, "Go") {
		t.Errorf("Detected = %q, want the toolchain the scaffold created", got.Detected)
	}

	// And now the point: a feature task's worktree has the scaffold in it.
	task, err := h.eng.Submit(ctx, BatchTask{Repo: path, Goal: "walk a local tree"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.setupWorktree(ctx, &task); err != nil {
		t.Fatalf("setupWorktree: %v", err)
	}
	for _, name := range []string{"go.mod", "cmd/sync/main.go", "DESIGN.md"} {
		if _, err := os.Stat(filepath.Join(task.WorktreeDir, name)); err != nil {
			t.Errorf("the feature task's worktree is missing %s: it would be building on nothing", name)
		}
	}
}

// A scaffold turn that writes nothing leaves no project to build on, and every
// task queued afterwards would be working in an empty directory.
func TestScaffoldFailsWhenTheAgentWroteNothing(t *testing.T) {
	h := newHarness(t, writeScript(t, "claude", `
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"s1"}'
`), "true")
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "empty-thing")
	repo, err := h.eng.CreateProject(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.st.CreateProposal(ctx, store.Proposal{
		Kind: store.ProposalCreate, State: store.ProposalScaffolding,
		RepoID: repo.ID, RepoPath: repo.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.eng.scaffold(ctx, p.ID)

	got, err := h.st.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.ProposalFailed {
		t.Errorf("State = %q, want failed", got.State)
	}
	if !strings.Contains(got.ErrMsg, "wrote nothing") {
		t.Errorf("ErrMsg = %q, want it to say the scaffold produced nothing", got.ErrMsg)
	}
}
