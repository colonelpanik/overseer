package engine

import (
	"strings"
	"testing"

	"overseer/internal/agent"
)

func TestPlanPromptForbidsCodeAndNamesPlanFile(t *testing.T) {
	p := PlanPrompt("Add CSV export", "No new JS dependencies")
	for _, want := range []string{"Add CSV export", "No new JS dependencies", "PLAN.md"} {
		if !strings.Contains(p, want) {
			t.Errorf("PlanPrompt missing %q:\n%s", want, p)
		}
	}
	if !strings.Contains(strings.ToLower(p), "do not") {
		t.Error("PlanPrompt must forbid writing code")
	}
}

func TestPlanPromptOmitsEmptyConstraintsSection(t *testing.T) {
	p := PlanPrompt("goal", "")
	if strings.Contains(p, "CONSTRAINTS") {
		t.Errorf("empty constraints must not produce a dangling heading:\n%s", p)
	}
}

func TestReviewPromptsDemandTheSchemaAndNothingElse(t *testing.T) {
	for name, p := range map[string]string{
		"plan": PlanReviewPrompt("Add CSV export"),
		"code": CodeReviewPrompt("Add CSV export", "origin/main"),
	} {
		if !strings.Contains(p, "findings") || !strings.Contains(p, "verdict") {
			t.Errorf("%s review prompt does not name the schema fields:\n%s", name, p)
		}
		if !strings.Contains(p, "Add CSV export") {
			t.Errorf("%s review prompt omits the goal", name)
		}
	}
	if !strings.Contains(CodeReviewPrompt("g", "origin/main"), "origin/main") {
		t.Error("code review prompt must name the base ref so Codex diffs the right range")
	}
	if !strings.Contains(PlanReviewPrompt("g"), "PLAN.md") {
		t.Error("plan review prompt must name PLAN.md")
	}
}

func TestReviseWithFindingsRendersEverySeverityAndLocation(t *testing.T) {
	file := "main.go"
	line := 12
	findings := []agent.Finding{
		{Severity: agent.SevMajor, Summary: "error from os.Open discarded", File: &file, Line: &line},
		{Severity: agent.SevNit, Summary: "rename tmp to buf"},
	}
	p := ReviseWithFindingsPrompt("PLAN.md", findings, nil)
	for _, want := range []string{
		"major", "error from os.Open discarded", "main.go", "12",
		"nit", "rename tmp to buf", "PLAN.md",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestReviseWithFindingsHandlesNoLocation(t *testing.T) {
	p := ReviseWithFindingsPrompt("the code", []agent.Finding{
		{Severity: agent.SevMajor, Summary: "no tests"},
	}, nil)
	if strings.Contains(p, ":0") {
		t.Errorf("a finding with no line must not render as \":0\":\n%s", p)
	}
}

// A first-round finding must read exactly as it did before recurrence markers
// existed: the marker is the signal, and a prompt that shouts on every finding
// says nothing.
func TestReviseWithFindingsSaysNothingAboutAFirstTimeFinding(t *testing.T) {
	raised := map[string]int{"no tests": 1}
	p := ReviseWithFindingsPrompt("the code", []agent.Finding{
		{Severity: agent.SevMajor, Summary: "no tests"},
	}, raised)
	for _, unwanted := range []string{"ALREADY RAISED", "come back", "did not fix"} {
		if strings.Contains(p, unwanted) {
			t.Errorf("a first-round finding must not be marked as recurring (%q):\n%s", unwanted, p)
		}
	}
}

// The whole point of the change: the second request must not be worded like
// the first, or the likeliest reply is the same one.
func TestReviseWithFindingsMarksAFindingThatHasComeBack(t *testing.T) {
	raised := map[string]int{"error from os.Open discarded": 2}
	p := ReviseWithFindingsPrompt("the code", []agent.Finding{
		{Severity: agent.SevMajor, Summary: "error from os.Open discarded"},
		{Severity: agent.SevNit, Summary: "rename tmp to buf"},
	}, raised)

	if !strings.Contains(p, "ALREADY RAISED") {
		t.Errorf("the recurring finding is not marked:\n%s", p)
	}
	if !strings.Contains(p, "round 2") {
		t.Errorf("the marker does not say which round this is:\n%s", p)
	}
	// The count in the closing paragraph is of recurring findings, not of all
	// of them: "2 of these have come back" on a list where one has is the kind
	// of wrongness that makes an agent distrust the rest of the prompt.
	if !strings.Contains(p, "One of these has come back") {
		t.Errorf("the closing paragraph miscounts the repeats:\n%s", p)
	}
	// It has to say the previous approach failed, not merely that the finding
	// is old — "try again" is the instruction that produces the same answer.
	if !strings.Contains(p, "did not fix it") {
		t.Errorf("the prompt does not tell the agent its last attempt failed:\n%s", p)
	}
}

// Findings are matched on the trimmed summary, because that is the form the
// store counts and a reviewer's stray whitespace must not hide a repeat.
func TestReviseWithFindingsMatchesOnTheTrimmedSummary(t *testing.T) {
	raised := map[string]int{"no tests": 3}
	p := ReviseWithFindingsPrompt("the code", []agent.Finding{
		{Severity: agent.SevMajor, Summary: "  no tests\n"},
	}, raised)
	if !strings.Contains(p, "round 3") {
		t.Errorf("whitespace around a summary hid the recurrence:\n%s", p)
	}
}

func TestExecPromptPointsAtThePlan(t *testing.T) {
	p := ExecPrompt("Add CSV export")
	if !strings.Contains(p, "PLAN.md") {
		t.Errorf("ExecPrompt must point at PLAN.md:\n%s", p)
	}
	if !strings.Contains(p, "Add CSV export") {
		t.Error("ExecPrompt must restate the goal")
	}
}

func TestAnalysisPromptAsksForASubjectAndSaysWhatMakesOne(t *testing.T) {
	p := AnalysisPrompt(nil, "", "", 8)
	for _, want := range []string{`"subject"`, "72 characters", `"goal"`} {
		if !strings.Contains(p, want) {
			t.Errorf("the analysis prompt does not mention %q", want)
		}
	}
	// The schema is embedded verbatim, so the field has to be in it too — the
	// model reads that as the contract.
	if !strings.Contains(p, `"subject": {`) {
		t.Error("the embedded schema does not describe subject")
	}
}

func TestArchitectAcceptPromptAsksForASubject(t *testing.T) {
	// The architect's list reaches the same rows through the same parser, so a
	// subject it does not know to write is a subject nothing else can supply.
	p := ArchitectAcceptPrompt(true, 8)
	if !strings.Contains(p, `"subject"`) {
		t.Error("the accept prompt does not ask for a subject")
	}
	if !strings.Contains(p, "72 characters") {
		t.Error("the accept prompt does not bound the subject")
	}
}
