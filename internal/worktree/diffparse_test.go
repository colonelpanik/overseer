package worktree

import "testing"

const sampleDiff = `diff --git a/internal/web/export.go b/internal/web/export.go
index 1a2b3c4..5d6e7f8 100644
--- a/internal/web/export.go
+++ b/internal/web/export.go
@@ -61,7 +61,8 @@ func (s *Server) racks(w http.ResponseWriter) {
 func (s *Server) exportInventory(w http.ResponseWriter, r *http.Request) {
-	rows, err := s.store.AllRacks(r.Context())
+	filter := parseRackFilter(r.URL.Query())
+	rows, err := s.store.RacksMatching(r.Context(), filter)
 	if err != nil {
 		return
 	}
diff --git a/internal/web/export_test.go b/internal/web/export_test.go
new file mode 100644
index 0000000..9999999
--- /dev/null
+++ b/internal/web/export_test.go
@@ -0,0 +1,2 @@
+package web
+
`

func TestParseUnifiedDiffTracksLineNumbers(t *testing.T) {
	files := ParseUnifiedDiff(sampleDiff)
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}

	got := files[0]
	if got.Path != "internal/web/export.go" {
		t.Errorf("path = %q, want internal/web/export.go", got.Path)
	}
	if got.Added != 2 || got.Removed != 1 {
		t.Errorf("stat = +%d −%d, want +2 −1", got.Added, got.Removed)
	}

	// The hunk starts at 61 on both sides; the first context line is 61/61,
	// the removed line takes old 62, and the two added lines take new 62 and
	// 63. Getting this wrong is what makes a finding anchor to the wrong row.
	want := []DiffLine{
		{Kind: "hunk", Text: "@@ -61,7 +61,8 @@ func (s *Server) racks(w http.ResponseWriter) {"},
		{Kind: "ctx", A: 61, B: 61, Text: "func (s *Server) exportInventory(w http.ResponseWriter, r *http.Request) {"},
		{Kind: "del", A: 62, Text: "\trows, err := s.store.AllRacks(r.Context())"},
		{Kind: "add", B: 62, Text: "\tfilter := parseRackFilter(r.URL.Query())"},
		{Kind: "add", B: 63, Text: "\trows, err := s.store.RacksMatching(r.Context(), filter)"},
		{Kind: "ctx", A: 63, B: 64, Text: "\tif err != nil {"},
		{Kind: "ctx", A: 64, B: 65, Text: "\t\treturn"},
		{Kind: "ctx", A: 65, B: 66, Text: "\t}"},
	}
	if len(got.Lines) != len(want) {
		t.Fatalf("lines = %d, want %d: %+v", len(got.Lines), len(want), got.Lines)
	}
	for i := range want {
		if got.Lines[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, got.Lines[i], want[i])
		}
	}
}

func TestParseUnifiedDiffNamesACreatedFile(t *testing.T) {
	files := ParseUnifiedDiff(sampleDiff)
	// The old side is /dev/null, so the name has to come off the new side.
	if got := files[1].Path; got != "internal/web/export_test.go" {
		t.Errorf("path = %q, want internal/web/export_test.go", got)
	}
	if got := files[1].Stat(); got != "+2 −0" {
		t.Errorf("stat = %q, want +2 −0", got)
	}
}

func TestParseUnifiedDiffIgnoresTrailingNewline(t *testing.T) {
	// A trailing newline splits into an empty final element. Reading that as
	// a context line would add a phantom row to the last file.
	with := ParseUnifiedDiff(sampleDiff)
	without := ParseUnifiedDiff(sampleDiff[:len(sampleDiff)-1])
	if len(with[1].Lines) != len(without[1].Lines) {
		t.Errorf("trailing newline changed line count: %d vs %d",
			len(with[1].Lines), len(without[1].Lines))
	}
}

func TestHunkStartsIgnoresTheSectionHeading(t *testing.T) {
	// The text after the closing @@ is the enclosing declaration, and it
	// regularly contains tokens that start with a minus.
	a, b := hunkStarts("@@ -0,0 +1,13 @@ func f(x int) { // step -1 handling")
	if a != 0 || b != 1 {
		t.Errorf("starts = %d/%d, want 0/1", a, b)
	}
}

func TestParseUnifiedDiffTruncatesAHugeFile(t *testing.T) {
	raw := "diff --git a/big.txt b/big.txt\n--- a/big.txt\n+++ b/big.txt\n@@ -1,1 +1,9999 @@\n"
	for i := 0; i < maxDiffLines+50; i++ {
		raw += "+line\n"
	}
	files := ParseUnifiedDiff(raw)
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	if !files[0].Truncated {
		t.Error("Truncated = false, want true")
	}
	if len(files[0].Lines) > maxDiffLines {
		t.Errorf("kept %d lines, want at most %d", len(files[0].Lines), maxDiffLines)
	}
	// The counts describe the whole change, not the kept prefix, so the file
	// tab does not understate a diff the panel had to cut short.
	if files[0].Added != maxDiffLines+50 {
		t.Errorf("Added = %d, want %d", files[0].Added, maxDiffLines+50)
	}
}
