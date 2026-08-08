# overseer

Runs the Claude-plan → Codex-review → feed-back loop for a list of tasks,
without a human in the middle of the loop.

Each task gets its own git worktree and branch. Claude writes a plan; Codex
reviews it against a JSON schema that forces a machine-readable verdict; the
findings go back into the *same* Claude session; that repeats until Codex
returns none. Then the same loop runs over the implementation. When both
converge, overseer pushes the branch and opens a draft pull request.

The human writes the task list and reviews the pull requests.

## Install

    go build -o overseer ./cmd/overseer

Requires `claude`, `codex`, `git`, and `gh` on PATH.

## Use

    overseer submit tasks.example.yaml
    overseer serve                      # daemon + dashboard on :7777
    overseer status                     # same information as a table
    overseer logs 3                     # one task's steps, verdicts, findings

Do not know what to put in the task list? Open the dashboard and press
**Analyse a repo**.

The dashboard is the point. It is one page: the task list on the left, the
selected task on the right. Every task carries a **`plan 3/10`** counter and a
sparkline of blocking findings per review round, so a task that is converging
looks different at a glance from one that is ping-ponging — a hollow bar is a
round that raised nothing.

Selecting a task gives the two-lane timeline — Claude on the left, Codex and
verify on the right — and a right-hand pane with three tabs:

- **Diff** — the accumulated change against the base ref, with each still-open
  finding pinned to the line it was raised against.
- **Findings** — every finding ever raised on the task, whether it is still
  open, and which round it was fixed in. A finding stays on the ledger after it
  is resolved, so one that comes back reads as a repeat rather than as news.
- **Live** — the running step's transcript, summarised one line per event.

When a task starts going in circles the convergence chart is replaced by a
**fingerprint matrix**: findings down the side, recent rounds across the top,
a red cell for a finding the reviewer had already raised. That is the same
signal the oscillation check trips on, made legible — and the task's action bar
offers the blocking threshold that would let it finish.

The whole view lives in the URL — selection, filter, open step, tab — so the
page survives the reload the daemon triggers on every state change, and a link
to "the parked task with its fingerprint open" is just a link.

## Configuration

`~/.overseer/config.yaml`, all keys optional:

```yaml
listen_addr: 127.0.0.1:7777
data_dir: ~/.overseer
max_parallel: 3          # tasks in flight at once
max_iterations: 10       # per phase, then the task parks for a human
step_timeout: 30m
analysis_timeout: 30m    # how long one repo analysis may run
blocking_severity: any   # any | minor | major | critical
sandbox: auto            # auto | bwrap | off
bwrap_bin: bwrap
verify_command: ""
run_cap_usd: 0           # advisory, 0 disables
task_cap_usd: 0          # advisory default per task, 0 disables
```

## Models and providers

Three jobs, each independently configurable: **code** writes the plan and the
implementation, **review** judges them and produces the verdict, **analyse**
reads a repository for the wizard. Each binds to an agent CLI, an endpoint and
a model.

```yaml
providers:
  anthropic:                       # configured by default
    kind: anthropic
    key_env: ANTHROPIC_API_KEY
    models: [claude-opus-5, claude-sonnet-5, claude-haiku-4-5]
  inhouse:
    kind: openai
    base_url: https://llm.dc.internal/v1
    key_env: DC_LLM_KEY            # the NAME of a variable, never a key
    models: [qwen3-coder-480b]

roles:
  code:    {agent: claude, provider: anthropic, model: claude-opus-5}
  review:  {agent: codex,  provider: inhouse,   model: qwen3-coder-480b}
  analyse: {agent: claude, provider: anthropic, model: claude-sonnet-5}
```

Providers are additive: a file naming one in-house endpoint keeps the vendor
ones. Absent roles keep their defaults, which are exactly the behaviour
overseer had before roles existed.

**A role may use either CLI.** Review with Claude, code through an
OpenAI-compatible endpoint, whatever fits. The one constraint is protocol —
`claude` talks to an Anthropic-shaped endpoint and `codex` to an OpenAI-shaped
one — and a mismatch is refused at load, not discovered halfway through a
paid-for task. `kind: openai` is any OpenAI-compatible endpoint: your own
gateway, a vLLM or LiteLLM deployment, or an upstream vendor.

**Keys are never stored.** A provider names the environment variable that holds
one; overseer reads it when it launches an agent and passes it through as that
CLI's own credential. Nothing lands in the config file, the database, or a log.
The dashboard shows whether each variable is set, because a missing key would
otherwise surface as an authentication failure that pauses the whole run.

`analysis_model` still works and is applied to `roles.analyse.model`.

**Models** on the dashboard shows the roles and providers, lets you repoint a
role, and writes the change back to the config file — editing only the
`providers:` and `roles:` keys, so the rest of your file keeps its comments and
ordering. A change applies to the next agent turn; steps already running keep
the role they started with. Adding a provider stays a config-file edit: pointing
the daemon at a new host is deliberate, not a dropdown.

`blocking_severity: any` means every Codex finding, including nits, keeps the
loop running. That is the strictest setting and the default. If a task starts
burning iterations on style nits, set `blocking_severity: major` on that task
in the batch file rather than babysitting it.

## Analysing a repository

**Analyse a repo** on the dashboard points Claude at a repository, read-only,
and comes back with a proposed task list you edit and queue. It is for the case
overseer is best at and worst to start: a codebase you have just inherited and
do not yet know what to ask for.

Four steps. Give it a path already on this machine, or a URL to clone. Say what
to look for — test coverage, tech debt, security, performance, documentation,
correctness — plus anything else in free text and a cap on how many tasks to
propose. Watch it read. Then review what came back: each proposal carries the
goal, the constraints it inferred from your repository's own conventions, the
verify command it actually found, a blocking threshold, a cost cap, the
dependencies between tasks, and — the part worth reading first — *why*, with
the `file:line` references behind it. Edit any of it, deselect what you do not
want, and queue the rest. If the list is wrong in a way you can describe, say
so and regenerate.

Nothing queues itself. The wizard always ends with a human pressing the button,
because that is the only checkpoint before money starts being spent.

**An analysis outlives the sitting it was made in.** Queue three of twelve
today and the other nine stay on the list; reopen it next week from
**Analyses** and queue what is left. Rows already turned into tasks are struck
through with a link to the task, and a dependency on one of them attaches to
the task that already exists rather than being silently dropped. The nav shows
a chip while an analysis is running or has tasks waiting to be reviewed, so
closing the tab does not lose the only link to it.

An analysis that a daemon restart interrupted is parked as failed with that
reason, rather than showing a spinner forever.

**If an analysis runs out of time**, `analysis_timeout` (default 30m) is the
knob, and the failure says so rather than leaving you with a bare timeout. A
large repository is both the best reason to run an analysis and the slowest
thing to read, so raise it — or narrow the focus and the task budget so there
is less to get through. Nothing partial survives a timeout: the task list
arrives as the agent's final message, so a run that never got there has nothing
to salvage. **Regenerate** on the failed analysis retries without restarting
the wizard.

Three things are true of the analysis and worth knowing:

- **The repository is mounted read-only**, `.git` included. `git log` and
  `git ls-files` work; nothing can be written. An analysis cannot leave a
  branch, a stash, or an edit behind in a repository you only asked it to read.
- **Only `https://` and `ssh://` URLs are cloned.** Other git transports can
  run a command the URL names — `ext::` most obviously — so they are refused.
  A clone lands in `<data_dir>/imported/<name>`; an existing clone of the same
  remote is reused, and a directory holding a clone of something else is an
  error rather than an overwrite.
- **It costs money and the dashboard says so.** Analysis spend is part of the
  run total in the header, not a separate figure you have to go looking for.

The **analyse** role picks the model (see Models and providers above), and the
wizard's own dropdown can override it for a single run. It is a separate knob
from whatever runs the loop: the analysis reads once and writes nothing, so it
is worth spending less on than the turns that produce the change.

## Spend caps

`run_cap_usd` and `task_cap_usd` are **advisory**. Passing one raises a banner
on the dashboard, next to a button that raises the cap; it does not stop
anything. That is deliberate: killing an agent halfway through an edit leaves
the worktree in a state nobody chose and throws away everything already paid
for getting there. The cap tells you a task has gone further than you expected,
which is the decision you actually wanted to make — not a kill switch that
makes the mess worse.

A per-task cap can be set in the batch file with `cost_cap`, or raised from the
dashboard.

## Dependencies

A task can name others it must follow:

```yaml
tasks:
  - repo: /home/kal/code/overseer
    goal: Enable WAL mode on the store connection.
  - repo: /home/kal/code/overseer
    goal: Validate config.yaml against a schema at startup.
    depends_on: [enable-wal-mode-on-the-store-connection]
```

The name is the other task's slug, which is derived from its goal. A dependency
may be a task earlier in the same batch or one already in the database; naming
one that does not exist fails the submit rather than queueing a task that waits
on nothing. Cycles and self-references are rejected on write.

A task waits only while it is queued. Once it has a worktree it is in flight,
and a dependency that fails afterwards does not pull it back out.

A dependency that fails will never reach done, so its dependents wait forever.
The board shows this — it names the dependency and its state — and offers
**Release anyway**, which clears the dependency and lets the task run.

## The verify gate

Set `verify_command` (or `verify:` on a single task) and overseer runs it in
the worktree after every implementation turn. It must exit zero before Codex is
asked to review, and a non-zero exit is fed back to the same Claude session as a
critical finding.

    verify_command: go test ./...

This is the only objective signal in the loop. Codex reviews the diff and runs
read-only, so it cannot execute anything; without a verify command, convergence
means "Codex stopped objecting", which a project that does not compile can
satisfy. With one, it means the tests pass as well.

A failing verify always blocks, whatever `blocking_severity` is set to. Failing
the same way twice escalates the task rather than spending the whole iteration
budget on it — timings and temporary paths are normalised away first, so "the
same way" means the same failing tests, not byte-identical output.

Output is streamed to the step transcript as the command runs, with only a
bounded tail kept in memory for the feedback to the agent, so a command that
prints continuously cannot exhaust the daemon.

## When a task parks

A task escalates when it hits the iteration cap, or earlier if the same set of
findings recurs — that means the agent is not making progress, and waiting for
iteration 10 would just cost money. The dashboard offers **continue**,
**abandon**, **take over**, and a one-click raise of the blocking threshold;
take over prints the `cd` and `claude --resume <session-id>` needed to drive
the session by hand.

With several tasks parked at once, tick them in the list and apply **continue**
or **abandon** to the selection in one go. If any of them cannot change — a
task a worker is currently driving cannot be abandoned — the response says so
and names them, rather than redirecting to a board where half the selection
quietly stayed put.

## Safety

- Agents run inside a [bubblewrap](https://github.com/containers/bubblewrap)
  sandbox by default (`sandbox: auto`). `$HOME` becomes an empty tmpfs and only
  what the agent needs is mounted back: the task worktree, the repository's git
  directory, the agent's own state directory, and its binary. Other
  repositories, your dotfiles, `~/.ssh`, and overseer's own database are simply
  absent. The agent's configuration (`~/.claude/settings.json`, its plugin
  directory, `~/.codex/config.toml`) is mounted read-only, so a sandboxed agent
  cannot plant a hook that would run on the next unsandboxed invocation. The
  agent's own Go build and module cache live under overseer's data directory,
  not `~/.cache/go-build` or `~/go/pkg/mod` — Go's build cache holds trusted
  output blobs, so a write smuggled through your real one could get linked
  into a later *unsandboxed* build without ever being rebuilt from source.
- The sandbox also clears the daemon's own process environment before the
  agent runs (`--clearenv`), then passes back only `HOME`, `PATH`, `TERM`,
  `LANG`/`LC_*`, and the agents' own credential variables
  (`ANTHROPIC_*`/`CLAUDE_*`/`CODEX_*`/`OPENAI_API_KEY`). `GITHUB_TOKEN`,
  `AWS_*`, and anything else your shell happens to export do not reach the
  agent. `sandbox_env_passthrough` lets an operator add more when a task
  genuinely needs it.
- Network is **not** restricted — the agents call an HTTPS API. The sandbox
  limits what an agent can read and write, not what it can send. Do not point
  overseer at a repository whose contents you would not want an agent to
  transmit.
- `sandbox: off` disables this. `--permission-mode bypassPermissions` skips the
  permission system rather than narrowing it, so with the sandbox off the agent
  has this user's full filesystem access. The board says so on every page when
  that is the case.
- Codex additionally runs `-s read-only`, and its worktree mount is read-only.
  The reviewer cannot write.
- Nothing merges. Pull requests are always drafts.
- Codex output that cannot be parsed into a verdict fails the task. It is
  never read as approval.
- If either CLI turns out not to be logged in, the whole run pauses with a
  banner rather than draining the queue — every task would have failed the
  same way. Log in, then press **Resume run**.
- The dashboard's state-changing routes (queuing a task, continuing or
  abandoning one, resuming a paused run) reject a cross-site request and a
  request whose `Host` header does not match `listen_addr`, so a hostile page
  open in the same browser cannot drive them.
- `overseer serve` takes an exclusive lock on `<data_dir>/overseer.lock`, so a
  second daemon against the same data directory fails fast instead of both
  daemons claiming the same tasks.
