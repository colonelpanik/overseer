// Package worktree owns every git and gh invocation overseer makes. Each
// task works in its own worktree on its own branch, so parallel agents
// cannot collide and the user's checkout is never touched.
package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Worktree is one task's isolated checkout.
type Worktree struct {
	RepoPath string
	Dir      string
	Branch   string
	// BaseRef is the fully-qualified ref the branch was cut from, and the
	// base Codex reviews the diff against.
	BaseRef string
	// CommonDir is the repository's shared git directory, resolved with
	// rev-parse rather than assumed to be RepoPath/.git — a submitted path
	// may itself be a linked worktree, where .git is a file.
	CommonDir string
	// AdminDir is this worktree's own administrative directory, normally
	// <CommonDir>/worktrees/<name>. Git operations in the worktree write
	// here, so it must be writable for even `git status` to work.
	AdminDir string
}

// Manager creates and tears down worktrees under Root.
type Manager struct {
	Root string

	mu     sync.Mutex
	repoMu map[string]*sync.Mutex
}

// NewManager returns a Manager that places worktrees under root.
func NewManager(root string) *Manager {
	return &Manager{Root: root, repoMu: map[string]*sync.Mutex{}}
}

// lockRepo serialises operations that touch one repository's index. Two
// workers running `git worktree add` against the same repo at once will
// otherwise race on index.lock.
func (m *Manager) lockRepo(repoPath string) func() {
	m.mu.Lock()
	mu, ok := m.repoMu[repoPath]
	if !ok {
		mu = &sync.Mutex{}
		m.repoMu[repoPath] = mu
	}
	m.mu.Unlock()

	mu.Lock()
	return mu.Unlock
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a goal into a branch-safe, filesystem-safe identifier.
func Slugify(s string) string {
	out := slugUnsafe.ReplaceAllString(strings.ToLower(s), "-")
	out = strings.Trim(out, "-")
	if len(out) > 60 {
		out = strings.Trim(out[:60], "-")
	}
	if out == "" {
		return "task"
	}
	return out
}

// git runs a git command in dir and returns trimmed combined output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Agents must never be prompted for credentials; a prompt would hang
	// the step until its timeout.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// branchProbe is the result of resolving a repository's default branch,
// together with enough context for Create to tell "no remote configured,
// this repo is offline by design" apart from "a remote is configured but
// could not be reached right now". DefaultBranch's plain (string, error)
// return cannot make that distinction, but Create needs it: silently
// falling back to the local branch in the second case would run the whole
// plan/execute loop against a stale or diverged base and only fail much
// later, at Push.
type branchProbe struct {
	name string
	// hasOrigin is true when the repository has an "origin" remote
	// configured at all. It says nothing about whether that remote is
	// currently reachable.
	hasOrigin bool
}

// resolveDefaultBranch does the work behind DefaultBranch, additionally
// reporting whether an "origin" remote is configured.
func resolveDefaultBranch(ctx context.Context, repoPath string) (branchProbe, error) {
	hasOrigin := hasRemote(ctx, repoPath, "origin")

	if out, err := git(ctx, repoPath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if _, branch, ok := strings.Cut(out, "/"); ok && branch != "" {
			return branchProbe{name: branch, hasOrigin: hasOrigin}, nil
		}
	}
	// origin/HEAD is often unset in a freshly cloned or locally-created
	// repo; ask the remote directly.
	if out, err := git(ctx, repoPath, "ls-remote", "--symref", "origin", "HEAD"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "ref: refs/heads/") {
				rest := strings.TrimPrefix(line, "ref: refs/heads/")
				if name, _, ok := strings.Cut(rest, "\t"); ok {
					return branchProbe{name: strings.TrimSpace(name), hasOrigin: hasOrigin}, nil
				}
			}
		}
	}
	name, err := git(ctx, repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return branchProbe{}, fmt.Errorf("determine default branch: %w", err)
	}
	return branchProbe{name: name, hasOrigin: hasOrigin}, nil
}

// hasRemote reports whether repoPath has a remote configured under name.
// This is a local, read-only config lookup with no network I/O, so it costs
// nothing extra even when the remote itself is unreachable.
func hasRemote(ctx context.Context, repoPath, name string) bool {
	out, err := git(ctx, repoPath, "remote")
	if err != nil {
		return false
	}
	for _, n := range strings.Fields(out) {
		if n == name {
			return true
		}
	}
	return false
}

// remoteRefMissing reports whether a `git fetch` failure means the ref
// simply does not exist yet on an otherwise-healthy, reachable remote --
// e.g. a brand-new repository that just had origin added, with nothing
// pushed -- as opposed to the remote being unreachable at all (bad host,
// auth failure, deleted repository, protocol mismatch, shallow-fetch
// trouble). Both cases exit git with status 128, so the exit code cannot
// tell them apart; git's own wording can. "couldn't find remote ref" is
// specific to a missing ref, and every other fatal fetch error is spelled
// differently (e.g. "Could not read from remote repository", "Could not
// resolve host", "Permission denied").
func remoteRefMissing(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "couldn't find remote ref")
}

// OriginURL reports the repository's origin remote, or an error if it has
// none. It is a local config read with no network I/O, so it costs nothing
// even when the remote is unreachable.
//
// This is display only: it is what lets the repo list show where a repository
// came from. Nothing clones or fetches from it, so it is never run through
// ValidateCloneURL — that check guards URLs an operator hands to `git clone`,
// and applying it here would make a repository with an unusual but perfectly
// working remote appear to have none.
func OriginURL(ctx context.Context, repoPath string) (string, error) {
	out, err := git(ctx, repoPath, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("no origin remote: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// DefaultBranch reports the repository's default branch, preferring what
// origin advertises and falling back to the current branch for repos with
// no remote.
func DefaultBranch(ctx context.Context, repoPath string) (string, error) {
	p, err := resolveDefaultBranch(ctx, repoPath)
	if err != nil {
		return "", err
	}
	return p.name, nil
}

// Create cuts a fresh branch from the repository's default branch and checks
// it out into its own worktree. It fetches first so the branch starts from
// the current remote tip.
//
// Create is idempotent. The task's state reaches "worktree" before this runs,
// so a daemon that exits between `git worktree add` succeeding and the paths
// being persisted will call Create again. Without adoption the second call
// fails on the existing branch and directory, and a task whose worktree was
// created perfectly well would be marked failed.
func (m *Manager) Create(ctx context.Context, repoPath, slug string) (Worktree, error) {
	unlock := m.lockRepo(repoPath)
	defer unlock()

	wt := Worktree{
		RepoPath: repoPath,
		Dir:      filepath.Join(m.Root, slug),
		Branch:   "overseer/" + slug,
	}

	probe, err := resolveDefaultBranch(ctx, repoPath)
	if err != nil {
		return Worktree{}, err
	}
	base := probe.name

	// Adopt an existing worktree from an interrupted attempt.
	if adopted, ok, err := m.adopt(ctx, wt, base); err != nil {
		return Worktree{}, err
	} else if ok {
		return adopted, nil
	}

	baseRef := "origin/" + base
	if _, err := git(ctx, repoPath, "fetch", "origin", base); err != nil {
		switch {
		case remoteRefMissing(err):
			// The remote is healthy and reachable; it simply does not have
			// this ref yet -- e.g. a brand-new repository that just had
			// origin added, with nothing pushed. That is exactly the
			// legitimate case the local fallback exists for, so fall back
			// quietly rather than misreporting a fine remote as unreachable.
			baseRef = base
		case probe.hasOrigin:
			// A remote is configured but a fetch against it failed for some
			// other reason: unreachable host, auth failure, protocol
			// mismatch, shallow-fetch trouble, etc. Falling back to the
			// local branch here would run the whole plan/execute loop
			// against a stale or diverged base and only fail much later, at
			// Push; fail loudly now instead, at the cheapest possible point.
			return Worktree{}, fmt.Errorf("fetch origin %s: remote is configured but unreachable: %w", base, err)
		default:
			// No remote at all is a legitimate offline repository.
			baseRef = base
		}
	} else if _, err := git(ctx, repoPath, "rev-parse", "--verify", baseRef); err != nil {
		baseRef = base
	}
	wt.BaseRef = baseRef

	if err := os.MkdirAll(m.Root, 0o755); err != nil {
		return Worktree{}, fmt.Errorf("create worktree root: %w", err)
	}
	if _, err := git(ctx, repoPath, "worktree", "add", "-b", wt.Branch, wt.Dir, baseRef); err != nil {
		return Worktree{}, fmt.Errorf("create worktree: %w", err)
	}
	return m.resolveDirs(ctx, wt)
}

// adopt returns an existing worktree at wt.Dir when it is already checked out
// on wt.Branch, so an interrupted Create can be repeated safely. A directory
// that exists but is not the expected worktree is an error rather than
// something to reuse or delete.
func (m *Manager) adopt(ctx context.Context, wt Worktree, base string) (Worktree, bool, error) {
	if _, err := os.Stat(wt.Dir); err != nil {
		if os.IsNotExist(err) {
			return Worktree{}, false, nil
		}
		return Worktree{}, false, fmt.Errorf("stat worktree dir: %w", err)
	}

	branch, err := git(ctx, wt.Dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return Worktree{}, false, fmt.Errorf(
			"%s exists but is not a usable git worktree: %w", wt.Dir, err)
	}
	if branch != wt.Branch {
		return Worktree{}, false, fmt.Errorf(
			"%s already exists on branch %q, expected %q", wt.Dir, branch, wt.Branch)
	}

	// BaseRef is derived, not stored in git, so recompute it the same way
	// Create would.
	wt.BaseRef = "origin/" + base
	if _, err := git(ctx, wt.Dir, "rev-parse", "--verify", wt.BaseRef); err != nil {
		wt.BaseRef = base
	}
	resolved, err := m.resolveDirs(ctx, wt)
	if err != nil {
		return Worktree{}, false, err
	}
	return resolved, true, nil
}

// resolveDirs fills in CommonDir and AdminDir by asking git, because the
// layout cannot be assumed: when the submitted repository path is itself a
// linked worktree its .git is a file, and <path>/.git/worktrees/<slug> does
// not exist at all.
func (m *Manager) resolveDirs(ctx context.Context, wt Worktree) (Worktree, error) {
	common, err := git(ctx, wt.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve git common dir: %w", err)
	}
	admin, err := git(ctx, wt.Dir, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve git dir: %w", err)
	}
	wt.CommonDir = common
	wt.AdminDir = admin
	return wt, nil
}

// Commit stages everything including untracked files and commits. It
// reports false when the tree was already clean.
func (m *Manager) Commit(ctx context.Context, wt Worktree, message string) (bool, error) {
	if _, err := git(ctx, wt.Dir, "add", "-A"); err != nil {
		return false, err
	}
	// --quiet exits 1 when there is something staged.
	if _, err := git(ctx, wt.Dir, "diff", "--cached", "--quiet"); err == nil {
		return false, nil
	}
	if _, err := git(ctx, wt.Dir, "-c", "user.name=overseer",
		"-c", "user.email=overseer@localhost",
		"commit", "-m", message); err != nil {
		return false, err
	}
	return true, nil
}

// HasCommits reports whether the branch is ahead of its base.
func (m *Manager) HasCommits(ctx context.Context, wt Worktree) (bool, error) {
	out, err := git(ctx, wt.Dir, "rev-list", "--count", wt.BaseRef+"..HEAD")
	if err != nil {
		return false, err
	}
	return out != "0" && out != "", nil
}

// Diff returns the accumulated diff against the base ref.
func (m *Manager) Diff(ctx context.Context, wt Worktree) (string, error) {
	return git(ctx, wt.Dir, "diff", wt.BaseRef+"...HEAD")
}

// Push publishes the branch and sets upstream.
func (m *Manager) Push(ctx context.Context, wt Worktree) error {
	_, err := git(ctx, wt.Dir, "push", "-u", "origin", wt.Branch)
	return err
}

// Remove deletes the worktree directory. The branch is deliberately kept:
// it is the only durable record of the work.
func (m *Manager) Remove(ctx context.Context, wt Worktree) error {
	unlock := m.lockRepo(wt.RepoPath)
	defer unlock()

	if _, err := git(ctx, wt.RepoPath, "worktree", "remove", "--force", wt.Dir); err != nil {
		// Fall back to removing the directory and pruning metadata, so a
		// partially-created worktree cannot wedge the repo permanently.
		if rmErr := os.RemoveAll(wt.Dir); rmErr != nil {
			return fmt.Errorf("remove worktree dir: %w (git said: %v)", rmErr, err)
		}
		if _, pruneErr := git(ctx, wt.RepoPath, "worktree", "prune"); pruneErr != nil {
			return fmt.Errorf("prune worktrees: %w", pruneErr)
		}
	}
	return nil
}
