package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"overseer/internal/agent"
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

// designReply with a subject on the first task and none on the second, so one
// accept covers both the supplied and the derived path.
const subjectDesignReply = `{"design":"# S3 sync\n\nOne binary. Resumable.","tasks":[` +
	`{"key":"scaffold","subject":"Scaffold the module",` +
	`"goal":"Scaffold the module and the CLI entry point. Everything else assumes it is there, so it comes first.",` +
	`"constraints":[],"verify":"go test ./...","blocking_severity":"any","cost_cap":0,` +
	`"depends_on":[],"rationale":"Everything else assumes it","evidence":[]},` +
	`{"key":"walk","goal":"Walk a local tree and emit its files.","constraints":[],` +
	`"verify":"go test ./...","blocking_severity":"any","cost_cap":0,` +
	`"depends_on":["scaffold"],"rationale":"The source side","evidence":[]}]}`

func TestAcceptCarriesTheArchitectsSubjectOntoTheRowsAndTheTask(t *testing.T) {
	h := newHarness(t, fakeArchitect(t, subjectDesignReply), "true")
	ctx := context.Background()

	// An existing repository, so this is a redesign: it reaches ready without
	// scaffolding, and its tasks can be queued straight away.
	p, err := h.eng.StartDesign(ctx, h.repo, "make the storage layer resumable", false)
	if err != nil {
		t.Fatal(err)
	}
	waitForTurns(t, h, p.ID, 2)
	if err := h.eng.Accept(ctx, p.ID); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got := waitForProposal(t, h, p.ID, store.ProposalReady, store.ProposalFailed); got.State != store.ProposalReady {
		t.Fatalf("State = %q (%s), want ready", got.State, got.ErrMsg)
	}

	rows, err := h.st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Subject != "Scaffold the module" {
		t.Errorf("Subject = %q, want the architect's subject", rows[0].Subject)
	}
	// The second task was given no subject: it derives one, exactly as the
	// analysis path does.
	if rows[1].Subject != "Walk a local tree and emit its files" {
		t.Errorf("derived Subject = %q, want the goal's first sentence", rows[1].Subject)
	}

	created, err := h.eng.QueueProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if created[0].Subject != "Scaffold the module" {
		t.Errorf("task Subject = %q, want the architect's subject", created[0].Subject)
	}
	if created[0].Slug != "scaffold-the-module" {
		t.Errorf("task Slug = %q, want the subject's slug", created[0].Slug)
	}
}

// `overseer new` is one process doing one thing: it owns the store and closes
// it on the way out. Scheduling the opening turn and returning killed it
// mid-flight and closed the database under whatever was left of it, then
// pointed the operator at a conversation containing nothing but their own
// brief.
func TestStartDesignAndWaitReturnsOnlyAfterTheReplyIsRecorded(t *testing.T) {
	h := newHarness(t, fakeArchitect(t, "Two questions before I sketch this."), "true")
	ctx := context.Background()

	p, err := h.eng.StartDesignAndWait(ctx, "", "a CLI that syncs S3 buckets", false)
	if err != nil {
		t.Fatalf("StartDesignAndWait: %v", err)
	}

	// Read once, with no polling: if this needs to wait, the method did not.
	turns, err := h.st.ArchitectTurns(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turn(s), want the brief and the reply", len(turns))
	}
	if turns[1].Speaker != store.SpeakerArchitect {
		t.Errorf("second turn = %s, want the architect", turns[1].Speaker)
	}
	if !strings.Contains(turns[1].Body, "Two questions") {
		t.Errorf("architect said %q", turns[1].Body)
	}
}

// A turn that produced no reply is an error to a caller that waited for one,
// even though it is also recorded as a turn: `overseer new` must exit non-zero
// rather than print a URL as though a conversation had started. The proposal
// comes back regardless, because the conversation exists and is where the
// operator should be sent.
func TestStartDesignAndWaitReportsATurnThatProducedNoReply(t *testing.T) {
	h := newHarness(t, writeScript(t, "claude", "exit 1"), "true")
	ctx := context.Background()

	p, err := h.eng.StartDesignAndWait(ctx, "", "a thing", false)
	if err == nil {
		t.Fatal("StartDesignAndWait returned nil for a turn that produced no reply")
	}
	if p.ID == 0 {
		t.Fatal("no proposal returned; there would be nowhere to send the operator")
	}

	turns, err := h.st.ArchitectTurns(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turn(s), want the brief and a recorded failure", len(turns))
	}
	if turns[1].ErrMsg == "" {
		t.Error("the failure was not recorded on the turn")
	}
}

// hangingArchitect emits its session line and then hangs until its process
// group is killed. It stands in for the architect mid-reply.
//
// It signals that it started through its stdout rather than a file, because it
// runs inside the sandbox where nothing the test owns is mounted — but the
// runner mirrors every line it prints into the transcript, which is written
// outside. See waitForArchitectStart.
func hangingArchitect(t *testing.T) string {
	t.Helper()
	return writeScript(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"arch-sess"}'
# Hang until the process group is killed.
while true; do sleep 0.05; done
`)
}

// waitForArchitectStart blocks until the agent has actually produced output,
// which is the only proof it is running rather than about to.
func waitForArchitectStart(t *testing.T, h *harness, proposalID int64) {
	t.Helper()
	path := filepath.Join(h.eng.proposalDir(proposalID), "architect.jsonl")
	waitFor(t, "the architect to start replying", func() bool {
		b, err := os.ReadFile(path)
		return err == nil && strings.Contains(string(b), "arch-sess")
	})
}

// Ctrl-C during `overseer new`. The agent is in a process group of its own, so
// only the cancelled context reaches it — and the failure then has to be
// written with a context that is NOT the cancelled one, or nothing is recorded
// at all. That is the whole defect: architectBusy reads a conversation whose
// last speaker is the operator as "a reply is still coming", so Say refuses,
// and an unrecorded failure wedges the conversation for good.
func TestAnInterruptedTurnIsRecordedAsAFailure(t *testing.T) {
	h := newHarness(t, hangingArchitect(t), "true")
	// Bounds the test if the cancellation somehow fails to reach the agent,
	// rather than sitting on the 30-minute default.
	h.eng.Cfg.AnalysisTimeout = 30 * time.Second

	p, err := h.eng.openDesign(context.Background(), "", "a thing", false)
	if err != nil {
		t.Fatalf("openDesign: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.eng.architectTurn(ctx, p.ID, "") }()

	waitForArchitectStart(t, h, p.ID)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an interrupted turn reported success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("architectTurn did not return after its context was cancelled")
	}

	turns, err := h.st.ArchitectTurns(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turn(s), want the brief and a recorded interruption", len(turns))
	}
	if !strings.Contains(turns[1].ErrMsg, "interrupted") {
		t.Errorf("the interrupted turn recorded %q; a killed agent reports "+
			"\"signal: killed\", which reads as a crash", turns[1].ErrMsg)
	}
}

// The session exists on disk whatever happened to the context, and losing it
// would silently start the conversation over on the next turn — from the
// dashboard, where the operator goes to pick up what they interrupted.
func TestAnInterruptedTurnKeepsTheSession(t *testing.T) {
	h := newHarness(t, hangingArchitect(t), "true")
	h.eng.Cfg.AnalysisTimeout = 30 * time.Second

	p, err := h.eng.openDesign(context.Background(), "", "a thing", false)
	if err != nil {
		t.Fatalf("openDesign: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.eng.architectTurn(ctx, p.ID, "") }()

	waitForArchitectStart(t, h, p.ID)
	cancel()
	<-done

	got, err := h.st.GetProposal(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ArchitectSession != "arch-sess" {
		t.Errorf("ArchitectSession = %q, want arch-sess", got.ArchitectSession)
	}
}

// The first window: Ctrl-C between creating the project and the turn reading
// the store. GetProposal fails with the dead context — database/sql checks
// ctx.Done() before it does anything else, so this is exactly what happens and
// not merely what might — and returning that error without writing anything
// wedges the conversation as thoroughly as a killed agent does.
func TestATurnCancelledBeforeItReadsTheStoreRecordsAFailure(t *testing.T) {
	h := newHarness(t, fakeArchitect(t, "never reached"), "true")

	p, err := h.eng.openDesign(context.Background(), "", "a thing", false)
	if err != nil {
		t.Fatalf("openDesign: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := h.eng.architectTurn(ctx, p.ID, ""); err == nil {
		t.Fatal("architectTurn returned nil for an already-cancelled context")
	}

	turns, err := h.st.ArchitectTurns(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turn(s); the conversation was left with the operator "+
			"as its last speaker, which Say refuses to continue", len(turns))
	}
	if !strings.Contains(turns[1].ErrMsg, "interrupted") {
		t.Errorf("the store read failed with %q; the operator pressed Ctrl-C, "+
			"and that is what the turn should say", turns[1].ErrMsg)
	}
}

// Every way of pressing Ctrl-C arrives here wearing a different disguise: the
// runner reports the SIGKILLed agent as "signal: killed", and the store reports
// the dead context as "context canceled". Neither says what happened; ctx does.
//
// Called directly because that is the only way to pin the substitution and the
// detached write on their own, without a turn's worth of scheduling in between.
func TestFailArchitectTurnRecordsThroughACancelledContext(t *testing.T) {
	h := newHarness(t, "true", "true")

	p, err := h.eng.openDesign(context.Background(), "", "a thing", false)
	if err != nil {
		t.Fatalf("openDesign: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := h.eng.failArchitectTurn(ctx, p.ID, "signal: killed"); err == nil {
		t.Fatal("failArchitectTurn returned nil for a failed turn")
	}

	turns, err := h.st.ArchitectTurns(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turn(s); the failure was not written at all", len(turns))
	}
	if turns[1].ErrMsg != interruptedMsg {
		t.Errorf("recorded %q, want %q", turns[1].ErrMsg, interruptedMsg)
	}
}

// The third window: Ctrl-C after the architect replied but before the reply is
// written down. The reply happened and was paid for, so the cancelled context
// must not be what decides whether it survives — and a reply that vanishes
// leaves the conversation wedged in exactly the way a lost failure does, from
// the one path where nothing actually went wrong.
//
// The window itself is the gap between two statements and cannot be scheduled
// from a test, so the insert is its own function and this pins it directly.
func TestAReplyIsRecordedThroughACancelledContext(t *testing.T) {
	h := newHarness(t, "true", "true")

	p, err := h.eng.openDesign(context.Background(), "", "a thing", false)
	if err != nil {
		t.Fatalf("openDesign: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := agent.Result{CostUSD: 0.3, InputTokens: 100, OutputTokens: 50}
	if err := h.eng.recordArchitectReply(ctx, p.ID, "Two questions.", res); err != nil {
		t.Fatalf("recordArchitectReply: %v", err)
	}

	turns, err := h.st.ArchitectTurns(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turn(s); the reply was thrown away", len(turns))
	}
	if turns[1].Speaker != store.SpeakerArchitect || turns[1].Body != "Two questions." {
		t.Errorf("second turn = %s: %q", turns[1].Speaker, turns[1].Body)
	}
	if turns[1].ErrMsg != "" {
		t.Errorf("a delivered reply recorded an error: %q", turns[1].ErrMsg)
	}
	if turns[1].CostUSD == 0 {
		t.Error("the reply recorded no usage; it was paid for either way")
	}
}
