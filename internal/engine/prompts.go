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

// ArchitectPrompt opens the design conversation.
//
// The one prompt in this file that is not asking for a finished artefact. It
// asks for a collaborator: something that pushes back, asks what it actually
// needs to know, and says what it would do — because the operator is sitting
// there, and the whole value of the conversation is the turns where they
// disagree.
//
// It is deliberately told not to produce the task list yet. A model asked to
// design and to decompose at once does both worse, and the operator has not
// agreed to anything yet.
//
// conversation is empty on the opening turn. It is filled only when a session
// could not be resumed and the turns have to be replayed from the database, so
// both paths share one function and the framing cannot drift between a
// conversation that has been going all along and one just rebuilt.
func ArchitectPrompt(brief string, existing bool, conversation string) string {
	var b strings.Builder
	b.WriteString("You are helping a developer design something, in conversation. " +
		"They are reading your replies and will answer.\n\n")

	if existing {
		b.WriteString(`You are in an existing repository, mounted READ-ONLY. Read whatever you
need — the README, the build manifest, the parts of the tree this would touch.
You cannot edit, commit or run anything that writes, and should not try.

Ground what you say in what is actually there. "This would go in
internal/store, beside the other repo_*.go files" is worth ten sentences of
architecture.

`)
	} else {
		b.WriteString(`There is no code yet. This is a new project, and this conversation decides
what it is.

`)
	}

	b.WriteString("WHAT THEY WANT:\n")
	b.WriteString(strings.TrimSpace(brief))
	b.WriteString("\n\n")

	if c := strings.TrimSpace(conversation); c != "" {
		b.WriteString(`THE CONVERSATION SO FAR (your own session was lost, so this is the record
of it — carry on from here as if you remembered it):

`)
		b.WriteString(c)
		b.WriteString("\n\n")
	}

	b.WriteString(`How to be useful here:

- Ask about the things that actually change the design, and ask them early.
  Two or three real questions beat a checklist. If something is genuinely
  ambiguous and the answer would change the shape, ask; if you can pick a
  sensible default and say you picked it, do that instead.
- Say what you would build, concretely. Name the pieces, what each one is
  responsible for, and where the boundaries are.
- Disagree when you disagree. If what they asked for has a problem — it will
  not scale, it duplicates something, it is three projects — say so plainly
  and say what you would do instead. Agreeing with everything makes this
  conversation worthless.
- Keep it short enough to read. This is a conversation, not a document.

Do not produce a task list or a plan document yet. You will be asked for both
when they are happy with the design. Reply in prose.`)
	return b.String()
}

// ArchitectAcceptPrompt ends the conversation and asks for both artefacts.
//
// It comes as a turn in the same session, so everything already agreed is
// context rather than something to restate. The task list goes through the
// same parser and the same rules as an analysis's, because it reaches the same
// scheduler and spends the same money.
func ArchitectAcceptPrompt(existing bool, maxTasks int) string {
	var b strings.Builder
	b.WriteString(`The developer is happy with the design. Write it down and break it into
work.

Reply with a single JSON object and nothing else — no prose before it, no
explanation after it, no markdown fence:

{
  "design": "<the design, as a markdown document>",
  "tasks": [ ... ]
}

"design" is what we agreed, written for someone who was not part of this
conversation: what this is, the shape of it, the decisions that were actually
decided and why, and anything ruled out on purpose. It becomes DESIGN.md and
every task is judged against it. Do not include a task list in it.

`)
	fmt.Fprintf(&b, `"tasks" is at most %d items. Each one must be:

- ONE self-contained change an agent can finish and a reviewer can judge. Not a
  theme, not "the storage layer". If you cannot describe the diff you expect,
  it is not a task.
- Ordered by "depends_on", naming the "key" of a task EARLIER in the array.
  Never name a later task and never form a cycle.
`, maxTasks)

	if existing {
		b.WriteString(`- Grounded in this repository. Put its conventions in "constraints", set
  "verify" to the test command that actually exists here, and cite what you
  read in "evidence".
`)
	} else {
		b.WriteString(`- Written for a project that does not exist yet. The first task is the
  scaffold everything else assumes; every other task should depend on it,
  directly or through another. Put the stack and the conventions you chose in
  "constraints", so each task builds the same thing. "evidence" may be empty:
  there is nothing to cite yet. Set "verify" to the command the scaffold will
  make work.
`)
	}

	b.WriteString(`
Set "blocking_severity" to how strict the review loop should be: "any" by
default, "major" where style nits would waste iterations.

`)
	b.WriteString(strings.TrimSpace(string(agent.ProposalSchema)))
	b.WriteString("\n\nThat schema describes the \"tasks\" array. Wrap it in the object above.")
	return b.String()
}

// ScaffoldPrompt turns an agreed design into a project's first commit.
//
// The one agent turn that writes outside a worktree, and the only one that is
// not reviewed — which is why it is scoped this narrowly. It builds the thing
// every later task assumes and nothing more; the features are tasks, and they
// get the full loop.
func ScaffoldPrompt(design string) string {
	return fmt.Sprintf(`Scaffold this project. The design was agreed with the developer:

%s

Create the skeleton every later piece of work will assume:

- The directory layout, and the package or module manifest.
- Enough of an entry point to build and run, doing nothing useful yet.
- A test command that works and passes — even with one trivial test. Later
  work is gated on it, so a scaffold whose tests do not run blocks everything.
- A README saying what this is and how to build, test and run it.

Do NOT implement the features. They are separate pieces of work, each planned
and reviewed on its own branch; building them here would put them outside that.
Stop at the point where someone could clone this and start.

Do not commit; that is handled for you.`, strings.TrimSpace(design))
}

// ChatPrompt opens, or re-opens, a conversation about a repository.
//
// This is the architect's sibling and its opposite. The architect is deciding
// what to build; this one is being asked about what is already there. So where
// the architect is told to propose a shape, this is told to answer the question
// asked, cite where the answer came from, and — the part that makes it worth
// having — offer the better alternative as something the operator can say yes
// to, rather than as a lecture they have to act on.
//
// It is told not to produce a task list for the same reason the architect is:
// a model asked to answer and to decompose at once does both worse. Here there
// is also a button for it, and pressing that button is a separate paid turn
// against a separate session.
//
// conversation is empty on the opening turn. It is filled only when a session
// could not be resumed and the turns have to be replayed from the database,
// which is why both paths share one function: the framing must not drift
// between a conversation that has been going all along and one that has just
// been rebuilt.
func ChatPrompt(detected, conversation string) string {
	var b strings.Builder
	b.WriteString(`A developer is asking you about this repository, in conversation. They are
reading your replies and will answer.

The checkout is mounted READ-ONLY. Read whatever you need to answer properly —
the README, the build manifest, the code the question is actually about. You
cannot edit, commit or run anything that writes, and should not try.

`)
	if detected = strings.TrimSpace(detected); detected != "" {
		b.WriteString("WHAT THE DAEMON ALREADY DETECTED:\n")
		b.WriteString(detected)
		b.WriteString("\n\n")
	}
	if c := strings.TrimSpace(conversation); c != "" {
		b.WriteString(`THE CONVERSATION SO FAR (your own session was lost, so this is the record
of it — carry on from here as if you remembered it):

`)
		b.WriteString(c)
		b.WriteString("\n\n")
	}

	b.WriteString(`How to be useful here:

- Answer the question that was asked, first, in a couple of sentences. Then the
  reasoning, if it needs any.
- Ground every claim in a file:line you actually read, and name it. "Because
  architectBusy at internal/engine/architect.go:106 derives it from turn order"
  is an answer; "because of how the state is tracked" is not. If you did not
  read it and are reasoning from the shape of things, say you are guessing.
- When the answer reveals something worth changing, offer it as a concrete
  question they can answer — "want an explicit in-flight column instead?" —
  and stop there. One at a time. You are not writing a report.
- Disagree when you disagree, including with the premise of the question. If
  the thing they think is a problem is deliberate, say so and say why.
- Keep it short enough to read. This is a conversation, not a document.

Do not produce a task list. When they want the work written down they will ask
for it, and that is a separate step. Reply in prose.`)
	return b.String()
}

// PullActionsPrompt turns a conversation into a reviewable task list.
//
// It runs against a fresh session with the conversation rendered into it,
// rather than as another turn of the chat. Resuming would append this
// instruction — "emit JSON and nothing else" — to the session permanently, and
// every later reply would be answering with that still in its context. It also
// means a pull can be taken from a conversation whose session is long gone.
//
// The two things it insists on are the two ways this goes wrong. Extracting
// what was discussed rather than what was decided turns every idea anyone
// floated into queued work. And a model that will not return an empty list
// invents something on a conversation that has not agreed anything yet, which
// is the normal state of every chat for its first few turns.
func PullActionsPrompt(conversation, detected string, filed []string, maxTasks int) string {
	var b strings.Builder
	b.WriteString(`Below is a conversation between a developer and an agent about this
repository. Turn what they DECIDED into a list of work items.

An automated system will carry each one out on its own branch, in its own
worktree, reviewed by a second agent until it converges, and open a draft pull
request. The developer reviews those pull requests. Nothing here runs without
them queueing it first.

The checkout is mounted READ-ONLY and is the same one the conversation was
about. Re-check every file:line you carry over — the conversation may have been
loose about them, and a task list must not be.

`)
	if detected = strings.TrimSpace(detected); detected != "" {
		b.WriteString("WHAT THE DAEMON ALREADY DETECTED:\n")
		b.WriteString(detected)
		b.WriteString("\n\n")
	}

	b.WriteString("THE CONVERSATION:\n\n")
	b.WriteString(strings.TrimSpace(conversation))
	b.WriteString("\n\n")

	if len(filed) > 0 {
		b.WriteString(`ALREADY PULLED OUT OF THIS CONVERSATION — do not propose any of these
again, whatever became of them. The developer has seen each one and decided:

`)
		for _, goal := range filed {
			if goal = strings.TrimSpace(goal); goal != "" {
				b.WriteString("- ")
				b.WriteString(goal)
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, `This conversation is still going. Extract what was DECIDED, not what was
discussed. A suggestion the agent made and the developer did not take up is not
an action. A question nobody answered is not an action. An explanation of how
something already works is not an action.

AN EMPTY "tasks" ARRAY IS A CORRECT ANSWER, and you should give it whenever
either is true: nothing has been agreed yet, or everything that was agreed is
already in the list above. Do not invent work to fill the list — the developer
will keep talking and pull again.

Propose at most %d tasks. Each one must be:

- ONE self-contained change an agent can finish and a reviewer can judge. Not a
  theme, not an area of the codebase. If you cannot describe the diff you
  expect, it is not a task.
- Traceable to the conversation. "rationale" says in one sentence what was
  decided and "evidence" lists the file:line references behind it.
- Carried by this repository's own conventions, in "constraints".

Set "verify" to the command that would prove the task is done in this
repository — the test command that actually exists here. Use null only if this
repository genuinely has none.

Use "depends_on" only for a real ordering constraint, naming the "key" of a
task EARLIER in your array. Never name a later task and never form a cycle.

Reply with a single JSON object matching this schema and nothing else — no
prose before it, no explanation after it, no markdown fence:

%s`, maxTasks, strings.TrimSpace(string(agent.ProposalSchema)))
	return b.String()
}
