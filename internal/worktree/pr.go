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

// PRTitle derives a pull request title from a task's headline: its first line,
// capped to 72 runes.
//
// Runes, not bytes, and that is not a nicety. The engine hands this an
// already-clamped subject, which store.Subject bounds to 72 RUNES — so a
// headline of 71 runes with a few multi-byte characters in it is comfortably
// over 72 bytes. A byte cap fires on a string that is already the right length
// and cuts it in the middle of a character.
//
// The clamp duplicates store.Subject's. Deliberately: these two packages sit
// side by side with no dependency between them, and worktree — git plumbing —
// importing the persistence package to shorten a string would be the wrong way
// round. agent.validSeverities duplicates config.ValidSeverities for the same
// reason and says so.
func PRTitle(headline string) string {
	title := strings.TrimSpace(headline)
	if i := strings.IndexByte(title, '\n'); i >= 0 {
		title = strings.TrimSpace(title[:i])
	}
	if r := []rune(title); len(r) > 72 {
		title = strings.TrimSpace(string(r[:69])) + "..."
	}
	if title == "" {
		return "overseer task"
	}
	return title
}

// PRBody assembles the PR description from the task goal, the converged
// plan, and the final Codex review.
func PRBody(goal, plan, review string) string {
	const footer = "---\n\nOpened by overseer. The plan and the code each " +
		"converged to zero blocking findings from Codex before this " +
		"pull request was created.\n"

	var b strings.Builder
	b.WriteString("## Goal\n\n")
	b.WriteString(truncateBody(strings.TrimSpace(goal), maxGoalBytes, "goal"))
	b.WriteString("\n\n")

	// The plan is the only unbounded input here — PLAN.md is written by an
	// agent and routinely runs to hundreds of kilobytes — so it absorbs
	// whatever budget the fixed sections leave. It is truncated rather than
	// dropped because the opening of a plan is the part a reviewer reads.
	if p := strings.TrimSpace(plan); p != "" {
		budget := maxPRBodyBytes - b.Len() - len(footer) - maxReviewBytes -
			len("## Plan\n\n\n\n") - len(truncationNotice)
		b.WriteString("## Plan\n\n")
		b.WriteString(truncateBody(p, budget, "plan"))
		b.WriteString("\n\n")
	}
	if r := strings.TrimSpace(review); r != "" {
		b.WriteString("## Final Codex review\n\n")
		b.WriteString(truncateBody(r, maxReviewBytes, "review"))
		b.WriteString("\n\n")
	}
	b.WriteString(footer)

	out := b.String()
	if len(out) > maxPRBodyBytes {
		// Belt and braces. Every section is already bounded, so reaching here
		// means an assumption above is wrong; losing the footer beats losing
		// the pull request.
		out = out[:maxPRBodyBytes-len(truncationNotice)] + truncationNotice
	}
	return out
}

// GitHub rejects a pull-request body over 65536 characters with
// "GraphQL: Body is too long". A task that hits that has already converged,
// pushed its branch, and spent its entire agent budget, so failing at the
// final step is the most expensive possible place to discover it. The margin
// leaves room for the API counting characters differently than Go counts
// bytes on non-ASCII content.
const (
	maxPRBodyBytes = 60000
	maxGoalBytes   = 4000
	maxReviewBytes = 8000
)

const truncationNotice = "\n\n_[truncated by overseer — the full text is on this branch]_\n"

// truncateBody caps a section, cutting at a line boundary where it can so the
// result stays readable markdown rather than stopping mid-token.
func truncateBody(s string, limit int, what string) string {
	if limit < len(truncationNotice)+64 {
		// No meaningful room left; say so rather than emitting a stub.
		return fmt.Sprintf("_[%s omitted by overseer — no room in the pull-request body]_", what)
	}
	if len(s) <= limit {
		return s
	}
	cut := s[:limit-len(truncationNotice)]
	if i := strings.LastIndexByte(cut, '\n'); i > len(cut)/2 {
		cut = cut[:i]
	}
	return cut + truncationNotice
}
