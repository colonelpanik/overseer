package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
