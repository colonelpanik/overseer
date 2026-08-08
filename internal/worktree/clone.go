package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// scpLike matches git's abbreviated ssh syntax, user@host:path.
//
// It is anchored and deliberately narrow: the host must look like a hostname
// and the path must not start with a slash, which is what separates
// "git@github.com:acme/widget.git" from a local path that happens to contain a
// colon.
var scpLike = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:[^/].*$`)

// ValidateCloneURL accepts only the transports overseer is willing to fetch
// over, and rejects everything else.
//
// This matters more than it looks. `git clone` supports transports that
// execute a command the URL names — `ext::sh -c ...` is the clearest, and
// `file://` combined with a repository carrying its own hooks is another way
// in. The daemon runs this on an operator's machine with that operator's
// credentials, so the set of schemes has to be an allowlist, not a
// denylist of the ones known to be dangerous today.
//
// A URL that starts with "-" is refused separately: git would read it as an
// option, and no amount of quoting inside the argv fixes that. Callers must
// also pass the URL after "--", which this package does.
func ValidateCloneURL(raw string) error {
	url := strings.TrimSpace(raw)
	if url == "" {
		return fmt.Errorf("no repository URL given")
	}
	if strings.HasPrefix(url, "-") {
		return fmt.Errorf("repository URL %q starts with a dash, which git would read as an option", url)
	}
	// Control characters and whitespace have no place in a URL and are a
	// standard way to smuggle a second argument past a naive check.
	if strings.ContainsAny(url, " \t\n\r\x00") {
		return fmt.Errorf("repository URL contains whitespace or a control character")
	}

	switch {
	case strings.HasPrefix(url, "https://"), strings.HasPrefix(url, "ssh://"):
		return nil
	case scpLike.MatchString(url):
		return nil
	}
	return fmt.Errorf("repository URL %q is not an https:// or ssh:// address; "+
		"other git transports can run commands the URL names, so they are refused", url)
}

// CloneName derives the directory name a URL should be cloned into.
func CloneName(url string) string {
	name := strings.TrimSuffix(strings.TrimSpace(url), "/")
	if i := strings.LastIndexAny(name, "/:"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ".git")
	if s := Slugify(name); s != "" {
		return s
	}
	return "imported"
}

// Clone fetches url into dest and returns the path it ended up at.
//
// An existing clone is reused when its origin is the same URL, so re-running
// the wizard against a repository already imported does not re-download it.
// A directory whose origin is something else is an error rather than a silent
// overwrite: it is somebody's work, and the operator has to say what to do
// with it.
//
// The clone is not shallow. The engine later cuts worktrees from
// origin/<default branch> and pushes them, and both are fragile against a
// depth-1 clone — saving a download here would cost a confusing failure much
// later, in a task that has already been paid for.
func Clone(ctx context.Context, url, dest string) (string, error) {
	if err := ValidateCloneURL(url); err != nil {
		return "", err
	}
	return cloneInto(ctx, strings.TrimSpace(url), dest)
}

// cloneInto is Clone with the transport check already done. It is separate so
// the destination handling can be exercised against a filesystem fixture,
// which the allowlist would otherwise refuse before reaching this code.
func cloneInto(ctx context.Context, url, dest string) (string, error) {
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		existing, err := git(ctx, dest, "remote", "get-url", "origin")
		if err != nil {
			return "", fmt.Errorf("%s already exists and is not a clone of %s", dest, url)
		}
		if strings.TrimSpace(existing) != url {
			return "", fmt.Errorf("%s is already a clone of %s, not %s", dest, existing, url)
		}
		return dest, nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("create import dir: %w", err)
	}
	// "--" ends option parsing, so neither the URL nor the destination can be
	// read as a flag whatever they contain.
	if _, err := git(ctx, filepath.Dir(dest), "clone", "--", url, dest); err != nil {
		// A failed clone can leave a partial directory behind, which would
		// then look like an existing clone on the next attempt.
		os.RemoveAll(dest)
		return "", fmt.Errorf("clone %s: %w", url, err)
	}
	return dest, nil
}
