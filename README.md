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
```

`blocking_severity: any` means every Codex finding, including nits, keeps the
loop running. That is the strictest setting and the default. If a task starts
burning iterations on style nits, set `blocking_severity: major` on that task
in the batch file rather than babysitting it.

## When a task parks

A task escalates when it hits the iteration cap, or earlier if the same set of
findings recurs — that means the agent is not making progress, and waiting for
iteration 10 would just cost money. The dashboard offers **continue**,
**abandon**, and **take over**; the last prints the `cd` and
`claude --resume <session-id>` needed to drive the session by hand.

## Safety

- **Claude is not sandboxed.** It runs with `--permission-mode
  bypassPermissions`, which skips the permission system rather than narrowing
  it. Its working directory is the task's worktree, but that is a starting
  directory, not a boundary: it can read and write any absolute path the
  daemon's user can, including your other repositories, your dotfiles, and
  `~/.ssh`. Omitting `--add-dir` changes nothing, because `--add-dir` only
  extends an allow-list that is already bypassed.

  In practice each task's *intended* work is confined to a throwaway worktree,
  and a misbehaving agent has never been the failure mode in the manual
  workflow this replaces. But run overseer as a user whose reach you are
  willing to hand to an unattended agent. Real confinement needs an OS-level
  sandbox — a container, `bwrap`, or `systemd-run` with `ProtectHome` — which
  overseer does not set up.
- Codex always runs `-s read-only`. The reviewer cannot write.
- Nothing merges. Pull requests are always drafts.
- Codex output that cannot be parsed into a verdict fails the task. It is
  never read as approval.
- If either CLI turns out not to be logged in, the whole run pauses with a
  banner rather than draining the queue — every task would have failed the
  same way. Log in, then press **Resume run**.
