package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"overseer/internal/agent"
	"overseer/internal/config"
	"overseer/internal/sandbox"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

// CreateProject makes a new git repository at path and registers it.
//
// It ends with an empty initial commit, and that is load-bearing rather than
// tidiness. A repository with no commits has no HEAD, so resolving its default
// branch fails and `git worktree add` cannot check out an unborn ref — every
// task against it would fail at setup. One commit and the entire existing
// worktree machinery works untouched, with no unborn-branch special case
// anywhere. It also means a scaffold turn that dies halfway leaves a valid
// repository rather than a half-made one.
func (e *Engine) CreateProject(ctx context.Context, path string) (store.Repo, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return store.Repo{}, fmt.Errorf("where should the project go?")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return store.Repo{}, fmt.Errorf("resolve %s: %w", path, err)
	}

	// An existing repository is not somewhere to put a new one, and neither is
	// anywhere inside one: git resolves upwards, so a project created under
	// another one would share its history and its branches.
	//
	// Asked of the nearest existing ancestor rather than the path itself, since
	// the path usually does not exist yet — which is the whole point.
	if err := checkRepo(ctx, abs); err == nil {
		return store.Repo{}, fmt.Errorf("%s is already a git repository", abs)
	}
	if enclosing, err := enclosingRepo(ctx, abs); err != nil {
		return store.Repo{}, err
	} else if enclosing != "" {
		return store.Repo{}, fmt.Errorf("%s is inside the repository at %s; "+
			"a new project needs a path of its own", abs, enclosing)
	}

	// A directory that already has something in it is a project somebody
	// started. Scaffolding into it would write over whatever that was.
	if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
		return store.Repo{}, fmt.Errorf("%s is not empty; a new project needs an empty or new directory", abs)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return store.Repo{}, fmt.Errorf("create %s: %w", abs, err)
	}

	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		// Identity, so the commit below works on a machine with no global git
		// config — a container, most obviously. Local to this repository, so it
		// says nothing about the operator's own settings.
		{"config", "user.name", "overseer"},
		{"config", "user.email", "overseer@localhost"},
		{"commit", "--allow-empty", "-m", "overseer: new project"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = abs
		if out, err := cmd.CombinedOutput(); err != nil {
			return store.Repo{}, fmt.Errorf("git %s: %w: %s",
				strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return e.EnsureRepo(ctx, abs)
}

// enclosingRepo returns the repository path contains, or "" if it is not
// inside one.
//
// It walks up to the first directory that exists, because the path a new
// project is going to usually does not yet — and git, asked from a directory
// that is not there, reports a chdir failure rather than anything about
// repositories.
func enclosingRepo(ctx context.Context, path string) (string, error) {
	dir := path
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil // walked to the root and found nothing
		}
		dir = parent
	}

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", nil // not inside a repository, which is what we want
	}
	return strings.TrimSpace(string(out)), nil
}

// scaffold writes a new project's first real commit.
//
// The one agent turn that runs outside a worktree and outside the review loop.
// It is here rather than as a task because of what `done` means: a task's work
// lands on its own branch in an unmerged pull request, so a scaffold task would
// leave every dependent task branching from a still-empty default branch. Doing
// it before anything is queued is what makes every later task ordinary.
//
// Unreviewed, and narrow on purpose — it builds the skeleton and stops. The
// features are tasks, and they get the whole loop.
func (e *Engine) scaffold(ctx context.Context, proposalID int64) {
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return
	}
	if p.RepoPath == "" {
		e.failProposal(ctx, proposalID, "there is nowhere to scaffold: this proposal has no repository", agent.Result{})
		return
	}

	res, err := e.runScaffold(ctx, &p)
	if err != nil {
		e.failProposal(ctx, proposalID, err.Error(), res)
		return
	}
	if res.ErrMsg != "" {
		if agent.IsAuthFailure(res.ErrMsg) {
			e.Pause(fmt.Sprintf("the scaffold agent is not authenticated: %s", res.ErrMsg))
		}
		e.failProposal(ctx, proposalID, "scaffolding failed: "+res.ErrMsg, res)
		return
	}

	// The design lands with the scaffold, in the repository, where every later
	// task and every reviewer can read it. This is the point at which writing
	// it into the tree is safe: the tree is one overseer just made.
	if p.Design != "" {
		if err := os.WriteFile(filepath.Join(p.RepoPath, "DESIGN.md"),
			[]byte(strings.TrimSuffix(p.Design, "\n")+"\n"), 0o644); err != nil {
			e.failProposal(ctx, proposalID, "write DESIGN.md: "+err.Error(), res)
			return
		}
	}

	wt := worktree.Worktree{RepoPath: p.RepoPath, Dir: p.RepoPath}
	committed, err := e.WT.Commit(ctx, wt, "overseer: scaffold")
	if err != nil {
		e.failProposal(ctx, proposalID, "commit the scaffold: "+err.Error(), res)
		return
	}
	if !committed {
		e.failProposal(ctx, proposalID,
			"the scaffold turn wrote nothing; there is no project to build on", res)
		return
	}

	// Re-read: the repository now has a toolchain and a default branch it did
	// not have when it was registered, and the probe is what puts them in front
	// of the operator.
	if _, err := e.EnsureRepo(ctx, p.RepoPath); err != nil {
		e.logf("proposal %d: re-probe the scaffolded repository: %v", proposalID, err)
	}

	p, err = e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return
	}
	p.State = store.ProposalReady
	p.ErrMsg = ""
	p.CostUSD += res.CostUSD
	p.InputTokens += res.InputTokens
	p.OutputTokens += res.OutputTokens
	if repo, err := e.Store.RepoByPath(ctx, p.RepoPath); err == nil {
		p.Detected = repo.Detected
	}
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return
	}
	e.notifyProposal()
}

// runScaffold invokes the agent that writes the project.
//
// The one place a non-task agent turn gets a WRITABLE repository. That is the
// whole difference from an analysis, and it is safe here for a reason that does
// not generalise: this repository is one overseer created a moment ago, and it
// contains a single empty commit.
func (e *Engine) runScaffold(ctx context.Context, p *store.Proposal) (agent.Result, error) {
	role, err := e.resolveRole(config.RoleCode)
	if err != nil {
		return agent.Result{ErrMsg: err.Error()}, nil
	}

	runDir := e.proposalDir(p.ID)
	if err := sandbox.EnsureDirs(runDir); err != nil {
		return agent.Result{}, err
	}
	if err := prepareAgentStateIn(runDir, role.Agent); err != nil {
		return agent.Result{}, err
	}
	transcript := filepath.Join(runDir, "scaffold.jsonl")

	return role.Runner.Run(ctx, agent.RunSpec{
		Args:           role.args(ScaffoldPrompt(p.Design), "", "", ""),
		Dir:            p.RepoPath,
		TranscriptPath: transcript,
		Timeout:        e.Cfg.StepTimeout,
		Attempt:        1,
		Sandbox:        e.Sandbox,
		SandboxSpec:    e.scaffoldSandboxSpec(p.RepoPath, runDir, role.Agent),
		Env:            e.agentEnv(role),
		OnEvent:        e.progressNotifier(0),
	})
}
