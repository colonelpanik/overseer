package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// PRRequest describes the pull request to open for a finished task.
type PRRequest struct {
	Worktree   Worktree
	Title      string
	Body       string
	BaseBranch string
}

// PROpener opens a pull request. It is an interface so the end-to-end test
// can assert the call instead of contacting GitHub.
type PROpener interface {
	Open(ctx context.Context, req PRRequest) (string, error)
}

// GhOpener opens draft pull requests with the gh CLI.
type GhOpener struct {
	Bin string
}

// NewGhOpener returns a GhOpener using the given gh binary.
func NewGhOpener(bin string) *GhOpener {
	if bin == "" {
		bin = "gh"
	}
	return &GhOpener{Bin: bin}
}

// Open creates a draft pull request and returns its URL.
//
// The PR is always a draft: overseer converges the loop, but a human still
// reviews before anything merges.
//
// Open is idempotent. A daemon that exits after gh succeeded but before the
// URL was persisted will call Open again for the same branch, so an existing
// open pull request is returned rather than a duplicate being attempted.
func (g *GhOpener) Open(ctx context.Context, req PRRequest) (string, error) {
	if url, ok := g.existing(ctx, req); ok {
		return url, nil
	}
	args := []string{
		"pr", "create", "--draft",
		"--head", req.Worktree.Branch,
		"--base", req.BaseBranch,
		"--title", req.Title,
		"--body", req.Body,
	}
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	cmd.Dir = req.Worktree.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w: %s", err, text)
	}
	// gh prints the PR URL on its own line, sometimes after warnings.
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http") {
			return line, nil
		}
	}
	return text, nil
}

// existing reports the URL of an open pull request already targeting this
// branch. A non-zero exit means there is none, which is the common case and
// not an error.
func (g *GhOpener) existing(ctx context.Context, req PRRequest) (string, bool) {
	cmd := exec.CommandContext(ctx, g.Bin, "pr", "view", req.Worktree.Branch,
		"--json", "url", "--jq", ".url")
	cmd.Dir = req.Worktree.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	url := strings.TrimSpace(string(out))
	if !strings.HasPrefix(url, "http") {
		return "", false
	}
	return url, true
}

// FakeOpener records requests instead of opening pull requests.
//
// The real engine reaches the pull-request step from several task workers at
// once, so this must be safe for concurrent use — otherwise the first test that
// finishes two tasks in parallel trips the race detector on the recording
// itself rather than on anything it is meant to be testing.
type FakeOpener struct {
	mu    sync.Mutex
	Calls []PRRequest
	URL   string
	Err   error
}

// Open records the request and returns the configured URL or error.
func (f *FakeOpener) Open(_ context.Context, req PRRequest) (string, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, req)
	err := f.Err
	url := f.URL
	f.mu.Unlock()

	if err != nil {
		return "", err
	}
	return url, nil
}

// Recorded returns a copy of the requests seen so far. Tests that finish tasks
// in parallel must read through this rather than touching Calls directly.
func (f *FakeOpener) Recorded() []PRRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PRRequest(nil), f.Calls...)
}

// PRTitle derives a PR title from a task goal: its first line, capped to 72
// characters.
func PRTitle(goal string) string {
	title := strings.TrimSpace(goal)
	if i := strings.IndexByte(title, '\n'); i >= 0 {
		title = strings.TrimSpace(title[:i])
	}
	if len(title) > 72 {
		title = strings.TrimSpace(title[:69]) + "..."
	}
	if title == "" {
		return "overseer task"
	}
	return title
}

// PRBody assembles the PR description from the task goal, the converged
// plan, and the final Codex review.
func PRBody(goal, plan, review string) string {
	var b strings.Builder
	b.WriteString("## Goal\n\n")
	b.WriteString(strings.TrimSpace(goal))
	b.WriteString("\n\n")

	if p := strings.TrimSpace(plan); p != "" {
		b.WriteString("## Plan\n\n")
		b.WriteString(p)
		b.WriteString("\n\n")
	}
	if r := strings.TrimSpace(review); r != "" {
		b.WriteString("## Final Codex review\n\n")
		b.WriteString(r)
		b.WriteString("\n\n")
	}
	b.WriteString("---\n\nOpened by overseer. The plan and the code each ")
	b.WriteString("converged to zero blocking findings from Codex before this ")
	b.WriteString("pull request was created.\n")
	return b.String()
}
