package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRun runs git in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepoWithRemote creates a repo on branch main with one commit, plus a
// bare remote named origin that it already tracks.
func newRepoWithRemote(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "remote.git")
	work := filepath.Join(base, "work")

	gitRun(t, base, "init", "--bare", "--initial-branch=main", bare)
	gitRun(t, base, "init", "--initial-branch=main", work)
	gitRun(t, work, "config", "user.name", "test")
	gitRun(t, work, "config", "user.email", "test@example.com")

	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "initial")
	gitRun(t, work, "remote", "add", "origin", bare)
	gitRun(t, work, "push", "-u", "origin", "main")
	return work
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Add CSV export to the rack view": "add-csv-export-to-the-rack-view",
		"Fix  bug/in::thing":              "fix-bug-in-thing",
		"---leading and trailing---":      "leading-and-trailing",
		"":                                "task",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
	long := Slugify(strings.Repeat("word ", 40))
	if len(long) > 60 {
		t.Errorf("Slugify produced a %d-char slug; want it capped at 60", len(long))
	}
}

func TestDefaultBranch(t *testing.T) {
	repo := newRepoWithRemote(t)
	got, err := DefaultBranch(context.Background(), repo)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if got != "main" {
		t.Errorf("DefaultBranch = %q, want main", got)
	}
}

func TestCreateMakesIsolatedWorktreeOnNewBranch(t *testing.T) {
	ctx := context.Background()
	repo := newRepoWithRemote(t)
	m := NewManager(t.TempDir())

	wt, err := m.Create(ctx, repo, "add-csv-export")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if wt.Branch != "overseer/add-csv-export" {
		t.Errorf("Branch = %q, want overseer/add-csv-export", wt.Branch)
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, "README.md")); err != nil {
		t.Errorf("worktree does not contain the repo contents: %v", err)
	}

	// The original checkout must be untouched.
	if branch := gitRun(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); branch != "main" {
		t.Errorf("source checkout switched to %q; it must stay on main", branch)
	}
}

func TestCreateResolvesOriginBaseRefWhenRemoteIsHealthy(t *testing.T) {
	ctx := context.Background()
	repo := newRepoWithRemote(t)
	m := NewManager(t.TempDir())

	wt, err := m.Create(ctx, repo, "healthy-remote")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if wt.BaseRef != "origin/main" {
		t.Errorf("BaseRef = %q, want origin/main", wt.BaseRef)
	}
}

func TestCreateFailsLoudlyWhenTheConfiguredRemoteIsUnreachable(t *testing.T) {
	// The remote is still configured (git config still has an "origin"
	// entry), but the repository behind it is gone -- e.g. deleted, or the
	// host is unreachable. This must not be treated the same as a
	// legitimately offline repo with no remote at all: silently falling
	// back to the local branch would let the whole plan/execute loop run
	// against a stale or diverged base and only fail much later, at Push.
	ctx := context.Background()
	repo := newRepoWithRemote(t)
	m := NewManager(t.TempDir())

	remoteURL := gitRun(t, repo, "remote", "get-url", "origin")
	if err := os.RemoveAll(remoteURL); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Create(ctx, repo, "broken-remote"); err == nil {
		t.Fatal("Create must fail when the configured remote cannot be reached, not silently fall back to the local branch")
	}
}

func TestCreateTwiceAdoptsTheExistingWorktree(t *testing.T) {
	// Recovery re-dispatches worktree setup when the daemon exited after
	// `git worktree add` but before the paths were persisted. The second call
	// must adopt, not fail a task whose worktree is perfectly fine.
	ctx := context.Background()
	repo := newRepoWithRemote(t)
	m := NewManager(t.TempDir())

	first, err := m.Create(ctx, repo, "dup")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.Dir, "work-in-progress.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := m.Create(ctx, repo, "dup")
	if err != nil {
		t.Fatalf("second Create must adopt, not fail: %v", err)
	}
	if second.Dir != first.Dir || second.Branch != first.Branch {
		t.Errorf("adopted a different worktree: %+v vs %+v", second, first)
	}
	if second.BaseRef != first.BaseRef {
		t.Errorf("BaseRef = %q, want %q", second.BaseRef, first.BaseRef)
	}
	if second.CommonDir != first.CommonDir || second.AdminDir != first.AdminDir {
		t.Errorf("git dirs differ after adoption: %+v vs %+v", second, first)
	}
	// Adoption must not discard work already done in the worktree.
	if _, err := os.Stat(filepath.Join(second.Dir, "work-in-progress.txt")); err != nil {
		t.Errorf("adoption destroyed existing work: %v", err)
	}
}

func TestCreateRefusesAnUnrelatedDirectoryInTheWay(t *testing.T) {
	ctx := context.Background()
	repo := newRepoWithRemote(t)
	root := t.TempDir()
	m := NewManager(root)

	// Something else already occupies the path. Adopting or deleting it
	// would both be wrong, so this must be a loud error.
	inTheWay := filepath.Join(root, "occupied")
	if err := os.MkdirAll(inTheWay, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inTheWay, "someones-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Create(ctx, repo, "occupied"); err == nil {
		t.Fatal("expected an error for a non-worktree directory in the way")
	}
	if _, err := os.Stat(filepath.Join(inTheWay, "someones-file")); err != nil {
		t.Errorf("Create removed a directory it did not create: %v", err)
	}
}

func TestCreateResolvesGitDirsViaGit(t *testing.T) {
	ctx := context.Background()
	repo := newRepoWithRemote(t)
	m := NewManager(t.TempDir())

	wt, err := m.Create(ctx, repo, "dirs")
	if err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"CommonDir": wt.CommonDir, "AdminDir": wt.AdminDir} {
		if dir == "" {
			t.Errorf("%s is empty", name)
			continue
		}
		if !filepath.IsAbs(dir) {
			t.Errorf("%s = %q, want an absolute path", name, dir)
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("%s = %q is not an existing directory (err %v)", name, dir, err)
		}
	}
	if wt.CommonDir == wt.AdminDir {
		t.Error("AdminDir must be the worktree's own directory, not the common one")
	}
}

func TestCreateFromARepoThatIsItselfALinkedWorktree(t *testing.T) {
	// Submit() accepts any path where `git rev-parse --git-dir` works, which
	// includes a linked worktree. There .git is a FILE, so assuming
	// <repo>/.git/worktrees/<slug> yields a path that does not exist and the
	// sandbox mount fails.
	ctx := context.Background()
	primary := newRepoWithRemote(t)
	linked := filepath.Join(filepath.Dir(primary), "linked")
	gitRun(t, primary, "worktree", "add", "-b", "side", linked, "main")

	if fi, err := os.Stat(filepath.Join(linked, ".git")); err != nil || fi.IsDir() {
		t.Fatalf("fixture wrong: linked/.git should be a file (err %v)", err)
	}

	m := NewManager(t.TempDir())
	wt, err := m.Create(ctx, linked, "from-linked")
	if err != nil {
		t.Fatalf("Create from a linked worktree: %v", err)
	}

	// The common dir must be the primary repository's, not <linked>/.git.
	if wt.CommonDir == filepath.Join(linked, ".git") {
		t.Errorf("CommonDir = %q, which is the .git FILE, not the shared dir", wt.CommonDir)
	}
	for name, dir := range map[string]string{"CommonDir": wt.CommonDir, "AdminDir": wt.AdminDir} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("%s = %q is not an existing directory (err %v)", name, dir, err)
		}
	}
	// And the worktree must actually work.
	if _, err := git(ctx, wt.Dir, "status", "--porcelain"); err != nil {
		t.Errorf("git status in the new worktree failed: %v", err)
	}
}

func TestCommitReportsNothingToCommit(t *testing.T) {
	ctx := context.Background()
	repo := newRepoWithRemote(t)
	m := NewManager(t.TempDir())

	wt, err := m.Create(ctx, repo, "empty")
	if err != nil {
		t.Fatal(err)
	}
	committed, err := m.Commit(ctx, wt, "nothing here")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if committed {
		t.Error("Commit reported a commit with a clean tree")
	}
}

func TestCommitIncludesUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	repo := newRepoWithRemote(t)
	m := NewManager(t.TempDir())

	wt, err := m.Create(ctx, repo, "with-plan")
	if err != nil {
		t.Fatal(err)
	}
	// PLAN.md is a new file; a commit that only picks up tracked changes
	// would silently drop the plan.
	if err := os.WriteFile(filepath.Join(wt.Dir, "PLAN.md"), []byte("# plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	committed, err := m.Commit(ctx, wt, "add plan")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !committed {
		t.Fatal("Commit reported nothing to commit with an untracked file present")
	}

	has, err := m.HasCommits(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("HasCommits = false after committing")
	}

	diff, err := m.Diff(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "PLAN.md") {
		t.Errorf("Diff does not mention PLAN.md:\n%s", diff)
	}
}

func TestPushToLocalBareRemote(t *testing.T) {
	ctx := context.Background()
	repo := newRepoWithRemote(t)
	m := NewManager(t.TempDir())

	wt, err := m.Create(ctx, repo, "pushable")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Dir, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Commit(ctx, wt, "add new.txt"); err != nil {
		t.Fatal(err)
	}
	if err := m.Push(ctx, wt); err != nil {
		t.Fatalf("Push: %v", err)
	}

	remotes := gitRun(t, repo, "ls-remote", "--heads", "origin")
	if !strings.Contains(remotes, "overseer/pushable") {
		t.Errorf("branch not on remote:\n%s", remotes)
	}
}

func TestRemoveDeletesWorktreeButKeepsBranch(t *testing.T) {
	ctx := context.Background()
	repo := newRepoWithRemote(t)
	m := NewManager(t.TempDir())

	wt, err := m.Create(ctx, repo, "removable")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Errorf("worktree dir still present: %v", err)
	}
	branches := gitRun(t, repo, "branch", "--list", "overseer/removable")
	if !strings.Contains(branches, "overseer/removable") {
		t.Error("Remove deleted the branch; the branch must always survive")
	}
}
