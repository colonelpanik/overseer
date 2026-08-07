package agent

import (
	"encoding/json"
	"testing"
)

func TestParseVerdictWithNullOptionalFields(t *testing.T) {
	raw := `{"verdict":"changes_requested","findings":[
		{"severity":"major","summary":"error discarded","file":"main.go","line":12},
		{"severity":"nit","summary":"rename tmp","file":null,"line":null}]}`
	v, err := ParseVerdict([]byte(raw))
	if err != nil {
		t.Fatalf("ParseVerdict: %v", err)
	}
	if len(v.Findings) != 2 {
		t.Fatalf("Findings = %d, want 2", len(v.Findings))
	}
	if v.Findings[0].FileOrEmpty() != "main.go" || v.Findings[0].LineOrZero() != 12 {
		t.Errorf("finding 0 = %+v", v.Findings[0])
	}
	if v.Findings[1].FileOrEmpty() != "" || v.Findings[1].LineOrZero() != 0 {
		t.Errorf("null file/line must degrade to empty/zero, got %+v", v.Findings[1])
	}
}

func TestParseVerdictRejectsUnknownSeverity(t *testing.T) {
	raw := `{"verdict":"changes_requested","findings":[{"severity":"blocker","summary":"x"}]}`
	if _, err := ParseVerdict([]byte(raw)); err == nil {
		t.Fatal("unknown severity must be an error, not silently ranked")
	}
}

func TestParseVerdictRejectsUnknownVerdict(t *testing.T) {
	raw := `{"verdict":"lgtm","findings":[]}`
	if _, err := ParseVerdict([]byte(raw)); err == nil {
		t.Fatal("unknown verdict value must be an error")
	}
}

func TestParseVerdictRejectsMalformed(t *testing.T) {
	if _, err := ParseVerdict([]byte(`I reviewed it and it looks fine!`)); err == nil {
		t.Fatal("prose must be an error; unparseable output is never approval")
	}
}

func TestParseVerdictRequiresBothFieldsToBePresent(t *testing.T) {
	// This is the silent-approval hole: a missing or null "findings" decodes
	// to zero findings, and zero findings means converged. Every one of these
	// must be an error.
	cases := map[string]string{
		"missing findings":  `{"verdict":"approved"}`,
		"null findings":     `{"verdict":"approved","findings":null}`,
		"missing verdict":   `{"findings":[]}`,
		"null verdict":      `{"verdict":null,"findings":[]}`,
		"both missing":      `{}`,
		"changes, no array": `{"verdict":"changes_requested"}`,
	}
	for name, raw := range cases {
		if v, err := ParseVerdict([]byte(raw)); err == nil {
			t.Errorf("%s: parsed as verdict %q with %d findings; want an error",
				name, v.Verdict, len(v.Findings))
		}
	}
}

func TestParseVerdictRejectsChangesRequestedWithNoFindings(t *testing.T) {
	// Self-contradictory: the reviewer wants changes but named none.
	// Converging would approve work it said was not ready.
	raw := `{"verdict":"changes_requested","findings":[]}`
	if _, err := ParseVerdict([]byte(raw)); err == nil {
		t.Fatal("changes_requested with an empty findings array must be an error")
	}
}

func TestParseVerdictRejectsTrailingData(t *testing.T) {
	cases := map[string]string{
		"trailing prose": `{"verdict":"approved","findings":[]} looks good!`,
		"second object":  `{"verdict":"approved","findings":[]}{"verdict":"changes_requested","findings":[]}`,
		"trailing brace": `{"verdict":"approved","findings":[]}}`,
	}
	for name, raw := range cases {
		if _, err := ParseVerdict([]byte(raw)); err == nil {
			t.Errorf("%s: parsed without error; trailing data must be rejected", name)
		}
	}
}

func TestParseVerdictExtractsFromFencedBlock(t *testing.T) {
	// Belt and braces: if a future CLI wraps the final message in a fence,
	// the JSON is still recoverable.
	raw := "```json\n{\"verdict\":\"approved\",\"findings\":[]}\n```"
	v, err := ParseVerdict([]byte(raw))
	if err != nil {
		t.Fatalf("ParseVerdict: %v", err)
	}
	if v.Verdict != "approved" {
		t.Errorf("Verdict = %q, want approved", v.Verdict)
	}
}

func TestBlockingRespectsThreshold(t *testing.T) {
	v := Verdict{Verdict: "changes_requested", Findings: []Finding{
		{Severity: SevNit, Summary: "n"},
		{Severity: SevMinor, Summary: "m"},
		{Severity: SevMajor, Summary: "M"},
		{Severity: SevCritical, Summary: "C"},
	}}
	cases := map[string]int{"any": 4, "minor": 3, "major": 2, "critical": 1}
	for threshold, want := range cases {
		if got := len(v.Blocking(threshold)); got != want {
			t.Errorf("Blocking(%q) = %d findings, want %d", threshold, got, want)
		}
	}
}

func TestApprovedVerdictWithFindingsStillBlocks(t *testing.T) {
	// The findings array is authoritative; the verdict enum is advisory.
	v := Verdict{Verdict: "approved", Findings: []Finding{
		{Severity: SevMajor, Summary: "still broken"},
	}}
	if len(v.Blocking("any")) != 1 {
		t.Fatal("approved verdict with findings must still block")
	}
}

func TestFingerprintIsStableAndOrderInsensitive(t *testing.T) {
	a := Verdict{Findings: []Finding{
		{Severity: SevMajor, Summary: "one"},
		{Severity: SevNit, Summary: "two"},
	}}
	b := Verdict{Findings: []Finding{
		{Severity: SevNit, Summary: "two"},
		{Severity: SevMajor, Summary: "one"},
	}}
	if a.Fingerprint("any") != b.Fingerprint("any") {
		t.Error("fingerprint must ignore finding order")
	}

	c := Verdict{Findings: []Finding{{Severity: SevMajor, Summary: "different"}}}
	if a.Fingerprint("any") == c.Fingerprint("any") {
		t.Error("different findings must fingerprint differently")
	}
	if (Verdict{}).Fingerprint("any") == "" {
		t.Error("empty verdict must still produce a fingerprint")
	}
}

func TestVerdictSchemaIsStrictModeValid(t *testing.T) {
	// Strict mode requires every key in properties to appear in required.
	// This test is the guard against reintroducing the HTTP 400.
	var schema map[string]any
	if err := json.Unmarshal(VerdictSchema, &schema); err != nil {
		t.Fatalf("embedded schema is not valid JSON: %v", err)
	}
	assertAllPropsRequired(t, schema, "root")
}

func assertAllPropsRequired(t *testing.T, node map[string]any, path string) {
	t.Helper()
	props, hasProps := node["properties"].(map[string]any)
	if hasProps {
		required := map[string]bool{}
		reqList, _ := node["required"].([]any)
		for _, r := range reqList {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
		for name := range props {
			if !required[name] {
				t.Errorf("%s: property %q is not in required (strict mode rejects this)", path, name)
			}
		}
		for name, child := range props {
			if m, ok := child.(map[string]any); ok {
				assertAllPropsRequired(t, m, path+"."+name)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		assertAllPropsRequired(t, items, path+".items")
	}
}

func TestFingerprintIgnoresDetail(t *testing.T) {
	// Detail exists to hold volatile content — command output, timings — that
	// must not perturb the fingerprint. If it were hashed, a repeated failure
	// would look new and oscillation detection would never fire.
	base := Finding{Severity: SevCritical, Summary: "make test failed"}
	withDetail := base
	withDetail.Detail = "ran in 1.482s at /tmp/build-9182"
	other := base
	other.Detail = "ran in 9.001s at /tmp/build-1"

	a := Verdict{Findings: []Finding{withDetail}}
	b := Verdict{Findings: []Finding{other}}
	if a.Fingerprint("any") != b.Fingerprint("any") {
		t.Error("Detail changed the fingerprint; it must be excluded")
	}

	// Summary still must matter.
	c := Verdict{Findings: []Finding{{Severity: SevCritical, Summary: "different"}}}
	if a.Fingerprint("any") == c.Fingerprint("any") {
		t.Error("Summary no longer affects the fingerprint")
	}
}
