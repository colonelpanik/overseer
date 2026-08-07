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

The dashboard is the point: each card shows a **`plan 3/10`** counter, so a
task that is converging looks different at a glance from one that is
ping-ponging. Clicking through gives the alternating timeline of Claude turns
and Codex reviews, with each review's findings.

## Configuration

`~/.overseer/config.yaml`, all keys optional:

```yaml
listen_addr: 127.0.0.1:7777
data_dir: ~/.overseer
max_parallel: 3          # tasks in flight at once
max_iterations: 10       # per phase, then the task parks for a human
step_timeout: 30m
blocking_severity: any   # any | minor | major | critical
sandbox: auto            # auto | bwrap | off
bwrap_bin: bwrap
verify_command: ""
```

`blocking_severity: any` means every Codex finding, including nits, keeps the
loop running. That is the strictest setting and the default. If a task starts
burning iterations on style nits, set `blocking_severity: major` on that task
in the batch file rather than babysitting it.

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
**abandon**, and **take over**; the last prints the `cd` and
`claude --resume <session-id>` needed to drive the session by hand.

## Safety

- Agents run inside a [bubblewrap](https://github.com/containers/bubblewrap)
  sandbox by default (`sandbox: auto`). `$HOME` becomes an empty tmpfs and only
  what the agent needs is mounted back: the task worktree, the repository's git
  directory, the agent's own state directory, and its binary. Other
  repositories, your dotfiles, `~/.ssh`, and overseer's own database are simply
  absent. The agent's configuration (`~/.claude/settings.json`, its plugin
  directory, `~/.codex/config.toml`) is mounted read-only, so a sandboxed agent
  cannot plant a hook that would run on the next unsandboxed invocation.
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
