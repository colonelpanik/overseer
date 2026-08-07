package agent

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed verdict.schema.json
var schemaFS embed.FS

// VerdictSchema is the JSON Schema passed to codex exec --output-schema.
var VerdictSchema = mustReadSchema()

func mustReadSchema() []byte {
	b, err := schemaFS.ReadFile("verdict.schema.json")
	if err != nil {
		panic("embedded verdict schema missing: " + err.Error())
	}
	return b
}

// WriteVerdictSchema materialises the embedded schema in dir and returns its
// path, because codex exec takes a file rather than inline JSON.
func WriteVerdictSchema(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create schema dir: %w", err)
	}
	path := filepath.Join(dir, "verdict.schema.json")
	if err := os.WriteFile(path, VerdictSchema, 0o644); err != nil {
		return "", fmt.Errorf("write schema: %w", err)
	}
	return path, nil
}

// Severity ranks a finding's importance.
type Severity string

const (
	SevNit      Severity = "nit"
	SevMinor    Severity = "minor"
	SevMajor    Severity = "major"
	SevCritical Severity = "critical"
)

var severityRank = map[Severity]int{
	SevNit: 1, SevMinor: 2, SevMajor: 3, SevCritical: 4,
}

// thresholdRank maps a configured threshold to the minimum finding rank that
// blocks convergence. "any" admits everything, including nits.
var thresholdRank = map[string]int{
	"any": 1, "minor": 2, "major": 3, "critical": 4,
}

// Finding is one item from a Codex review. File and Line are pointers
// because the strict-mode schema sends JSON null when they do not apply.
type Finding struct {
	Severity Severity `json:"severity"`
	Summary  string   `json:"summary"`
	File     *string  `json:"file"`
	Line     *int     `json:"line"`
	// Detail is supplementary context shown to the agent but deliberately
	// EXCLUDED from Fingerprint. Verify failures put raw command output here:
	// it carries timings and temporary paths that change every run, so
	// fingerprinting it would make every failure look new and defeat
	// oscillation detection. json:"-" keeps it off the wire, so a reviewer
	// can never set it.
	Detail string `json:"-"`
}

// FileOrEmpty returns the file path, or "" when the finding is not
// file-scoped.
func (f Finding) FileOrEmpty() string {
	if f.File == nil {
		return ""
	}
	return *f.File
}

// LineOrZero returns the line number, or 0 when absent.
func (f Finding) LineOrZero() int {
	if f.Line == nil {
		return 0
	}
	return *f.Line
}

// Verdict is Codex's structured review result.
type Verdict struct {
	Verdict  string    `json:"verdict"`
	Findings []Finding `json:"findings"`
}

// verdictWire decodes the response with field presence distinguishable from
// a zero value. Plain `[]Finding` cannot tell an absent "findings" from an
// empty one, and an absent one decoding to zero findings would read as
// approval — the exact failure this parser exists to prevent.
type verdictWire struct {
	Verdict  *string    `json:"verdict"`
	Findings *[]Finding `json:"findings"`
}

// ParseVerdict decodes and validates a Codex final message.
//
// This function is the safety boundary. The CLI's --output-schema normally
// guarantees the shape, but when that enforcement fails or is bypassed,
// nothing else stands between malformed output and a silent approval.
// Every deviation is therefore an error, never a lenient default.
func ParseVerdict(b []byte) (Verdict, error) {
	body := stripFence(strings.TrimSpace(string(b)))

	var w verdictWire
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Verdict{}, fmt.Errorf("parse verdict: %w", err)
	}
	// Nothing may follow the object. Without this, a truncated retry or a
	// second concatenated object would be silently discarded, and the first
	// object taken as the whole answer.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Verdict{}, errors.New("parse verdict: unexpected data after the JSON object")
	}

	// Presence, not just type. JSON null also leaves these pointers nil, and
	// null is off-schema.
	if w.Verdict == nil {
		return Verdict{}, errors.New(`parse verdict: missing "verdict"`)
	}
	if w.Findings == nil {
		return Verdict{}, errors.New(`parse verdict: missing "findings"`)
	}

	v := Verdict{Verdict: *w.Verdict, Findings: *w.Findings}
	if v.Verdict != "approved" && v.Verdict != "changes_requested" {
		return Verdict{}, fmt.Errorf("parse verdict: unknown verdict %q", v.Verdict)
	}
	for i, f := range v.Findings {
		if _, ok := severityRank[f.Severity]; !ok {
			return Verdict{}, fmt.Errorf("parse verdict: finding %d has unknown severity %q", i, f.Severity)
		}
		if strings.TrimSpace(f.Summary) == "" {
			return Verdict{}, fmt.Errorf("parse verdict: finding %d has empty summary", i)
		}
	}
	// A reviewer asking for changes without naming any is self-contradictory.
	// Converging here would approve work the reviewer said was not ready, so
	// fail loudly and let a human look instead.
	if v.Verdict == "changes_requested" && len(v.Findings) == 0 {
		return Verdict{}, errors.New("parse verdict: changes_requested with an empty findings array")
	}
	return v, nil
}

// stripFence removes a surrounding ```json ... ``` fence if present.
func stripFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(strings.TrimSpace(s), "```")
}

// Blocking returns the findings at or above the threshold. These are the
// findings that keep the loop running.
func (v Verdict) Blocking(threshold string) []Finding {
	min, ok := thresholdRank[threshold]
	if !ok {
		min = 1 // unknown threshold behaves as "any"
	}
	var out []Finding
	for _, f := range v.Findings {
		if severityRank[f.Severity] >= min {
			out = append(out, f)
		}
	}
	return out
}

// Fingerprint hashes the blocking findings, order-independently. The loop
// compares fingerprints across iterations to detect oscillation: the same
// blocking set twice means the agent is not making progress.
//
// Detail is excluded on purpose. Anything volatile — timings, temporary
// paths, addresses — belongs there rather than in Summary, so that two
// occurrences of the same failure hash identically.
func (v Verdict) Fingerprint(threshold string) string {
	blocking := v.Blocking(threshold)
	keys := make([]string, 0, len(blocking))
	for _, f := range blocking {
		keys = append(keys, fmt.Sprintf("%s|%s|%d|%s",
			f.Severity, f.FileOrEmpty(), f.LineOrZero(), f.Summary))
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\x00")))
	return hex.EncodeToString(sum[:])
}
