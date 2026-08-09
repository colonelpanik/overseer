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
    overseer repos                      # per-repository time, turns and usage
    overseer backlog                    # what each repository still has waiting
    overseer new ~/code/thing -brief "…"  # create a project and design it
    overseer stop 3                     # park a task; -now kills the agent
    overseer start 3                    # put it back to work
    overseer restart 3                  # run it again, on a fresh branch
    overseer plan 3                     # the plan it is working from
    overseer logs 3                     # one task's steps, verdicts, findings

Do not know what to put in the task list? Open the dashboard and press
**Analyse a repo** to have one proposed, or **Chat** to ask about the
repository first and pull the actions out of the answers.

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

Five jobs, each independently configurable: **code** writes the plan and the
implementation, **review** judges them and produces the verdict, **analyse**
reads a repository for the wizard, **architect** talks a design through with
you, and **chat** answers questions about a repository. Each binds to an agent
CLI, an endpoint and a model.

They are separate knobs because they want different models. The two
conversations are the clearest case: the architect decides what everything
downstream builds and happens once per piece of work, so it defaults to the
strongest model; the chat answers a question at a time, many times a day, and
defaults to a middling one. Sharing a role would mean the only way to make
casual questions cheaper is to make the design conversation cheaper too.

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
  code:      {agent: claude, provider: anthropic, model: claude-opus-5}
  review:    {agent: codex,  provider: inhouse,   model: qwen3-coder-480b}
  analyse:   {agent: claude, provider: anthropic, model: claude-sonnet-5}
  architect: {agent: claude, provider: anthropic, model: claude-opus-5}
  chat:      {agent: claude, provider: anthropic, model: claude-sonnet-5}
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

## Designing something, and starting a project

**Design something** on the dashboard opens a conversation with an architect.
You say what you want; it asks what it actually needs to know, says what it
would build, and disagrees where it disagrees. When you accept, it writes the
design down and breaks it into tasks.

    you:        a CLI that syncs S3 buckets one way, resumable. Go, no deps.
    architect:  Resumable across what — a killed process, or a machine that
                comes back a day later? And is "one way" a mirror or an append?
    you:        a day later. append only — no delete at all, not even a flag.
    architect:  Then the manifest lives beside the destination, so any machine
                can pick up where any other left off. Three pieces: …
    you:        Accept

That works two ways.

**For a repository that exists**, it reads the code first, so its questions are
grounded in what is actually there — and it reads it *read-only*, so a
conversation cannot leave a branch, a stash or an edit behind in a tree you only
asked it to think about. Accepting gives you the task list, and the tasks cite
real files.

**For a new project**, give it a path instead. The directory is created and
initialised before the conversation starts, so there is somewhere real to work.
Accepting writes `DESIGN.md`, scaffolds the project, and *then* proposes the
tasks against what was actually built.

### Why the scaffold is not a task

Because `done` means "draft pull request opened", not "merged". Every task cuts
its worktree from the default branch, so a scaffold task would leave its
`go.mod` on an unmerged branch while every task that depended on it started from
an empty base and built on nothing.

So the scaffold is one turn, before anything is queued, committed straight to
the default branch. It is the only agent turn that is not reviewed, and it is
deliberately narrow: the layout, the manifest, something that builds, a test
command that works, and a README. The features are tasks, and they get the whole
loop. You can read the whole thing in the Diff tab before queueing anything.

The architect is its own configurable role, defaulting to the strongest model —
this is the conversation that decides what everything else builds.

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
- **What it used is visible.** An analysis's usage is part of the header's
  figures and its repository's totals, not something you have to go looking
  for. Whether that usage is money depends on which provider served it — see
  *what the money figures mean* below.

The **analyse** role picks the model (see Models and providers above), and the
wizard's own dropdown can override it for a single run. It is a separate knob
from whatever runs the loop: the analysis reads once and writes nothing, so it
is worth spending less on than the turns that produce the change.

## Asking about a repository

**Chat** is the surface for the questions that come before you know what to ask
for. *Why does this infer busy from turn order instead of taking a lock? What
breaks if I change it?* You get an answer grounded in the checkout, citing the
`file:line` it read — and, when there is a better shape, the offer as a
question you can answer: *"because a lock in memory dies with the process —
want an explicit in-flight column instead?"*

When the conversation has decided something, **Pull actions** turns what you
agreed into a task list. It arrives in the same review pane an analysis uses:
edit it, deselect what you do not want, queue the rest.

**Pulling does not end the conversation.** That is the whole difference between
this and the design conversation, which finishes when you accept it. Here you
pull, queue two of the three, keep talking, and pull again next week. The chat
is per repository and lives as long as the repository does.

A pull that finds nothing is a normal outcome, not a failure. A conversation
that has not agreed anything yet has nothing to pull, and so does one where
everything agreed is already filed — the second pull will say so rather than
showing you the work you queued an hour ago. What has already been pulled out
is off-limits to every later pull of the same conversation, deterministically,
not just by asking nicely.

Some things worth knowing:

- **The checkout is mounted read-only**, `.git` included, exactly as an
  analysis is. A conversation about a repository cannot leave a branch, a
  stash or an edit behind in it.
- **The pull runs in its own session**, not as another turn of the chat. It has
  to: "reply with JSON and nothing else" would otherwise sit in the
  conversation's context for ever and every later answer would be shaped by it.
- **Each reply is one agent turn**, and the overlay says what the conversation
  has cost under the box. This is the surface most likely to spend real money
  casually, so the figure is where you are looking rather than somewhere you
  have to go and find.
- **New chat** archives the current conversation and starts a fresh one. The
  old one stays readable. It is for a thread that has wandered somewhere it
  will not come back from, not for tidying up.

The **chat** role picks the model (see Models and providers above) and defaults
to a middling one rather than the strongest. The design conversation decides
what everything downstream builds and happens once per piece of work; this one
answers a question at a time, many times a day, and reads rather than decides.
They are separate knobs so you can move either without moving the other.

## Stopping, starting, restarting

    overseer stop 3          # park it where it is
    overseer stop 3 -now     # and kill the agent mid-turn
    overseer start 3         # put it back to work
    overseer restart 3       # run it again, on a fresh branch

**Stop** parks a task. **Start** puts it back, and it resumes the action it was
on rather than beginning again — the same mechanism that lets a task survive a
daemon restart. A stopped task reads as `stopped` on the board and is never
claimed, but underneath it keeps the state that says where it had got to.

Stopping is **soft by default**: the current agent turn finishes, then the task
parks at the boundary. That costs at most one turn and leaves nothing
half-written. **Stop now** kills the agent instead, for a step that is wedged
and would otherwise run to its 30-minute timeout.

What a hard stop does with the half-finished turn: it commits it, as
`overseer: interrupted during exec iteration 3` — never the normal message,
which would claim an iteration completed. The tree is left clean, so the work
is visible in the Diff tab and your own edits do not get swept into the next
turn's commit alongside it.

**Restart** runs a task again from the top, on `overseer/<slug>-r2` in its own
worktree. The previous attempt is kept exactly as it was, so you can compare
against the thing you are restarting — usually the reason you are. You can
amend the goal and the constraints on the way through: *restart it, but this
time do not touch the schema*. A task with a pull request is refused, because
restarting it would either do nothing or force-push over the open branch.

**Abandon** ends a task on purpose. It lands in `abandoned`, not `failed`:
`failed` means the machinery or the agent failed, and an operator's decision
reading the same way makes the board's one urgent signal mean two things.

**Stop all** on the nav stops the scheduler claiming anything; running tasks
park at their next boundary, and a second press kills their agents. It is
persisted, so a restart does not quietly resume everything you just stopped.

## Seeing and editing the plan

The **Plan** tab shows `PLAN.md` as it is on disk. That is not a copy: the plan
review, the implementation turn, the code review and the pull request body all
read it from there, so what the tab shows is what the next turn will act on.

**Stop a task and the plan becomes editable.** Save it and the next turn builds
what you wrote. That is the loop worth knowing:

    stop  →  read the plan  →  fix it  →  start

Editing is offered only while stopped, and not as a policy: a write landing
mid-turn races the agent editing the same tree, and would be folded into that
turn's commit as if the agent had made it.

Saving also clears the execution session, which is what makes the edit take
effect. A resumed implementation turn is prompted with the review's findings
and never re-reads `PLAN.md` — it runs on the session's memory of it — so
without that, an edit would be ignored by exactly the turn it was written for.

## Repositories

A repository is registered the first time you submit or analyse against it, so
**an existing task file keeps working unchanged** and the repo list fills
itself in. **Repos** on the dashboard, or `overseer repos`, is then the answer
to "what has this repository cost me" — tasks, analyses, agent time, turns,
usage, and how many things are still on its backlog.

Once a repository is registered, `repo:` may name it instead of repeating the
path:

```yaml
tasks:
  - repo: overseer          # the slug, not /home/kal/code/overseer
    goal: Enable WAL mode on the store connection.
```

Two repositories whose directories share a basename — a vendored copy, or the
same project checked out twice — get distinct slugs (`widget`, `widget-2`) and
stay distinct everywhere, including the board's group headers.

A repository carries defaults new tasks inherit. Settings resolve **task > repo
> daemon default**, and empty at any level means "fall through", never "off":

| setting | task file | repo | daemon |
| --- | --- | --- | --- |
| verify command | `verify:` | Repos overlay | `verify_command` |
| blocking threshold | `blocking_severity:` | Repos overlay | `blocking_severity` |
| cost cap | `cost_cap:` | Repos overlay | `task_cap_usd` |

Archiving a repository hides it from the pickers and deletes nothing: its
tasks, analyses and backlog stay where they are, because the record of what was
done to a repository outlives your interest in it.

## The backlog

Each repository has a durable todo list, fed by three sources and deduplicated
by fingerprint:

- **Reviews.** A finding *below* the task's blocking threshold is one the loop
  deliberately did not act on. It used to be displayed on the finding ledger
  and could never become anything; now it lands here with its `file:line` and
  which task raised it. Blocking findings are not copied — the loop is already
  acting on those.
- **Analyses.** Queueing three of twelve puts the other nine here. The analysis
  stays on **Analyses** as the record of one run; the backlog is the working
  list.
- **You.** A form on the panel.

The fingerprint is why a nit the reviewer raises on three separate tasks is one
item reading **seen 3×** rather than three identical rows — and why dismissing
one makes it stay dismissed instead of coming back on every review that notices
it again.

**Queue it** turns an item into a task through the ordinary submit path,
inheriting the repository's defaults, with the item's evidence carried into the
task's constraints so the agent starts from the citation instead of
rediscovering it.

## Spend caps, and what the money figures mean

**The default `claude` and `codex` providers run against your subscription**,
through each CLI's own stored login. What those CLIs report as `total_cost_usd`
is what the usage *would* have cost through the API — a usage signal, not a
bill. Only a provider you configured yourself with a `base_url` and a `key_env`
is metered to you.

So the dashboard shows two figures and never adds them together:

- **reported** — subscription-covered CLI usage, priced as if it had gone
  through the API.
- **metered** — usage against an endpoint you supplied. This one is money.

Per-repository accounting leads with **agent time** and **turns** for the same
reason: those are true whatever provider served the work.

`run_cap_usd` and `task_cap_usd` are **advisory**. Passing one raises a banner
on the dashboard, next to a button that raises the cap; it does not stop
anything. That is deliberate: killing an agent halfway through an edit leaves
the worktree in a state nobody chose and throws away everything already paid
for getting there. The cap tells you a task has gone further than you expected,
which is the decision you actually wanted to make — not a kill switch that
makes the mess worse.

A per-task cap can be set in the batch file with `cost_cap`, on the repository,
or raised from the dashboard.

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
- **The agents' own sandboxes are turned off inside overseer's.** Both CLIs
  confine their own shell tool with bubblewrap, and a sandbox inside a sandbox
  is refused on a kernel that gates unprivileged user namespaces behind an
  AppArmor profile — Ubuntu 24.04 and later, where
  `kernel.apparmor_restrict_unprivileged_userns` is `1`. Overseer's own sandbox
  works there; the agent's then fails on every run with `bwrap: No permissions
  to create a new namespace`, which reads in a transcript exactly like
  overseer's being broken. So a confined `claude` is told it is already confined
  (`CLAUDE_CODE_SANDBOXED`), and a confined `codex` is run with
  `--dangerously-bypass-approvals-and-sandbox`, which is what codex documents
  for "environments that are externally sandboxed" — every one of its `-s`
  modes, `danger-full-access` included, is built out of bubblewrap and would
  still nest. Neither is done with `sandbox: off`, where the agent's own
  sandbox is the only one there is. The board says which applies.
- **The reviewer still cannot write.** That property now rests entirely on
  bubblewrap: overseer mounts the reviewer's worktree read-only because
  writability follows the *role*, not the CLI. Dropping codex's own
  `-s read-only` removes a second statement of the same rule, not the rule.
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
