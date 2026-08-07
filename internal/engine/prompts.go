package engine

import (
	"fmt"
	"strings"

	"overseer/internal/agent"
)

// PlanPrompt opens the plan loop. It forbids code so the plan review has a
// plan to review rather than a finished change.
func PlanPrompt(goal, constraints string) string {
	var b strings.Builder
	b.WriteString("You are planning a change in this repository.\n\n")
	b.WriteString("GOAL:\n")
	b.WriteString(strings.TrimSpace(goal))
	b.WriteString("\n\n")
	if c := strings.TrimSpace(constraints); c != "" {
		b.WriteString("CONSTRAINTS:\n")
		b.WriteString(c)
		b.WriteString("\n\n")
	}
	b.WriteString(`Explore the repository as much as you need, then write a complete
implementation plan to PLAN.md in the repository root. The plan should name
the exact files to change, describe the tests that will prove the change
works, and call out anything about this codebase that a reviewer would need
to know to judge the approach.

Do not write, modify, or delete any code in this turn. PLAN.md is the only
file you should create. An independent reviewer will read PLAN.md next.`)
	return b.String()
}

// PlanReviewPrompt asks Codex to review the plan.
func PlanReviewPrompt(goal string) string {
	return fmt.Sprintf(`Review the implementation plan in PLAN.md against this goal.

GOAL:
%s

Read PLAN.md and enough of the repository to judge whether the plan is
correct, complete, and appropriate for this codebase. Look for wrong
assumptions about how the code works, missing steps, missing tests, and
approaches that will not survive contact with the existing design.

Respond with the JSON object required by the output schema: a "verdict" of
either "approved" or "changes_requested", and a "findings" array. Put every
problem you want addressed in "findings" with an honest severity. If the plan
is genuinely ready to implement, return "approved" with an empty "findings"
array. Do not include findings you do not want acted on.`, strings.TrimSpace(goal))
}

// ExecPrompt opens the execute loop in a fresh session seeded with the plan.
func ExecPrompt(goal string) string {
	return fmt.Sprintf(`Implement the plan in PLAN.md in this repository.

GOAL:
%s

Read PLAN.md first, then carry it out. Write tests as the plan describes and
run them. If you discover the plan is wrong about something concrete, do the
right thing and note the deviation at the bottom of PLAN.md.

Do not commit; that is handled for you. An independent reviewer will read the
resulting diff next.`, strings.TrimSpace(goal))
}

// CodeReviewPrompt asks Codex to review the accumulated diff.
func CodeReviewPrompt(goal, baseRef string) string {
	return fmt.Sprintf(`Review the changes on this branch against the goal and the plan.

GOAL:
%s

Run `+"`git diff %s...HEAD`"+` to see the full change, and read PLAN.md for the
intended approach. Judge correctness first: bugs, unhandled errors, broken
edge cases, tests that do not actually test the behaviour they name. Then
judge fit with the surrounding code.

Respond with the JSON object required by the output schema: a "verdict" of
either "approved" or "changes_requested", and a "findings" array. Put every
problem you want fixed in "findings" with an honest severity and, where it
applies, the file and line. If the change is genuinely ready, return
"approved" with an empty "findings" array. Do not include findings you do not
want acted on.`, strings.TrimSpace(goal), baseRef)
}

// ReviseWithFindingsPrompt renders a reviewer's blocking findings as the next
// turn of an existing agent session. target names what to revise, so the same
// function serves both loops.
func ReviseWithFindingsPrompt(target string, findings []agent.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "An independent reviewer examined your work and raised %d finding(s):\n\n",
		len(findings))
	for i, f := range findings {
		fmt.Fprintf(&b, "%d. [%s] ", i+1, f.Severity)
		if file := f.FileOrEmpty(); file != "" {
			b.WriteString(file)
			if line := f.LineOrZero(); line > 0 {
				fmt.Fprintf(&b, ":%d", line)
			}
			b.WriteString(" — ")
		}
		b.WriteString(strings.TrimSpace(f.Summary))
		b.WriteString("\n")
		if d := strings.TrimSpace(f.Detail); d != "" {
			// Volatile context — command output and the like. Kept out of
			// Summary so it does not perturb the fingerprint, but the agent
			// needs it to act.
			b.WriteString("\n")
			b.WriteString(d)
			b.WriteString("\n\n")
		}
	}
	fmt.Fprintf(&b, `
Address every finding in %s. Where you disagree with a finding, say so
explicitly and explain why rather than silently ignoring it — the reviewer
will see this round again.`, target)
	return b.String()
}
