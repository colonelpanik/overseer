package worktree

import (
	"context"
	"strconv"
	"strings"
)

// maxDiffLines bounds how much of one file's diff is kept. The dashboard
// renders every line as its own grid row, and a generated file with a
// thousand-line change would make the page unusable rather than informative.
// The remainder is dropped and the file is marked truncated.
const maxDiffLines = 2000

// DiffLine is one row of a unified diff, carrying the line numbers on both
// sides so the dashboard can anchor a finding recorded as file:line.
type DiffLine struct {
	// Kind is "ctx", "add", "del" or "hunk".
	Kind string
	// A is the line number on the old side, zero for an added line.
	A int
	// B is the line number on the new side, zero for a removed line.
	B int
	// Text is the line without its leading +/-/space marker.
	Text string
}

// FileDiff is one file's worth of a unified diff.
type FileDiff struct {
	Path      string
	Added     int
	Removed   int
	Binary    bool
	Truncated bool
	Lines     []DiffLine
}

// Stat renders the +/- summary shown on the file tab.
func (f FileDiff) Stat() string {
	return "+" + strconv.Itoa(f.Added) + " −" + strconv.Itoa(f.Removed)
}

// ParseUnifiedDiff turns `git diff` output into per-file line lists.
//
// It is deliberately a plain text parser over the porcelain-stable parts of
// the format (the ---/+++ and @@ lines): the alternative, one `git` call per
// file, would multiply the process count by the size of the change for a page
// that is re-rendered on every state event.
func ParseUnifiedDiff(raw string) []FileDiff {
	var files []FileDiff
	var cur *FileDiff
	var a, b int

	flush := func() {
		if cur != nil {
			files = append(files, *cur)
		}
		cur = nil
	}

	// A trailing newline would otherwise split into one empty final element,
	// which the in-hunk blank-line case below would read as real context.
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &FileDiff{Path: gitHeaderPath(line)}
			a, b = 0, 0

		case cur == nil:
			// Preamble before the first file header; nothing to attach it to.
			continue

		case strings.HasPrefix(line, "--- "):
			if p, ok := diffPath(line, "--- "); ok && cur.Path == "" {
				cur.Path = p
			}
		case strings.HasPrefix(line, "+++ "):
			// The new-side path wins when both exist: it is the name the file
			// has after the change, which is what the reviewer cited.
			if p, ok := diffPath(line, "+++ "); ok {
				cur.Path = p
			}

		case strings.HasPrefix(line, "@@"):
			a, b = hunkStarts(line)
			cur.append(DiffLine{Kind: "hunk", Text: line})

		case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
			cur.Binary = true

		case strings.HasPrefix(line, "+"):
			cur.Added++
			cur.append(DiffLine{Kind: "add", B: b, Text: line[1:]})
			b++

		case strings.HasPrefix(line, "-"):
			cur.Removed++
			cur.append(DiffLine{Kind: "del", A: a, Text: line[1:]})
			a++

		case strings.HasPrefix(line, " "):
			cur.append(DiffLine{Kind: "ctx", A: a, B: b, Text: line[1:]})
			a++
			b++

		case line == "":
			// A hunk's trailing blank context line loses its leading space in
			// some pipelines; inside a hunk, treat it as context.
			if a > 0 || b > 0 {
				cur.append(DiffLine{Kind: "ctx", A: a, B: b})
				a++
				b++
			}
		}
		// "\ No newline at end of file", "index", "new file mode", "similarity
		// index" and friends fall through with no case and are dropped.
	}
	flush()
	return files
}

func (f *FileDiff) append(l DiffLine) {
	if len(f.Lines) >= maxDiffLines {
		f.Truncated = true
		return
	}
	f.Lines = append(f.Lines, l)
}

// gitHeaderPath pulls the new-side path out of `diff --git a/x b/x`. It is a
// fallback for the ---/+++ pair, which is parsed in preference because a path
// containing " b/" defeats this one.
func gitHeaderPath(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	i := strings.Index(rest, " b/")
	if i < 0 {
		return ""
	}
	return strings.TrimPrefix(rest[i+1:], "b/")
}

// diffPath reads the path from a --- or +++ header, reporting false for
// /dev/null, which marks a created or deleted file rather than naming one.
func diffPath(line, prefix string) (string, bool) {
	p := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	// git appends a tab and metadata when the path contains whitespace.
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if p == "/dev/null" || p == "" {
		return "", false
	}
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	return p, true
}

// hunkStarts reads the two starting line numbers out of `@@ -a,b +c,d @@`.
//
// It stops at the first -/+ field of each sign rather than taking the last,
// because git appends the enclosing function's signature after the closing
// @@, and that text routinely contains tokens starting with a minus.
func hunkStarts(line string) (int, int) {
	var a, b int
	var gotA, gotB bool
	for _, f := range strings.Fields(line) {
		switch {
		case !gotA && strings.HasPrefix(f, "-"):
			a, gotA = leadingInt(f[1:]), true
		case !gotB && strings.HasPrefix(f, "+"):
			b, gotB = leadingInt(f[1:]), true
		}
		if gotA && gotB {
			break
		}
	}
	return a, b
}

func leadingInt(s string) int {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// ParsedDiff returns the worktree's accumulated diff against its base ref,
// already split into files and lines.
func (m *Manager) ParsedDiff(ctx context.Context, wt Worktree) ([]FileDiff, error) {
	raw, err := m.Diff(ctx, wt)
	if err != nil {
		return nil, err
	}
	return ParseUnifiedDiff(raw), nil
}
