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

// AnalysisPrompt asks for a task list for a repository nobody has written one
// for yet.
//
// Three things it insists on, each because the alternative is a task list that
// looks useful and is not. Every proposal must cite what it read, so a
// proposal can be judged rather than taken on faith. Every goal must be one
// self-contained change, because "improve error handling" is not something a
// plan-review-execute loop can converge on. And the response must be the JSON
// object alone: the Claude CLI has no --output-schema, so the schema below is
// documentation, and the parser on the other side rejects anything else.
func AnalysisPrompt(focus []string, notes, detected string, maxTasks int) string {
	var b strings.Builder
	b.WriteString(`You are proposing a queue of independent work items for this repository.
An automated system will carry each one out on its own branch, in its own
worktree, reviewed by a second agent until it converges, and open a draft pull
request. A human reviews those pull requests. Nothing you propose runs without
that human queueing it first.

Read enough of the repository to be specific. Start with the README, the build
or package manifest, the test layout and the most-changed parts of the tree.
This checkout is READ-ONLY: you cannot edit, commit, or run anything that
writes. Do not try.

`)
	if detected = strings.TrimSpace(detected); detected != "" {
		b.WriteString("WHAT THE DAEMON ALREADY DETECTED:\n")
		b.WriteString(detected)
		b.WriteString("\n\n")
	}
	if len(focus) > 0 {
		b.WriteString("THE OPERATOR ASKED YOU TO FOCUS ON:\n")
		for _, f := range focus {
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(f))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if n := strings.TrimSpace(notes); n != "" {
		b.WriteString("ADDITIONAL INSTRUCTIONS FROM THE OPERATOR:\n")
		b.WriteString(n)
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, `Propose at most %d tasks. Fewer, well-founded tasks beat a long list.

Each task must be:

- ONE self-contained change an agent can finish and a reviewer can judge. Not a
  theme, not an area of the codebase, not "audit X". If you cannot describe the
  diff you expect, it is not a task.
- Grounded in something you actually read. "rationale" says why in one
  sentence and "evidence" lists the file:line references behind it. A task you
  cannot cite is a task you guessed at, and you should drop it instead.
- Carried by this repository's own conventions. Put those in "constraints" —
  the patterns a change here has to follow, in this repository's terms.

Set "verify" to the command that would prove the task is done in this
repository — the test command you found, not one you assume. Use null only if
this repository genuinely has none.

Set "blocking_severity" to how strict the review loop should be. Use "any" by
default; use "major" for a task where style nits would waste iterations.

Use "depends_on" only for a real ordering constraint, naming the "key" of a
task EARLIER in your array. Never name a later task and never form a cycle.

Reply with a single JSON object matching this schema and nothing else — no
prose before it, no explanation after it, no markdown fence:

%s`, maxTasks, strings.TrimSpace(string(agent.ProposalSchema)))
	return b.String()
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
