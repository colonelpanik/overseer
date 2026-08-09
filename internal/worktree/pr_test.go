package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPRTitleUsesFirstLineAndIsCapped(t *testing.T) {
	got := PRTitle("Add CSV export to the rack inventory view\n\nMore detail here.")
	if got != "Add CSV export to the rack inventory view" {
		t.Errorf("PRTitle = %q", got)
	}
	long := PRTitle(strings.Repeat("x", 200))
	if len(long) > 72 {
		t.Errorf("PRTitle length = %d, want <= 72", len(long))
	}
}

func TestPRBodyIncludesGoalPlanAndReview(t *testing.T) {
	body := PRBody("Add CSV export", "# Plan\n1. do it\n", "No findings.")
	for _, want := range []string{"Add CSV export", "# Plan", "1. do it", "No findings.", "overseer"} {
		if !strings.Contains(body, want) {
			t.Errorf("PRBody missing %q:\n%s", want, body)
		}
	}
}

func TestPRBodyTolerantOfEmptyPlanAndReview(t *testing.T) {
	body := PRBody("goal only", "", "")
	if !strings.Contains(body, "goal only") {
		t.Errorf("PRBody dropped the goal:\n%s", body)
	}
}

func TestFakeOpenerRecordsCalls(t *testing.T) {
	f := &FakeOpener{URL: "https://example.test/pr/1"}
	var opener PROpener = f

	url, err := opener.Open(context.Background(), PRRequest{
		Worktree:   Worktree{Branch: "overseer/x", Dir: "/tmp/x"},
		Title:      "t",
		Body:       "b",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if url != "https://example.test/pr/1" {
		t.Errorf("url = %q", url)
	}
	if len(f.Calls) != 1 || f.Calls[0].Title != "t" || f.Calls[0].BaseBranch != "main" {
		t.Errorf("Calls = %+v", f.Calls)
	}
}

func TestGhOpenerBuildsDraftPRCommand(t *testing.T) {
	// A fake gh records its argv so the flags can be asserted without
	// touching GitHub. `pr view` fails first (no PR yet, as in
	// TestGhOpenerCreatesWhenNoPRExists) so Open falls through to `pr
	// create`, whose argv is what gets recorded and asserted below.
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	bin := filepath.Join(dir, "gh")
	script := "#!/bin/sh\n" +
		"if [ \"$2\" = \"view\" ]; then exit 1; fi\n" +
		"printf '%s\\n' \"$@\" > " + argvFile + "\necho https://example.test/pr/7\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	o := NewGhOpener(bin)
	url, err := o.Open(context.Background(), PRRequest{
		Worktree:   Worktree{Dir: dir, Branch: "overseer/add-csv"},
		Title:      "Add CSV export",
		Body:       "body text",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if url != "https://example.test/pr/7" {
		t.Errorf("url = %q, want the URL gh printed", url)
	}

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, want := range []string{"pr", "create", "--draft", "--head", "overseer/add-csv", "--base", "main"} {
		found := false
		for _, a := range argv {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("argv %v missing %q", argv, want)
		}
	}
}

func TestGhOpenerReturnsAnExistingPRInsteadOfCreatingADuplicate(t *testing.T) {
	// Recovery re-runs finish for a task whose PR already exists. Open must
	// report that PR rather than attempting a second create.
	dir := t.TempDir()
	createdMarker := filepath.Join(dir, "created")
	bin := filepath.Join(dir, "gh")
	script := "#!/bin/sh\n" +
		"if [ \"$2\" = \"view\" ]; then echo https://example.test/pr/existing; exit 0; fi\n" +
		"if [ \"$2\" = \"create\" ]; then touch " + createdMarker + "; echo https://example.test/pr/new; exit 0; fi\n" +
		"exit 9\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	o := NewGhOpener(bin)
	url, err := o.Open(context.Background(), PRRequest{
		Worktree: Worktree{Dir: dir, Branch: "overseer/x"}, BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if url != "https://example.test/pr/existing" {
		t.Errorf("url = %q, want the existing PR", url)
	}
	if _, err := os.Stat(createdMarker); err == nil {
		t.Error("gh pr create was called even though a PR already existed")
	}
}

func TestGhOpenerCreatesWhenNoPRExists(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh")
	// `pr view` exits non-zero when there is no PR for the branch.
	script := "#!/bin/sh\n" +
		"if [ \"$2\" = \"view\" ]; then echo 'no pull requests found' >&2; exit 1; fi\n" +
		"echo https://example.test/pr/new\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	url, err := NewGhOpener(bin).Open(context.Background(), PRRequest{
		Worktree: Worktree{Dir: dir, Branch: "overseer/x"}, BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if url != "https://example.test/pr/new" {
		t.Errorf("url = %q, want the newly created PR", url)
	}
}

func TestGhOpenerReportsFailure(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'no auth' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := NewGhOpener(bin)
	if _, err := o.Open(context.Background(), PRRequest{
		Worktree: Worktree{Dir: dir, Branch: "b"}, BaseBranch: "main",
	}); err == nil {
		t.Fatal("expected an error when gh fails")
	}
}

func TestPRBodyStaysUnderGitHubsLimit(t *testing.T) {
	// The bug this guards: PLAN.md is agent-written and routinely hundreds of
	// kilobytes. GitHub rejects a body over 65536 characters, and the task has
	// by then converged, pushed its branch and spent its whole agent budget —
	// the most expensive possible place to fail.
	hugePlan := strings.Repeat("# Task 1: do the thing\n\nSome plan prose here.\n\n", 20000)
	if len(hugePlan) < 400_000 {
		t.Fatalf("fixture too small to exercise the limit: %d bytes", len(hugePlan))
	}

	body := PRBody("Add CSV export", hugePlan, "No blocking findings remained.")

	if len(body) > 65536 {
		t.Errorf("body is %d bytes, over GitHub's 65536 limit", len(body))
	}
	// The parts a reviewer actually needs must survive.
	for _, want := range []string{"## Goal", "Add CSV export", "## Plan", "Opened by overseer"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lost %q to truncation", want)
		}
	}
	if !strings.Contains(body, "truncated by overseer") {
		t.Error("truncation happened silently, with no notice to the reader")
	}
}

func TestPRBodyLeavesNormalSizedContentIntact(t *testing.T) {
	plan := "# Plan\n\n1. Add the function\n2. Test it\n"
	review := "No blocking findings remained."
	body := PRBody("Add CSV export", plan, review)

	for _, want := range []string{plan, review, "Add CSV export"} {
		if !strings.Contains(body, want) {
			t.Errorf("ordinary content was altered; missing %q", want)
		}
	}
	if strings.Contains(body, "truncated by overseer") {
		t.Error("a small body was truncated")
	}
}

func TestPRBodyHugeReviewAndGoalAlsoBounded(t *testing.T) {
	// Codex output and a pasted goal are attacker-adjacent in size too.
	body := PRBody(strings.Repeat("g", 200_000), "", strings.Repeat("r", 200_000))
	if len(body) > 65536 {
		t.Errorf("body is %d bytes, over the limit", len(body))
	}
	if !strings.Contains(body, "Opened by overseer") {
		t.Error("the footer was lost")
	}
}

func TestPRTitleCountsRunesNotBytes(t *testing.T) {
	// 68 ASCII characters and three multi-byte ones: 71 runes, 77 bytes. The
	// subject that feeds this is bounded in runes, so a headline like this one
	// arrives untouched and is inside the budget — but a byte cap fires on it
	// and lands one byte into a three-byte character.
	headline := strings.Repeat("x", 68) + "日本語"
	got := PRTitle(headline)
	if !utf8.ValidString(got) {
		t.Errorf("PRTitle = %q, want valid UTF-8", got)
	}
	if got != headline {
		t.Errorf("PRTitle = %q, want a 71-rune headline left alone", got)
	}

	// And a headline genuinely past the budget is still cut, still cleanly.
	long := PRTitle(strings.Repeat("日", 200))
	if !utf8.ValidString(long) {
		t.Errorf("PRTitle = %q, want valid UTF-8", long)
	}
	if n := utf8.RuneCountInString(long); n > 72 {
		t.Errorf("PRTitle = %q (%d runes), want at most 72", long, n)
	}
	if !strings.HasSuffix(long, "...") {
		t.Errorf("PRTitle = %q, want an elision marker", long)
	}
}
