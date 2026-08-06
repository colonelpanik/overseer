# Overseer — design

Date: 2026-08-06
Status: approved, ready for implementation planning

## Problem

The current workflow is manual and human-bound:

1. Ask Claude for a plan.
2. Paste the plan to Codex for review.
3. Paste Codex's review back to Claude.
4. Repeat until the plan is good.
5. Tell Claude to execute.
6. Paste the diff to Codex for review.
7. Paste Codex's review back to Claude.
8. Repeat until the code is good.

Every arrow is a human copy-paste. The human adds no judgment at those arrows — they
are pure transport. The loop also serialises: only one task can be in flight, because
the human is the bus.

## Goal

A single tool that accepts a list of tasks, runs the plan loop and the execute loop for
each task autonomously and in parallel, and presents a dashboard where the human can
see whether each task is converging or ping-ponging. The human's only remaining jobs are
writing the task list and reviewing the resulting pull requests.

## Decisions

| Question | Decision |
|---|---|
| Human gates | None mid-loop. Loop until Codex reports no changes. At iteration 10, park and ask permission to continue. |
| Concurrency | Parallel, one git worktree + branch per task. |
| Interface | One Go binary, SQLite state, server-rendered web dashboard. |
| Convergence | Codex returns a machine-readable verdict. Blocking = **any** finding (configurable per task). |
| Task input | `overseer submit tasks.yaml`, plus an add-task box in the dashboard. |
| Completion | Commit, push branch, open a **draft** PR. Nothing merges without the human. |
| Agent permissions | Claude runs `--permission-mode bypassPermissions` inside a bubblewrap sandbox. Codex reviews with `-s read-only`, also sandboxed. |
| Architecture | Go daemon driving both CLIs as subprocesses with structured output. |

### Correction: what the agent's working directory does and does not do

The option this decision was chosen from described the blast radius as "one throwaway
worktree". That was wrong, and the error is recorded here rather than quietly dropped
because it may change the decision.

`--permission-mode bypassPermissions` skips the permission system; it does not narrow it.
Setting the working directory to the worktree and omitting `--add-dir` confines nothing —
`--add-dir` only extends the allow-list that has already been bypassed. Claude runs with
the daemon user's full filesystem access and can read or write any absolute path that user
can, including other repositories, dotfiles, and SSH keys.

What the worktree genuinely provides is **isolation between concurrent tasks**: two agents
working at once cannot collide on the same files or branch, and the user's own checkout is
never the working directory. That was the original motivation for worktrees and it still
holds. Containment of a misbehaving agent was never actually delivered.

Real containment needs an OS-level sandbox. **This has since been added** — see the
Sandbox section below — so the caveat above applies only when `sandbox: off`.

### Rejected alternatives

- **Claude Agent SDK orchestrator (TS/Python).** Covers only the Claude side; Codex would
  still be a subprocess, making the design asymmetric, in a stack not otherwise used here.
- **tmux/expect driving the interactive TUIs.** Faithful to the manual flow but requires
  screen-scraping ANSI output. Both CLIs already expose structured headless output,
  so there is no reason to parse a terminal.
- **Auto-merge on convergence.** Ships unreviewed code to the default branch.
- **Gate at every loop iteration.** Puts the human back on the bus, which is the problem
  being solved.

## CLI capabilities this design depends on

Verified against `codex-cli 0.146.0` and Claude Code `2.1.223`:

- `claude -p` — headless. `--output-format stream-json` for a JSONL event stream,
  `--resume <session-id>` to continue a session across loop iterations,
  `--permission-mode bypassPermissions`, `--add-dir` (deliberately unused).
  `--verbose` is mandatory alongside `--output-format stream-json`.
- `codex exec` — headless. `--json` for JSONL events, `--output-schema <file>` to constrain
  the final message to a JSON Schema, `-o/--output-last-message <file>` to capture it,
  `-s read-only` for the sandbox. **Its stdin must be closed:** `codex exec` appends piped
  stdin to the prompt and blocks waiting for EOF, so a non-TTY stdin that is never closed
  hangs the process indefinitely.
- `gh pr create --draft` for the completion step.

Both event vocabularies were captured from live runs rather than inferred. Claude emits
`system/init` (carrying `session_id`), `assistant`, `rate_limit_event` and `result`, and
also relays the user's `SessionStart` hook output as `system/hook_started` and
`system/hook_response` — parsers must tolerate those. Codex emits `thread.started`
(carrying `thread_id`), `turn.started`, `item.completed` (only `agent_message` items hold
the verdict), `turn.completed`, and `error`/`turn.failed`.

`--resume` is the load-bearing flag: it means each Codex review is delivered into the
*same* Claude session that produced the work, so Claude retains its own context exactly
as it does when a human pastes a review into an open session.

`codex exec review` exists as a dedicated subcommand but does not accept
`--output-schema`. The design therefore uses plain `codex exec` with a review prompt and
an explicit schema, because a deterministic verdict matters more than the built-in
review framing.

## Architecture

```
overseer serve
  ├── scheduler        N workers, semaphore on max_parallel
  ├── loop             pure state machine: Next(task, lastResult) → Action
  ├── agent            Runner interface; Claude and Codex drivers
  ├── worktree         git + gh plumbing
  ├── store            SQLite: tasks, steps, findings
  └── web              html/template pages + SSE
```

`modernc.org/sqlite` keeps the build cgo-free, so the result is one static binary.

### Package boundaries

- **`internal/agent`** is the only package that knows a CLI exists. Both drivers satisfy:

  ```go
  type Runner interface {
      Run(ctx context.Context, prompt string, opts Opts) (Result, <-chan Event, error)
  }
  ```

  Each driver parses its own tool's JSONL into a shared `Event` type. `Result` carries
  the session ID, exit code, token/cost totals, and the parsed final message.

- **`internal/loop`** is a pure function over task state. It spawns nothing and touches
  no database, so the entire control flow is unit-testable in milliseconds.

- **`internal/worktree`** owns every git and `gh` invocation. A per-repo mutex prevents
  two workers from racing on the same repository's index during `worktree add`.

- **`internal/store`** holds state and metadata only.

- **`internal/web`** renders server-side and exposes one SSE endpoint.

### Storage split

SQLite holds state; transcripts go to files.

Full agent transcripts are written to
`~/.overseer/runs/<task-slug>/<phase>-<iteration>-<agent>.jsonl`. Token-level events
would bloat the database, and tailing a file is how SSE gets live output without a
second storage path.

Tables:

- `tasks` — id, slug, repo_path, goal, constraints, state, phase, iteration,
  plan_session_id, exec_session_id, branch, pr_url, blocking_severity, error,
  created_at, updated_at
- `steps` — id, task_id, phase, iteration, agent, started_at, ended_at, exit_code,
  verdict, tokens, cost_usd, transcript_path
- `findings` — id, step_id, severity, file, line, summary, resolved_in_step_id

## Task lifecycle

```
queued → worktree → PLAN LOOP → EXECUTE LOOP → finish → done
                       ↑↓            ↑↓
                    escalated     escalated
                       ↓              ↓
                     failed        failed
```

### Worktree setup

`git fetch`, then
`git worktree add <root>/.overseer/<task-slug> -b overseer/<task-slug> origin/<default-branch>`.

Each task is fully isolated. The user's own checkout is never touched.

### Plan loop

1. Run `claude -p --permission-mode bypassPermissions --output-format stream-json` with
   cwd set to the worktree. Prompt: the task goal, its constraints, and an instruction to
   write the implementation plan to `PLAN.md` and write no code. Capture `session_id`
   from the init event and persist it.
2. Run `codex exec -s read-only --output-schema verdict.schema.json -o last.json` with a
   prompt to review `PLAN.md` against the goal. The sandbox mode is passed explicitly on
   every Codex invocation rather than relying on the CLI default, so the reviewer can
   never write.
3. If `findings` is non-empty, run `claude -p --resume <plan_session_id>` with the
   findings rendered as the next turn. Increment `iteration`.
4. If `findings` is empty, the plan has converged.

### Execute loop

Same shape, with one deliberate difference: it starts a **fresh** Claude session seeded
with `PLAN.md` rather than resuming the planning session. The plan is already the
distilled intent, and a clean context leaves more room for the implementation work.

The overseer commits at the end of each Claude turn, rather than instructing Claude to
commit. The branch state is then deterministic, and a turn that forgets the instruction
cannot lose work. Codex reviews with `--base origin/<default-branch>`, so it sees the real
accumulated diff rather than a description of it. Reviews feed back via
`--resume <exec_session_id>`.

### Verdict schema

`--output-schema` is validated in OpenAI **strict** mode: every key in a `properties`
object must also appear in that object's `required` array. Optional fields are therefore
expressed as nullable and still listed as required. Omitting `file` from `required`
returns HTTP 400 `invalid_json_schema` — verified against `codex-cli 0.146.0`.

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["verdict", "findings"],
  "properties": {
    "verdict": { "type": "string", "enum": ["approved", "changes_requested"] },
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["severity", "summary", "file", "line"],
        "properties": {
          "severity": { "type": "string", "enum": ["critical", "major", "minor", "nit"] },
          "summary": { "type": "string" },
          "file": { "type": ["string", "null"] },
          "line": { "type": ["integer", "null"] }
        }
      }
    }
  }
}
```

Severity is captured on every finding even though the default blocking threshold is
`any`, because the dashboard displays it and because raising the threshold must not
require re-running anything.

The `findings` array is authoritative for convergence; the `verdict` enum is recorded and
displayed but never decides the loop. If Codex returns `approved` alongside a non-empty
`findings` array, the findings win and the loop continues.

### Convergence and the blocking threshold

A phase converges when Codex returns zero findings at or above the task's
`blocking_severity`. Allowed values are `any` (the default — every finding blocks,
including nits), `minor`, `major`, and `critical`. Setting it to `major` on a task that is
burning iterations on style nits is the intended escape hatch, and it is a single config
field rather than a separate mechanism.

Non-blocking findings are still recorded and shown on the task page as a punch list, so
raising the threshold never discards information.

### Escalation

The iteration counter is per phase and resets to zero when the task moves from planning to
executing, so each loop gets its own budget of 10.

Hitting iteration 10 in either loop parks the task in `escalated`. Its dashboard card
turns amber and offers three actions:

- **continue** — grant 10 more iterations
- **abandon** — mark failed, keep the branch and worktree
- **take over** — display the `cd` and `claude --resume <session-id>` commands for
  driving the session by hand

### Completion

Push the branch and open a draft PR via `gh pr create --draft`, with the final `PLAN.md`
and the last Codex review in the body. The task moves to `done`. The worktree is removed;
the branch is kept.

## Failure handling

1. **Off-schema Codex output.** Re-ask once with a stricter instruction. A second failure
   escalates. Unparseable output is *never* treated as approval — that single mistake
   would silently ship unreviewed code, and it is the most important invariant in the
   system.

   The parser is the safety boundary and validates independently of the CLI's schema
   enforcement, because that enforcement is exactly what may have failed. It requires
   `verdict` and `findings` to be *present* — a decoder that maps an absent `findings` to
   an empty slice would read a truncated response as approval — rejects anything trailing
   the JSON object, and rejects `changes_requested` carrying an empty `findings` array as
   self-contradictory rather than converging on it.
2. **Agent process failure.** Retryable causes (rate limit, network, 5xx) get exponential
   backoff up to 3 attempts and **do not count against the iteration cap**.
   Authentication failure pauses the entire run with a banner, since every task would
   fail identically.
3. **Oscillation.** Hash the blocking-findings set on each iteration and compare it against
   every earlier iteration of the same phase. A repeat means Claude is not making progress
   on that finding, so the task escalates immediately rather than burning through to
   iteration 10. This is the practical mitigation for a blocking threshold of `any`: a nit
   that Claude "fixes" twice is caught around iteration 3.
4. **Step timeout.** A 30-minute wall clock per step. On expiry, kill the process group,
   mark the step timed out, retry once, then escalate.
5. **git or gh failure.** The task moves to `failed` with the worktree **preserved** and
   the failing command displayed. Worktrees are removed only on `done`; branches always
   survive.
6. **Daemon restart.** Steps left in `running` at startup are marked `interrupted`, and the
   task re-dispatches whatever action its state was waiting on rather than assuming that
   action completed. Persisted session IDs make resumption safe; a resume action reloads
   its findings from the database, since they were otherwise only held in memory.

   Re-dispatch means every action must tolerate being repeated. Agent turns are naturally
   safe — a repeated plan turn overwrites `PLAN.md`, a repeated review costs one review.
   Finishing is the exception, because a task only becomes `done` after `finish` returns:
   a crash in that window would otherwise re-run `finish` against an already-pushed branch
   and an already-removed worktree, and fail a task that had in fact succeeded. Two things
   make the repeat safe: `finish` returns early when the task already records a PR URL, and
   opening a PR first checks for an existing one on the branch instead of creating a
   duplicate.

7. **Concurrent tasks.** No per-task state lives on the shared engine. Step records are
   passed through the call chain rather than parked in a field, or a second task's review
   could attach its verdict and findings to the first task's step. The concurrency test
   runs under `-race`.

## Interfaces

### CLI

- `overseer serve` — run the daemon and dashboard
- `overseer submit tasks.yaml` — enqueue a batch
- `overseer status` — table of tasks, states, iteration counters
- `overseer logs <task>` — tail a task's transcripts

### Task file

Batch files carry tasks only. `max_parallel` and other daemon settings live in
`~/.overseer/config.yaml`, so submitting a second batch cannot silently change the
concurrency of a run already in flight.

```yaml
tasks:
  - repo: /home/kal/code/dc-planner
    goal: |
      Add CSV export to the rack inventory view.
    constraints:
      - Server-rendered, no new JS dependencies
  - repo: /home/kal/code/clanker
    goal: Replace the hand-rolled retry logic with a shared backoff helper.
    blocking_severity: major
```

### Dashboard

- `/` — one card per task showing goal, repo, state badge, an iteration counter such as
  `plan 3/10`, elapsed time, and cost. Amber for escalated, red for failed, green for
  done with a PR link. An add-task box sits at the top.
- `/task/<id>` — an alternating timeline of Claude turns and Codex reviews. Each review
  expands to its findings with severities; each turn expands to its transcript. The
  active step streams live. Escalation actions live here.
- `/events` — SSE stream of state transitions and output lines.

The iteration counter and the per-review findings list are the substance of the
monitoring requirement: together they show at a glance whether a task is converging or
ping-ponging.

## Testing strategy

- **`internal/loop`** — table-driven tests over the state machine: converges on iteration
  1; converges on iteration 4; hits the cap; oscillates; off-schema verdict; agent crash.
  Pure function, sub-second, no subprocesses.
- **`internal/agent`** — golden-file tests against **real recorded JSONL** captured from
  `claude -p --output-format stream-json` and `codex exec --json`. Capturing those
  fixtures is an explicit early step in the implementation plan; without them the parsers
  rest on guesses about the event shapes.
- **`internal/worktree`** — integration tests against a temporary repository with a local
  bare remote. Real git, no network.
- **End-to-end smoke** — one trivial task ("add a function returning 42, with a test")
  against a throwaway repository, running both loops for real. The PR step sits behind an
  interface so the test asserts the invocation instead of contacting GitHub.

## Sandbox

Both agents run inside a [bubblewrap](https://github.com/containers/bubblewrap) namespace.
`$HOME` becomes an empty tmpfs and only what the agent needs is mounted back:

| Mount | Access | Why |
|---|---|---|
| Task worktree | rw for Claude, **ro** for Codex | The work. The reviewer never writes. |
| `<repo>/.git` | ro | Objects and refs, so `git log` and `git diff` work. |
| `<repo>/.git/worktrees/<slug>` | rw for Claude | The worktree's index. Without it `git status` fails. |
| Agent state dir (`~/.claude`, `~/.codex`) | rw | Session persistence, so `--resume` works. |
| Agent config + plugins | **ro** | A writable config lets the agent plant a hook that runs on the next *unsandboxed* invocation. That is an escape, not a sandbox. |
| Agent binary directories | ro | Both CLIs install as symlinks into versioned directories under `$HOME`. |
| `<runs>/<slug>` | rw | Codex writes `--output-last-message` itself. |
| Verdict schema file | ro | Codex reads `--output-schema` itself. |
| `/usr`, `/etc`, `/run/systemd/resolve` | ro | The system, and DNS. |

Everything else is absent: other repositories, dotfiles, `~/.ssh`, and overseer's own
database. The data directory is never mounted whole, only the current task's run
directory, so an agent cannot rewrite task state.

Three constraints were found by running real agents inside candidate profiles rather than
by reasoning about them:

- `/etc/resolv.conf` symlinks into `/run`, so a `/run` tmpfs breaks DNS and every API call
  with it. `/run/systemd/resolve` must be re-exposed.
- Codex reads the schema and writes the last-message file itself, so both paths need
  mounts; a missing schema mount fails the run outright.
- Writes to unmounted paths beneath a tmpfs parent silently succeed and are discarded.
  Real data is safe, but an agent may report having written a file that does not exist.

Network is deliberately **not** unshared: the agents call an HTTPS API. The sandbox
constrains what an agent can read and write, not what it can transmit.

`sandbox` accepts `auto` (default — use bubblewrap where it works, warn loudly where it
does not), `bwrap` (require it; fail to start otherwise), and `off`. Mode selection probes
by actually creating a namespace, because bubblewrap's presence does not imply it can run:
unprivileged user namespaces can be disabled by sysctl, and on Ubuntu an AppArmor profile
governs bubblewrap's access to them. When a run is unsandboxed the board says so on every
page.

## Out of scope

- Merging. Draft PRs only.
- Restricting agent network access. The sandbox is a filesystem boundary only.
- Per-task containers or VMs. Bubblewrap covers the filesystem exposure that mattered;
  a container would mainly add image and credential plumbing.
- Linear or other issue-tracker ingestion.
- Multi-machine or remote execution.
- Any reviewer other than Codex.

## Estimate

Roughly one day for the loop, agent drivers, and CLI — the state machine and the two
JSONL parsers are the substance. Roughly half a day for the dashboard. Contingent on the
recorded JSONL fixtures matching expectations.
