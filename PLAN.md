# Overseer Plan-Defect Fixes — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix thirteen defects in `docs/superpowers/plans/2026-08-06-overseer.md` — three that stop its Go code compiling, seven that would ship wrong runtime behaviour, and three that make the plan self-inconsistent — before anyone executes it.

**Architecture:** This repository contains **no implementation code yet**. It holds a design spec, one 11,868-line implementation plan whose steps embed complete Go files and test files, and three recorded JSONL fixtures. Every "bug" below is therefore a defect in Go source or test source that is currently *inside the plan document*. The fix is to edit those embedded snippets — and the two Interfaces/spec statements that contradict them — so that the plan an implementer follows produces correct code the first time. This is the same workflow the last five commits used (`Fix three verify-gate defects found in review`, `Fix five review findings`, …).

**Tech Stack:** Markdown (the plan and the spec). The embedded code is Go 1.26 with `modernc.org/sqlite` and `gopkg.in/yaml.v3`. No build system runs in this repo today.

## Global Constraints

- **Only two files change:** `docs/superpowers/plans/2026-08-06-overseer.md` and `docs/superpowers/specs/2026-08-06-overseer-design.md`. Do not create `internal/`, `cmd/`, `go.mod`, or any Go file — the plan is not being executed here, only corrected.
- **Anchor edits on quoted text, not line numbers.** Line numbers below are as of commit `184c632` and shift as soon as the first edit lands. Every edit names a unique string to search for.
- **Do not renumber the plan's tasks.** Task 16 and Task 17 are referenced by the design spec and by commit messages. Edits go inside the existing tasks.
- **Preserve the plan's voice.** It explains *why* each non-obvious choice exists. Every fix that changes behaviour must carry a comment saying what would break without it, in the same register as the surrounding comments.
- **Keep the test-first shape.** Where a fix changes behaviour, the same task adds the test that fails against the old code and passes against the new one. A behaviour fix with no test is an incomplete fix.
- `testdata/claude-stream.jsonl`, `testdata/codex-stream.jsonl` and `testdata/codex-failed.jsonl` were checked line-by-line against the plan's Step 1 fixture listings and against `json.loads`: they match exactly and are valid JSONL. **Do not touch them.**
- `docs/superpowers/validated-sandbox-profile.sh` is checked-in evidence from a validation run, not code. **Do not touch it.**

---

## Note on bubblewrap in this workspace

Every file here is world-readable (`mode 664`, owner `kal`), but any tool that wraps its file access in a bubblewrap namespace fails to start:

```
$ cat /proc/self/uid_map                    #   1000  0  1   — single-entry map
$ unshare --user --map-root-user true
unshare: write failed /proc/self/uid_map: Operation not permitted
```

This session is already inside an unprivileged user namespace with a one-entry `uid_map`, so nesting another is denied regardless of what it is asked to mount. Reading the files directly works. Fixing the nesting is the outer harness's job (`newuidmap`/`/etc/subuid`), not this repository's.

It is recorded here because it is a live instance of the condition **Task 16's `Probe` exists to detect**: `bwrap` is installed (0.11.1) and `kernel.unprivileged_userns_clone` is `1`, yet namespace creation still fails. That is exactly why the plan probes by *creating* a namespace rather than by checking that the binary exists, and it is the evidence behind item 5 below and Task 10 Step 5.

## What a reviewer needs to know before judging this

1. **There is nothing to run.** `go test` does not exist here. The proof that a fix is right is (a) the regression test the fix adds *into the plan*, which the future implementer will run, and (b) a `grep` over the document confirming the edit landed. Each task below gives both. Where the fix is a regex or a classifier, I ran the proposed replacement against both the old and the new test tables in a scratch module and recorded the output — those results are quoted inline so a reviewer does not have to take the regex on faith.

2. **`internal/loop` is a pure function and `internal/agent` is the only package that knows a CLI exists.** Several fixes below deliberately keep behaviour changes inside `internal/agent` (event classification) rather than teaching the engine about rate limits, because that boundary is the plan's main structural claim.

3. **The single most important invariant in the system is "unparseable Codex output is never approval."** Task 7 below is the one defect that can violate it: the plan mounts `<runs>/<slug>` read-write for *both* agents, and stores the reviewer's `--output-last-message` verdict there. The agent under review can write the file that decides whether it passed. That is worth reading first if you only read one task.

4. **Severity ordering is deliberate.** Tasks 1–2 are compile/never-executes failures and must land first; a reviewer can reject Task 5 (fingerprint normalisation) on taste without blocking anything else.

5. **One empirical caveat, not a defect.** On this machine `bwrap` is present (bubblewrap 0.11.1) but `Probe` fails: `bwrap: No permissions to create a new namespace, likely because the kernel does not allow non-privileged user namespaces.` This is the same failure that broke a previous review round's tooling, as described above. The plan's `sandbox: auto` handles it correctly by downgrading to `Passthrough` with a loud note — the design is right. But the plan's final Verification checklist demands "The board shows `sandbox: bwrap (auto)`, not an `UNSANDBOXED` warning", which cannot hold in this environment. Task 10 makes that item conditional rather than absolute. Task 16's claim that the profile was validated with a real `claude -p` was made on the target host and is not re-litigated here.

---

## Defect inventory

| # | Task | Where in the plan | Effect |
|---|---|---|---|
| D1 | 1 | Task 16 Step 1, `TestSpecAddPreservesOrder` | `sandbox_test.go` does not compile: `Mount.Path` does not exist |
| D2 | 1 | Task 16 Step 10, `TestPrepareAgentStateSeeds…` | `engine/sandbox_test.go` does not compile: `json.Valid` with no `encoding/json` import |
| D3 | 1 | Task 16 Step 8, `stubWrapper` | `TestRunAppliesTheSandboxWrapper` asserts `calls() == 0` against a method hardcoded to return 1 — the assertion can never fail |
| D4 | 2 | Task 10 `harness.submit`, used by Task 17 | Three of Task 17's verify tests never exercise the verify gate: the task rows they create have an empty `VerifyCommand`, so `toLoop` sets `Verify: false` |
| D5 | 2 | Task 10 `newHarness`, Task 12 `newTestServer` | Engine and web suites inherit `Sandbox: "auto"`, so every test behaves differently depending on whether `bwrap` works on the host |
| D6 | 3 | Task 4 `ParseClaudeLine`, Task 6 `consume` | Any rate-limit status other than the literal `allowed` sets `ErrMsg`; `consume` keeps the first `ErrMsg`; a warning-level advisory therefore fails a turn that completed successfully — after burning all three retries, because "rate limit" is a retryable marker |
| D7 | 4 | Task 6 `retryableMarkers`, Task 14 `authMarkers` | Bare substrings: `"500"` matches `v1.502.0`, `"login"` matches `/code/login-service`. A false `IsAuthFailure` pauses **every** task in the run |
| D8 | 5 | Task 17 `failureLine` | Go compiler diagnostics and `make` errors match nothing, so every such failure normalises to `exited N`; two *different* compile errors fingerprint identically and the task is escalated as "oscillating" on iteration 2 |
| D9 | 6 | Task 10 `LastBlockingFindings` | Filters `agent = 'codex'`, so after a crash during a verify-triggered exec turn recovery replays a stale code review, or loses the session entirely |
| D10 | 7 | Task 10 `runCodex`, Task 16 `sandboxSpec` | The verdict file is never removed before a review and lives in a directory the implementing agent has mounted read-write |
| D11 | 8 | Task 17 `runVerify` | Under a real sandbox, verify's required `--bind` sources may not exist; bubblewrap aborts |
| D12 | 9 | Task 11 `Submit` | `blocking_severity` is validated in `ParseBatch` but not in `Submit`, which is what `POST /tasks` calls |
| D13 | 10 | Several | Stale comment, four out-of-date Interfaces blocks, 25 wrong test counts, one wrong rationale, one unachievable checklist item |

---

## File Structure

| Path | Responsibility in this change |
|---|---|
| `docs/superpowers/plans/2026-08-06-overseer.md` | All thirteen defects. Edits are confined to the embedded Go/test snippets, the per-task **Interfaces** blocks, and the `Expected:` lines. |
| `docs/superpowers/specs/2026-08-06-overseer-design.md` | One row of the Sandbox mount table and one sentence in the Sandbox section, both made wrong by Task 7's fix. |

---

## Task 1: Make the plan's sandbox test code compile

Three defects in Task 16's embedded test files. All three are mechanical, and none of them can be caught by reading the plan's prose — they only show up when the code is typed out.

**Files:**
- Modify: `docs/superpowers/plans/2026-08-06-overseer.md` — Task 16 Interfaces block (~line 9250), Step 1 `TestSpecAddPreservesOrder` (~line 9297), Step 8 `stubWrapper` (~line 10010), Step 10 test imports (~line 10253)

**Interfaces:**
- Consumes: nothing.
- Produces: nothing new. Corrects the documented shape of `sandbox.Mount` to match `internal/sandbox/sandbox.go` as the plan already implements it: `Src string`, `Dest string`, `Write bool`, `Optional bool`.

- [ ] **Step 1: Fix the `Mount` Interfaces line**

Find this line in Task 16's **Interfaces / Produces** block:

```
  - `sandbox.Mount` struct: `Path string`, `Write bool`
```

Replace it with:

```
  - `sandbox.Mount` struct: `Src string` (host path), `Dest string` (path inside; differs from `Src` when a per-task directory stands in for a real one), `Write bool`, `Optional bool` (skipped when `Src` is absent, because bubblewrap aborts on a missing `--bind` source)
```

In the same block, find:

```
  - `(Spec).Add(path string, write bool) Spec`
```

Replace it with:

```
  - `(Spec).Add(path string, write bool) Spec` — required mount at the same path
  - `(Spec).AddAt(src, dest string, write bool) Spec` — required mount that appears at `dest` inside
  - `(Spec).AddOptional(path string, write bool) Spec` — `--bind-try`; skipped when the source is absent
  - `sandbox.EnsureDirs(paths ...string) error` — creates required writable mount sources before wrapping
```

- [ ] **Step 2: Fix `TestSpecAddPreservesOrder`**

In Task 16 Step 1, find:

```go
	if s.Mounts[1].Path != "/home/u/.claude/settings.json" {
```

Replace with:

```go
	if s.Mounts[1].Dest != "/home/u/.claude/settings.json" {
```

`Spec.Add` calls `AddAt(path, path, write)`, so `Src` and `Dest` are equal here; asserting on `Dest` is the one that stays true if the test is later extended to cover `AddAt`.

- [ ] **Step 3: Fix the missing import in the engine sandbox test**

In Task 16 Step 10, `internal/engine/sandbox_test.go` uses `json.Valid(raw)` inside `TestPrepareAgentStateSeedsAWritableClaudeJSONCopy` but never imports the package. Find:

```go
import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"overseer/internal/loop"
	"overseer/internal/sandbox"
)
```

Replace with:

```go
import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"overseer/internal/loop"
	"overseer/internal/sandbox"
)
```

- [ ] **Step 4: Make `stubWrapper` actually count calls**

In Task 16 Step 8, find the whole stub:

```go
type stubWrapper struct {
	bin string
	n   *int
}

func (s stubWrapper) Wrap(_ string, args []string, _ sandbox.Spec) (string, []string) {
	if s.n != nil {
		*s.n++
	}
	return s.bin, args
}
func (stubWrapper) Name() string { return "stub" }
func (s stubWrapper) calls() int { return 1 }
```

Replace with:

```go
// stubWrapper stands in for a real sandbox so the runner's wiring can be
// tested on a host with no usable bwrap. It counts calls through a pointer
// because Wrapper is satisfied by value: a value receiver mutating a plain
// int field would increment a copy and the count would always read zero.
type stubWrapper struct {
	bin string
	n   *int
}

func newStubWrapper(bin string) stubWrapper {
	return stubWrapper{bin: bin, n: new(int)}
}

func (s stubWrapper) Wrap(_ string, args []string, _ sandbox.Spec) (string, []string) {
	*s.n++
	return s.bin, args
}

func (stubWrapper) Name() string { return "stub" }

func (s stubWrapper) calls() int { return *s.n }
```

And in `TestRunAppliesTheSandboxWrapper`, find:

```go
	stub := stubWrapper{bin: bin}
```

Replace with:

```go
	stub := newStubWrapper(bin)
```

- [ ] **Step 5: Prove the edits landed**

```bash
P=docs/superpowers/plans/2026-08-06-overseer.md
grep -c 'Mounts\[1\].Path' "$P"          # expect 0
grep -c 'Mounts\[1\].Dest' "$P"          # expect 1
grep -c 'func (s stubWrapper) calls() int { return 1 }' "$P"   # expect 0
grep -c 'func newStubWrapper' "$P"       # expect 1
grep -c '"encoding/json"' "$P"           # expect 1
grep -c 'sandbox.Mount` struct: `Path string`' "$P"            # expect 0
```

Expected: `0 1 0 1 1 0`.

Then read the three edited snippets end to end and confirm every identifier they use is defined: `Mount.Dest` (defined in Step 3 of Task 16), `json.Valid` (now imported), `newStubWrapper` (now defined above its first use in the file order the plan lays out).

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/plans/2026-08-06-overseer.md
git commit -m "fix(plan): make the sandbox test snippets compile and assert something"
```

---

## Task 2: Make the verify gate's own tests actually exercise it

**D4** is the most consequential defect here: Task 17 adds a verify gate and eleven tests for it, and three of the four tests that drive a whole task through the gate never turn the gate on. **D5** is the reason those tests are also non-deterministic across hosts, so both are fixed together — they touch the same two helpers.

`harness.submit` builds a `store.Task` by hand and never sets `VerifyCommand`. `toLoop` derives `Verify: t.VerifyCommand != ""`, so `loop.Next` skips `StateVerifying` entirely. `TestVerifyGatePassesAndTaskCompletes` then fails at `t.Fatal("no verify step recorded")`, and `TestFailingVerifyIsFedBackAndBlocksThePR` fails because the task reaches `done` and opens a PR — which is exactly the bug that test exists to catch.

**Files:**
- Modify: `docs/superpowers/plans/2026-08-06-overseer.md` — Task 10 Step 7 `newHarness` and `harness.submit` (~lines 5184–5218), Task 12 Step 1 `newTestServer` (~line 6979)

**Interfaces:**
- Consumes: `config.Config.Sandbox`, `config.Config.VerifyCommand` (Task 17), `store.Task.VerifyCommand` (Task 17).
- Produces: no new API. `harness.submit` gains the daemon's configured verify command; both test harnesses pin `sandbox: off`.

- [ ] **Step 1: Pin the sandbox mode in the engine harness**

In Task 10 Step 7, find:

```go
	cfg.StepTimeout = 30 * time.Second
	cfg.MaxParallel = 2
```

Replace with:

```go
	cfg.StepTimeout = 30 * time.Second
	cfg.MaxParallel = 2
	// Pinned, not inherited. config.Default() asks for "auto", which resolves
	// to bwrap on a host where unprivileged user namespaces work and to
	// Passthrough where they do not — so every engine test would take a
	// different path on different machines, and the fake agent scripts under
	// t.TempDir() would be mounted or not depending on the kernel. The two
	// tests that need a real sandbox set eng.Sandbox themselves.
	cfg.Sandbox = "off"
```

- [ ] **Step 2: Carry the verify command onto tasks the harness creates**

In Task 10 Step 7, find:

```go
func (h *harness) submit(t *testing.T, goal string) store.Task {
	t.Helper()
	task, err := h.st.CreateTask(context.Background(), store.Task{
		Slug: worktree.Slugify(goal), RepoPath: h.repo, Goal: goal,
		State: string(loop.StateQueued), MaxIterations: 10, BlockingSeverity: "any",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}
```

Replace with:

```go
// submit creates a queued task directly, bypassing Engine.Submit's repository
// validation. It mirrors what Submit persists, including the daemon's verify
// command: toLoop derives Task.Verify from VerifyCommand being non-empty, so a
// task created without it silently skips the verify gate and every test that
// sets Cfg.VerifyCommand would pass while proving nothing.
func (h *harness) submit(t *testing.T, goal string) store.Task {
	t.Helper()
	task, err := h.st.CreateTask(context.Background(), store.Task{
		Slug: worktree.Slugify(goal), RepoPath: h.repo, Goal: goal,
		State: string(loop.StateQueued), MaxIterations: 10, BlockingSeverity: "any",
		VerifyCommand: h.eng.Cfg.VerifyCommand,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}
```

Note the ordering this depends on: every Task 17 test that uses the gate sets `h.eng.Cfg.VerifyCommand` **before** calling `h.submit`. Check each one when applying this edit — `TestVerifyGatePassesAndTaskCompletes`, `TestFailingVerifyIsFedBackAndBlocksThePR`, `TestVerifyRecoversAfterTheAgentFixesIt` and `TestNoVerifyCommandKeepsTheOldBehaviour` all already do.

- [ ] **Step 3: Add the test that catches this class of mistake**

Append to `internal/engine/verify_test.go` in Task 17 Step 4, before `TestVerifyGatePassesAndTaskCompletes`:

```go
func TestHarnessSubmitCarriesTheVerifyCommand(t *testing.T) {
	// Guards the wiring the rest of this file depends on. Without it every
	// gate test below runs a task with Verify false and passes vacuously.
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	h.eng.Cfg.VerifyCommand = "true"

	task := h.submit(t, "carries verify")
	if task.VerifyCommand != "true" {
		t.Fatalf("VerifyCommand = %q, want the daemon's configured command", task.VerifyCommand)
	}
	if !toLoop(task).Verify {
		t.Error("toLoop did not set Verify; the gate would be skipped")
	}
}
```

- [ ] **Step 4: Pin the sandbox mode in the web harness**

In Task 12 Step 1, find:

```go
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
```

Replace with:

```go
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	// engine.New probes bwrap when Sandbox is "auto". The web tests never run
	// an agent, so pin the mode: probing spawns a subprocess per test and its
	// result varies by host.
	cfg.Sandbox = "off"
```

- [ ] **Step 5: Prove the edits landed**

```bash
P=docs/superpowers/plans/2026-08-06-overseer.md
grep -c 'VerifyCommand: h.eng.Cfg.VerifyCommand' "$P"   # expect 1
grep -c 'cfg.Sandbox = "off"' "$P"                      # expect 2
grep -c 'func TestHarnessSubmitCarriesTheVerifyCommand' "$P"   # expect 1
```

Expected: `1 2 1`.

Then trace by hand: with `VerifyCommand` set, `toLoop` returns `Verify: true`; `loop.Next` in `StateExecuting` returns `ActVerify` and `StateVerifying`; `dispatch` routes `ActVerify` to `runVerify`; `runVerify` records a step with `Agent: "verify"`. That is the chain `TestVerifyGatePassesAndTaskCompletes` asserts on, and it is broken at the first link today.

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/plans/2026-08-06-overseer.md
git commit -m "fix(plan): verify-gate tests must enable the gate, and pin sandbox mode in test harnesses"
```

---

## Task 3: A rate-limit advisory must not fail a turn that succeeded

**D6.** `ParseClaudeLine` sets `ErrMsg` for *any* `rate_limit_event` whose status is not the literal string `"allowed"`. `Runner.consume` keeps the first non-empty `ErrMsg` it sees, and both `runClaude` and `runCodex` treat a non-empty `Result.ErrMsg` as `Outcome{Failed: true}` — which `loop.Next` turns straight into `ActFail`.

So a quota-warning event emitted mid-run poisons a turn whose `result` event says `"subtype":"success"`. Worse, the message it writes contains the substring `rate limit`, which `IsRetryable` matches, so `runAgent` retries the whole turn up to three times with exponential backoff — spending real tokens each time — before failing the task.

The fixture at `testdata/claude-stream.jsonl` only exercises `"status":"allowed"`, which is why this survived review.

Two layers, because guessing the CLI's exact status vocabulary is what caused the bug in the first place: narrow the parser to statuses that are genuinely not allowed, and make `consume` treat a rate-limit message as advisory when the run went on to report success.

**Files:**
- Modify: `docs/superpowers/plans/2026-08-06-overseer.md` — Task 4 Step 5 `ParseClaudeLine` (~line 1554), Task 4 Step 2 tests (~line 1330), Task 6 Step 3 `consume` (~line 2786), Task 6 Step 1 tests (~line 2495)

**Interfaces:**
- Consumes: `agent.Event`, `agent.Result`.
- Produces: no signature change. `Event.Kind == EventRateLimit` keeps its meaning; `Event.ErrMsg` on such an event now means "the request was refused", not "something about quota was mentioned".

- [ ] **Step 1: Narrow the parser**

In Task 4 Step 5, find:

```go
	case "rate_limit_event":
		ev.Kind = EventRateLimit
		if cl.RateLimitInfo.Status != "" && cl.RateLimitInfo.Status != "allowed" {
			ev.ErrMsg = fmt.Sprintf("rate limit %s (%s)",
				cl.RateLimitInfo.Status, cl.RateLimitInfo.RateLimitType)
		}
```

Replace with:

```go
	case "rate_limit_event":
		ev.Kind = EventRateLimit
		// Only a status that is not some flavour of "allowed" is a failure.
		// The CLI emits this event informationally as a quota is approached,
		// and treating every non-"allowed" value as an error fails a turn
		// whose result event reports success — after three retries, because
		// the message it produces contains "rate limit", which IsRetryable
		// matches. Prefix-matching covers "allowed" and any "allowed_*"
		// warning variant without having to enumerate them.
		if s := cl.RateLimitInfo.Status; s != "" && !strings.HasPrefix(s, "allowed") {
			ev.ErrMsg = fmt.Sprintf("rate limit %s (%s)", s, cl.RateLimitInfo.RateLimitType)
		}
```

`strings` is already imported by `claude.go`.

- [ ] **Step 2: Add the parser regression test**

In Task 4 Step 2, immediately after `TestParseClaudeRateLimitExhausted`, add:

```go
func TestParseClaudeRateLimitWarningIsNotAnError(t *testing.T) {
	// An advisory emitted as quota runs low. Failing the turn here would
	// fail a run that goes on to succeed, and would cost three retries
	// first because "rate limit" reads as retryable.
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"five_hour"},"session_id":"s1"}`
	ev, err := ParseClaudeLine([]byte(line))
	if err != nil {
		t.Fatalf("ParseClaudeLine: %v", err)
	}
	if ev.Kind != EventRateLimit {
		t.Fatalf("Kind = %q, want EventRateLimit", ev.Kind)
	}
	if ev.ErrMsg != "" {
		t.Errorf("ErrMsg = %q, want empty for a warning-level status", ev.ErrMsg)
	}
}
```

- [ ] **Step 3: Make `consume` treat a rate-limit message as advisory**

In Task 6 Step 3, find the body of `consume` from its `var writeErr error` declaration through the `case EventError, EventRateLimit:` arm, and replace:

```go
	var writeErr error
```

with:

```go
	var writeErr error

	// A rate-limit message is held aside rather than written straight into
	// res. It is the one event kind that routinely arrives on a run that then
	// completes normally, and res.ErrMsg is what fails the task.
	var rateLimitMsg string
	var sawSuccessfulResult bool
```

then find:

```go
		case EventResult:
			res.CostUSD += ev.CostUSD
			res.InputTokens += ev.InputTokens
			res.OutputTokens += ev.OutputTokens
			if ev.ErrMsg != "" && res.ErrMsg == "" {
				res.ErrMsg = ev.ErrMsg
			}
		case EventError, EventRateLimit:
			if ev.ErrMsg != "" && res.ErrMsg == "" {
				res.ErrMsg = ev.ErrMsg
			}
		}
```

and replace it with:

```go
		case EventResult:
			res.CostUSD += ev.CostUSD
			res.InputTokens += ev.InputTokens
			res.OutputTokens += ev.OutputTokens
			if ev.ErrMsg != "" {
				if res.ErrMsg == "" {
					res.ErrMsg = ev.ErrMsg
				}
			} else {
				sawSuccessfulResult = true
			}
		case EventError:
			if ev.ErrMsg != "" && res.ErrMsg == "" {
				res.ErrMsg = ev.ErrMsg
			}
		case EventRateLimit:
			if ev.ErrMsg != "" && rateLimitMsg == "" {
				rateLimitMsg = ev.ErrMsg
			}
		}
```

Finally, find the tail of `consume`:

```go
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read agent output: %w", err)
	}
	// Reported only after the pipe is fully drained.
	return writeErr
```

and replace it with:

```go
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read agent output: %w", err)
	}
	// A refused rate limit fails the run only when nothing better happened:
	// no other error, and no result event reporting success. A run that
	// completed is not a failed run, whatever a mid-stream advisory said.
	if res.ErrMsg == "" && rateLimitMsg != "" && !sawSuccessfulResult {
		res.ErrMsg = rateLimitMsg
	}
	// Reported only after the pipe is fully drained.
	return writeErr
```

- [ ] **Step 4: Add the two runner regression tests**

In Task 6 Step 1, after `TestRunInvokesOnEventForEachLine`, add:

```go
func TestRunIgnoresARateLimitEventBeforeASuccessfulResult(t *testing.T) {
	// The CLI reports quota state mid-run. A turn that then reports success
	// must not be failed — and must not be retried three times first,
	// because "rate limit" reads as retryable.
	bin := writeFakeAgent(t, `
cat <<'EOF'
{"type":"system","subtype":"init","session_id":"s"}
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour"},"session_id":"s"}
{"type":"result","subtype":"success","is_error":false,"session_id":"s","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}
EOF`)
	res, err := NewClaudeRunner(bin).Run(context.Background(), RunSpec{
		Args: []string{"x"}, Dir: t.TempDir(),
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Timeout:        30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ErrMsg != "" {
		t.Errorf("ErrMsg = %q; a completed run must not be failed by a rate-limit advisory", res.ErrMsg)
	}
	if res.Retryable {
		t.Error("Retryable = true for a run that succeeded")
	}
}

func TestRunReportsARateLimitThatEndedTheRun(t *testing.T) {
	// No result event: the run really was cut short, so the message must
	// survive and be retryable.
	bin := writeFakeAgent(t, `
echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour"},"session_id":"s"}'`)
	res, err := NewClaudeRunner(bin).Run(context.Background(), RunSpec{
		Args: []string{"x"}, Dir: t.TempDir(),
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Timeout:        30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.ErrMsg, "rate limit") {
		t.Errorf("ErrMsg = %q, want the rate-limit rejection", res.ErrMsg)
	}
	if !res.Retryable {
		t.Error("a rate-limit rejection must be retryable")
	}
}
```

- [ ] **Step 5: Prove the edits landed**

```bash
P=docs/superpowers/plans/2026-08-06-overseer.md
grep -c 'strings.HasPrefix(s, "allowed")' "$P"                       # expect 1
grep -c 'sawSuccessfulResult' "$P"                                   # expect 3
grep -c 'TestParseClaudeRateLimitWarningIsNotAnError' "$P"           # expect 1
grep -c 'TestRunIgnoresARateLimitEventBeforeASuccessfulResult' "$P"  # expect 1
grep -c 'case EventError, EventRateLimit:' "$P"                      # expect 0
```

Expected: `1 3 1 1 0`.

Then confirm the existing `TestParseClaudeRateLimitExhausted` (status `"rejected"`) still expects a non-empty `ErrMsg`: `"rejected"` has no `"allowed"` prefix, so it still does.

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/plans/2026-08-06-overseer.md
git commit -m "fix(plan): a rate-limit advisory must not fail a completed agent turn"
```

---

## Task 4: Stop the retry and auth classifiers matching incidental text

**D7.** Both classifiers are `strings.Contains` over a list of bare substrings.

`retryableMarkers` contains `"500"`, `"502"`, `"503"`, `"504"`, `"429"`. `Result.ErrMsg` is built as `"%s: %v: %s"` from the binary path, the wait error, and 500 bytes of stderr — so a module path like `.../x@v1.502.0/bin/claude` makes a permanently broken invocation look transient and burns three retries with backoff.

`authMarkers` contains `"login"`, `"401"`, `"403"`. A repository at `/home/kal/code/login-service`, or a test named `TestLoginHandler` appearing in stderr, triggers `IsAuthFailure` — and `runAgent` responds by calling `e.Pause(...)`, which stops **every** task in the run behind a banner until an operator clears it. That is the widest blast radius of any defect in the plan.

The replacements below were run against the plan's existing test tables plus new false-positive cases in a scratch module. Output: no failures — every case in `TestIsRetryable` and `TestIsAuthFailure` still classifies as the plan expects, and all six new adversarial cases classify correctly.

**Files:**
- Modify: `docs/superpowers/plans/2026-08-06-overseer.md` — Task 6 Step 3 `retryableMarkers`/`IsRetryable` (~line 2850), Task 6 Step 1 `TestIsRetryable` (~line 2579), Task 14 Step 2 `authfail.go` (~line 8416), Task 14 Step 1 `TestIsAuthFailure` (~line 8371)

**Interfaces:**
- Consumes: nothing.
- Produces: `agent.IsRetryable(msg string) bool` and `agent.IsAuthFailure(msg string) bool` keep their signatures. Both gain a regex for HTTP-status-shaped tokens; both lose their bare numeric and bare-word markers.

- [ ] **Step 1: Replace `IsRetryable`**

In Task 6 Step 3, find:

```go
// retryableMarkers are substrings that indicate a transient failure.
var retryableMarkers = []string{
	"rate limit", "rate_limit", "429",
	"500", "502", "503", "504",
	"connection reset", "connection refused", "broken pipe",
	"timeout", "deadline exceeded", "temporarily unavailable",
	"overloaded", "eof",
}

// IsRetryable reports whether an agent error message describes a transient
// condition worth retrying. Authentication and usage errors are not
// retryable: repeating them wastes time and money.
func IsRetryable(msg string) bool {
	if msg == "" {
		return false
	}
	l := strings.ToLower(msg)
	for _, m := range retryableMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}
```

Replace with:

```go
// httpRetryStatus matches a 429 or 5xx reported the way the CLIs report one:
// as a standalone token. A bare strings.Contains("500") also matches a module
// version like v1.502.0 or a temp directory named .../Test429042/, and
// Result.ErrMsg is assembled from the binary path plus 500 bytes of stderr —
// so a permanently broken invocation would look transient and burn three
// retries with backoff. The boundary classes deliberately exclude '.', which
// is what version strings use.
var httpRetryStatus = regexp.MustCompile(`(^|[\s(\[])(429|50[0-4])([\s):\].,]|$)`)

// retryableMarkers are substrings that indicate a transient failure. Each one
// is a phrase rather than a fragment: "eof" alone matched inside unrelated
// words, so it is spelled out.
var retryableMarkers = []string{
	"rate limit", "rate_limit",
	"connection reset", "connection refused", "broken pipe",
	"timeout", "deadline exceeded", "temporarily unavailable",
	"overloaded", "unexpected eof",
}

// IsRetryable reports whether an agent error message describes a transient
// condition worth retrying. Authentication and usage errors are not
// retryable: repeating them wastes time and money.
func IsRetryable(msg string) bool {
	if msg == "" {
		return false
	}
	l := strings.ToLower(msg)
	if httpRetryStatus.MatchString(l) {
		return true
	}
	for _, m := range retryableMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}
```

Add `"regexp"` to `runner.go`'s import block, which currently reads `bufio`, `context`, `errors`, `fmt`, `io`, `os`, `os/exec`, `path/filepath`, `strings`, `syscall`, `time`.

- [ ] **Step 2: Extend `TestIsRetryable` with the false-positive cases**

In Task 6 Step 1, find:

```go
	fatal := []string{
		"not logged in",
		"invalid_json_schema",
		"unknown flag: --nope",
		"",
	}
```

Replace with:

```go
	fatal := []string{
		"not logged in",
		"invalid_json_schema",
		"unknown flag: --nope",
		"",
		// Digits that are not a status. Each of these was retryable when the
		// markers were bare numbers, so a permanent failure cost three
		// retries with exponential backoff before the task failed.
		"start /tmp/TestRun429042/001/claude: no such file or directory",
		"start /home/u/go/pkg/mod/x@v1.502.0/bin/claude: permission denied",
		"the model produced 5000 tokens",
	}
```

- [ ] **Step 3: Replace `IsAuthFailure`**

In Task 14 Step 2, find the whole body of `internal/agent/authfail.go` from `import "strings"` to the closing brace, and replace it with:

```go
import (
	"regexp"
	"strings"
)

// httpAuthStatus matches a 401 or 403 as a standalone token. As with the
// retry classifier, a bare substring match also fires on a module version
// like v1.403.2.
var httpAuthStatus = regexp.MustCompile(`(^|[\s(\[])(401|403)([\s):\].,]|$)`)

// authMarkers are phrases that indicate the agent CLI is not authenticated.
//
// They are phrases, not fragments, because the consequence of a false
// positive is out of proportion to everything else in this package: the
// engine responds by pausing the WHOLE run behind a banner. A bare "login"
// marker fired on a repository path such as /home/kal/code/login-service and
// on any stderr line mentioning a test called TestLogin, halting every task
// in flight until an operator noticed.
var authMarkers = []string{
	"not logged in", "not authenticated", "authentication failed",
	"authentication error", "invalid api key", "invalid_api_key",
	"api key not found", "credentials not found", "token has expired",
	"expired token", "unauthorized", "forbidden",
	"claude login", "codex login", "run /login",
}

// IsAuthFailure reports whether an agent error means the CLI is not
// authenticated. Every task would fail identically, so the engine pauses the
// whole run rather than draining the queue.
func IsAuthFailure(msg string) bool {
	if msg == "" {
		return false
	}
	l := strings.ToLower(msg)
	if httpAuthStatus.MatchString(l) {
		return true
	}
	for _, m := range authMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Extend `TestIsAuthFailure` with the false-positive cases**

In Task 14 Step 1, find:

```go
	notAuth := []string{
		"429 Too Many Requests",
		"invalid_json_schema",
		"step timeout after 30m",
		"unknown flag: --nope",
		"",
	}
```

Replace with:

```go
	notAuth := []string{
		"429 Too Many Requests",
		"invalid_json_schema",
		"step timeout after 30m",
		"unknown flag: --nope",
		"",
		// Incidental matches. Each of these paused the entire run when the
		// markers were bare words and bare numbers.
		"start /home/kal/code/login-service/bin/claude: no such file or directory",
		"--- FAIL: TestLoginHandler (0.01s)",
		"cannot find module x@v1.403.2",
	}
```

- [ ] **Step 5: Prove the edits landed**

```bash
P=docs/superpowers/plans/2026-08-06-overseer.md
grep -c 'httpRetryStatus = regexp.MustCompile' "$P"   # expect 1
grep -c 'httpAuthStatus = regexp.MustCompile' "$P"    # expect 1
grep -c '"rate limit", "rate_limit", "429",' "$P"     # expect 0
grep -c '"not logged in", "login", "401", "403"' "$P" # expect 0
grep -c 'login-service' "$P"                          # expect 2 (comment + test case)
```

Expected: `1 1 0 0 2`.

The behavioural proof is the extended tables in Steps 2 and 4: each new case fails against the current markers and passes against the replacements. This was confirmed by running both implementations over both tables — all old cases keep their classification, all six new cases classify correctly.

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/plans/2026-08-06-overseer.md
git commit -m "fix(plan): classify retryable and auth failures on phrases, not fragments"
```

---

## Task 5: Teach verify-failure fingerprinting about compilers and make

**D8.** `NormalizeFailureOutput` exists so that two runs of the same failure hash identically, which is what makes oscillation detection fire in three rounds instead of ten. Its `failureLine` regex recognises `--- FAIL`, `FAIL:`, `panic:`, `error:` and a few words — and nothing else.

Running the plan's regex over real output confirms the gap:

```
go compile   ("main.go:7:2: undefined: foo")           -> []
go compile2  ("main.go:9:5: undefined: bar")           -> []
make         ("make: *** [Makefile:12: test] Error 2") -> []
go test      ("--- FAIL: TestThing (0.03s)...")        -> ["--- FAIL: TestThing (N.Ns)" "FAIL example.com/pkg N.Ns"]
```

When nothing matches, `VerifyFindings` falls back to `[]string{fmt.Sprintf("exited %d", exitCode)}`. So *every* compile failure and *every* `make` failure produces the identical summary `exited 2` — and `Verdict.Fingerprint` hashes the summary. A task that fixes one compile error and introduces a different one is therefore escalated as "verify keeps failing the same way" on iteration 2, even though it is making steady progress. That is the exact opposite of what the normalisation was written for.

The fix adds a compiler-diagnostic pattern, broadens `error:` to `error\b` so `Error 2` matches, and replaces the exit-code-only fallback with the normalised last five non-empty lines — stable across runs of one failure, distinct between different failures.

Verified output of the proposed implementation:

```
go compile A   ("main.go:7:2: undefined: foo", 1.482s, /tmp/build-9182) -> ["main.go:N:N: undefined: foo"]
go compile A'  (same error, 9.001s, /tmp/build-1)                       -> ["main.go:N:N: undefined: foo"]   <- identical
go compile B   ("main.go:9:5: undefined: bar")                          -> ["main.go:N:N: undefined: bar"]   <- distinct
make           ("make: *** [Makefile:12: test] Error 2")                -> ["make: *** [Makefile:N: test] Error N"]
make2          ("make: *** [Makefile:20: lint] Error 1")                -> ["make: *** [Makefile:N: lint] Error N"]  <- distinct
go test        (0.03s run)  -> ["--- FAIL: TestThing (N.Ns)" "FAIL example.com/pkg N.Ns" "thing_test.go:N: got N, want N"]
go test        (2.71s run)  -> ["--- FAIL: TestThing (N.Ns)" "FAIL example.com/pkg N.Ns" "thing_test.go:N: got N, want N"]  <- identical
alpha/beta     (different failing tests)                                -> distinct
empty output                                                            -> []  (falls back to "exited N")
```

Every existing assertion in `TestNormalizeFailureOutputIsStableAcrossRuns`, `TestNormalizeFailureOutputSeparatesDifferentFailures`, `TestVerifyFingerprintIsStableAcrossIdenticalFailures` and `TestVerifyFindingsPutRawOutputInDetailNotSummary` still holds under this output.

**Files:**
- Modify: `docs/superpowers/plans/2026-08-06-overseer.md` — Task 17 Step 7 `failureLine`/`NormalizeFailureOutput` (~lines 11695–11735), Task 17 Step 4 tests (~line 11056)

**Interfaces:**
- Consumes: nothing.
- Produces: `engine.NormalizeFailureOutput(output string) []string` keeps its signature. It now returns a normalised tail instead of nothing when no line matches a failure pattern.

- [ ] **Step 1: Replace the patterns and the function**

In Task 17 Step 7, find:

```go
// failureLine matches the lines worth fingerprinting: test failures and
// compiler errors, rather than progress chatter.
var failureLine = regexp.MustCompile(`(?i)(^|\s)(---\s+FAIL|FAIL[:\s]|FAILED|panic:|error:|Error:|assert|expected)`)
```

Replace with:

```go
// failureLine matches the lines worth fingerprinting: test failures and
// build errors, rather than progress chatter. `error\b` rather than `error:`
// so that make's "Error 2" is caught; the alternation is case-insensitive, so
// a separate `Error:` alternative was always redundant.
var failureLine = regexp.MustCompile(`(?i)(^|\s)(---\s+FAIL|FAIL[:\s]|FAILED|panic:|error\b|assert|expected)`)

// diagnosticLine matches a compiler- or test-style diagnostic:
// "main.go:7:2: undefined: foo", "thing_test.go:14: got 3, want 4". The
// failureLine alternation misses every one of these — the words "FAIL" and
// "error" simply do not appear — so before this pattern existed, every
// compile failure normalised to nothing and fell back to the exit code. Two
// DIFFERENT compile errors then fingerprinted identically and the task was
// escalated as oscillating on its second iteration.
var diagnosticLine = regexp.MustCompile(`^[^\s:]+\.[A-Za-z0-9_]+:\d+(:\d+)?:\s`)
```

Then find the whole of `NormalizeFailureOutput`:

```go
func NormalizeFailureOutput(output string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !failureLine.MatchString(line) {
			continue
		}
		norm := tempPaths.ReplaceAllString(line, "/tmp/X")
		norm = digits.ReplaceAllString(norm, "N")
		norm = strings.Join(strings.Fields(norm), " ")
		if seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, norm)
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}
```

and replace it with:

```go
func NormalizeFailureOutput(output string) []string {
	lines := strings.Split(output, "\n")
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if failureLine.MatchString(line) || diagnosticLine.MatchString(line) {
			add(normalizeLine(line))
		}
	}

	if len(out) == 0 {
		// Nothing matched a known failure shape. Falling back to the exit code
		// alone would make every unrecognised failure hash identically, so a
		// task that swaps one broken build for a different broken build would
		// be escalated as oscillating. The normalised tail is stable across
		// runs of one failure — timings and temp paths are collapsed — and
		// distinct between different ones.
		var tail []string
		for i := len(lines) - 1; i >= 0 && len(tail) < 5; i-- {
			if s := strings.TrimSpace(lines[i]); s != "" {
				tail = append(tail, normalizeLine(s))
			}
		}
		for _, s := range tail {
			add(s)
		}
	}

	sort.Strings(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// normalizeLine collapses the parts of a line that differ between runs of the
// same failure: temporary paths, timings, line offsets and addresses.
func normalizeLine(line string) string {
	n := tempPaths.ReplaceAllString(line, "/tmp/X")
	n = digits.ReplaceAllString(n, "N")
	return strings.Join(strings.Fields(n), " ")
}
```

- [ ] **Step 2: Add the regression tests**

In Task 17 Step 4, after `TestNormalizeFailureOutputSeparatesDifferentFailures`, add:

```go
func TestNormalizeFailureOutputRecognisesCompilerDiagnostics(t *testing.T) {
	// The whole point of normalising: two runs of one compile failure must
	// hash the same, and two different compile failures must not. Before
	// diagnosticLine existed neither line matched anything, both fell back to
	// "exited N", and a task that swapped one compile error for another was
	// escalated as oscillating on iteration 2.
	first := NormalizeFailureOutput(
		"# example.com/pkg\nmain.go:7:2: undefined: foo\nbuilt in 1.482s at /tmp/build-9182")
	again := NormalizeFailureOutput(
		"# example.com/pkg\nmain.go:7:2: undefined: foo\nbuilt in 9.001s at /tmp/build-1")
	other := NormalizeFailureOutput("# example.com/pkg\nmain.go:9:5: undefined: bar")

	if len(first) == 0 {
		t.Fatal("a compiler diagnostic produced nothing to fingerprint")
	}
	if strings.Join(first, "|") != strings.Join(again, "|") {
		t.Errorf("timings changed the normalised form:\n%v\n%v", first, again)
	}
	if strings.Join(first, "|") == strings.Join(other, "|") {
		t.Errorf("two different compile errors normalised the same: %v", first)
	}
}

func TestNormalizeFailureOutputSeparatesDifferentMakeTargets(t *testing.T) {
	a := NormalizeFailureOutput("make: *** [Makefile:12: test] Error 2")
	b := NormalizeFailureOutput("make: *** [Makefile:20: lint] Error 1")
	if len(a) == 0 {
		t.Fatal("a make failure produced nothing to fingerprint")
	}
	if strings.Join(a, "|") == strings.Join(b, "|") {
		t.Errorf("two different make failures normalised the same: %v", a)
	}
}

func TestNormalizeFailureOutputFallsBackToTheTail(t *testing.T) {
	// Output in no recognised format at all. The fallback must still tell two
	// different failures apart, and must ignore what changes between runs.
	a := NormalizeFailureOutput("checking\nwidget 3 broke at /tmp/run-991 after 1.2s")
	b := NormalizeFailureOutput("checking\nwidget 3 broke at /tmp/run-114 after 8.9s")
	c := NormalizeFailureOutput("checking\ngadget 3 broke at /tmp/run-991 after 1.2s")

	if len(a) == 0 {
		t.Fatal("the fallback produced nothing to fingerprint")
	}
	if strings.Join(a, "|") != strings.Join(b, "|") {
		t.Errorf("volatile values survived the fallback:\n%v\n%v", a, b)
	}
	if strings.Join(a, "|") == strings.Join(c, "|") {
		t.Errorf("different failures shared a fallback fingerprint: %v", a)
	}
	if len(NormalizeFailureOutput("")) != 0 {
		t.Error("empty output must yield nothing, so VerifyFindings can use the exit code")
	}
}
```

- [ ] **Step 3: Prove the edits landed**

```bash
P=docs/superpowers/plans/2026-08-06-overseer.md
grep -c 'diagnosticLine = regexp.MustCompile' "$P"                       # expect 1
grep -c 'func normalizeLine' "$P"                                        # expect 1
grep -c 'panic:|error:|Error:|assert' "$P"                               # expect 0
grep -c 'TestNormalizeFailureOutputRecognisesCompilerDiagnostics' "$P"   # expect 1
```

Expected: `1 1 0 1`.

The behavioural proof is the recorded output above: the current regex returns `[]` for all three compiler and make cases, the replacement returns distinct stable values, and no existing assertion changes classification.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/plans/2026-08-06-overseer.md
git commit -m "fix(plan): fingerprint compiler and make failures instead of collapsing them to the exit code"
```

---

## Task 6: Recovery must replay the finding that actually caused the resume

**D9.** After a restart, `RunTask` re-dispatches whatever action the task's state was waiting on. For `ActClaudeExecResume` it reloads the findings from the store, because they were only ever held in memory:

```go
	SELECT id FROM steps
	WHERE task_id = ? AND phase = ? AND agent = 'codex' AND state = 'done'
	ORDER BY id DESC LIMIT 1
```

Task 17 introduces a second producer of blocking findings — the verify gate, whose steps are recorded with `Agent: "verify"`. So a task sitting in `StateExecuting` at iteration 3 *because verify failed at iteration 2* recovers by loading the last **codex** review instead:

- if an earlier code review left blocking findings, the agent is handed a stale review while the actual blocker is a failing build;
- if no code review has run yet in this phase, `LastBlockingFindings` returns nothing, and `RunTask`'s fallback rewrites the action to `ActClaudeExec` with `ResumeSessionID` cleared — throwing away the implementation session and restarting the whole exec prompt from scratch.

**Files:**
- Modify: `docs/superpowers/plans/2026-08-06-overseer.md` — Task 10 Step 6 `LastBlockingFindings` (~line 4974), Task 17 Step 6 (add the store test alongside the other Task 17 store plumbing)

**Interfaces:**
- Consumes: `store.Step.Agent`.
- Produces: `(*store.Store).LastBlockingFindings(ctx, taskID int64, phase string) ([]Finding, error)` keeps its signature; it now considers verify steps as well as reviews.

- [ ] **Step 1: Widen the query**

In Task 10 Step 6, find:

```go
// LastBlockingFindings returns the blocking findings from the most recent
// review step in the given phase. The engine uses it to rebuild a resume
// prompt after a restart, since findings are otherwise only held in memory.
func (s *Store) LastBlockingFindings(ctx context.Context, taskID int64, phase string) ([]Finding, error) {
	var stepID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM steps
		WHERE task_id = ? AND phase = ? AND agent = 'codex' AND state = 'done'
		ORDER BY id DESC LIMIT 1`, taskID, phase).Scan(&stepID)
```

Replace with:

```go
// LastBlockingFindings returns the blocking findings from the most recent
// step in the given phase that produced any — a Codex review or a failed
// verify run. The engine uses it to rebuild a resume prompt after a restart,
// since findings are otherwise only held in memory.
//
// The verify gate matters here as much as the review does. A task parked in
// StateExecuting because verify failed would, if this looked only at Codex
// steps, be handed either a stale code review or nothing at all — and
// "nothing at all" makes the engine downgrade the resume to a fresh session,
// discarding the implementation context and re-running the whole exec prompt.
func (s *Store) LastBlockingFindings(ctx context.Context, taskID int64, phase string) ([]Finding, error) {
	var stepID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM steps
		WHERE task_id = ? AND phase = ? AND agent IN ('codex', 'verify')
		  AND state = 'done'
		ORDER BY id DESC LIMIT 1`, taskID, phase).Scan(&stepID)
```

`runVerify` never sets `step.ErrMsg`, so `FinishStep` records a failing verify with `state = 'done'` — the existing `state = 'done'` filter is correct for both agents and does not change.

- [ ] **Step 2: Add the store regression test**

In Task 17 Step 6, after the sentence "In `toLoop`, set the flag:", add a new paragraph and test:

Append to `internal/store/repo_steps_test.go`:

```go
func TestLastBlockingFindingsPrefersAFailedVerifyOverAnOlderReview(t *testing.T) {
	// Recovery replays whatever caused the task to be waiting for another
	// agent turn. When verify failed after a review, the verify failure is
	// what the agent must be told about.
	ctx := context.Background()
	s := newTestStore(t)
	task := seedTask(t, s)

	review, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "exec", Iteration: 1, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep(ctx, review, []Finding{
		{Severity: "major", Summary: "old review finding", Blocking: true},
	}); err != nil {
		t.Fatal(err)
	}

	verify, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "exec", Iteration: 2, Agent: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep(ctx, verify, []Finding{
		{Severity: "critical", Summary: "`go test ./...` failed", Detail: "--- FAIL: TestX", Blocking: true},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastBlockingFindings(ctx, task.ID, "exec")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if got[0].Severity != "critical" {
		t.Errorf("Severity = %q, want the verify failure, not the older review", got[0].Severity)
	}
	if got[0].Detail == "" {
		t.Error("Detail was dropped; the agent needs the failure output to act")
	}
}
```

- [ ] **Step 3: Prove the edits landed**

```bash
P=docs/superpowers/plans/2026-08-06-overseer.md
grep -c "agent IN ('codex', 'verify')" "$P"                                  # expect 1
grep -c "agent = 'codex' AND state = 'done'" "$P"                            # expect 0
grep -c 'TestLastBlockingFindingsPrefersAFailedVerifyOverAnOlderReview' "$P" # expect 1
```

Expected: `1 0 1`.

Then confirm the existing `TestLastBlockingFindingsReturnsMostRecentReviewOnly` still holds: it creates only `codex` steps, so widening the filter cannot change its result.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/plans/2026-08-06-overseer.md
git commit -m "fix(plan): recovery must replay a failed verify, not an older review"
```

---

## Task 7: The agent under review must not be able to write the reviewer's verdict

**D10.** Read this one carefully; it is the only defect that can break the plan's stated most-important invariant.

Two facts combine:

1. `runCodex` computes `lastPath` as `<runs>/<slug>/<phase>-<iteration>-verdict.json`, runs Codex with `--output-last-message lastPath`, and then does `os.ReadFile(lastPath)`. **The file is never removed first.** If Codex exits zero without writing it — killed, re-dispatched after a crash, or a CLI change — the read succeeds and returns whatever was already there.
2. `sandboxSpec` mounts `e.runDir(task)` **read-write for both agents**, because Codex needs to write its last-message file there. Claude therefore has write access to the directory holding the verdicts that decide whether Claude's work is approved.

The reachable failure is crash recovery: a task in `plan_review` at iteration 1 already has `plan-1-verdict.json` on disk from before the crash. Recovery re-dispatches the same review, targeting the same path. A Codex run that exits zero without writing leaves the pre-crash verdict in place, and the loop converges on a review that did not happen this time. If that verdict was `approved`, the task proceeds to implementation, then to a draft PR.

The fix has three parts: put the verdicts in a directory of their own, mount that directory only for the reviewer, and remove the target file before every Codex invocation.

**Files:**
- Modify: `docs/superpowers/plans/2026-08-06-overseer.md` — Task 1 Step 4 `config.go` helpers (~line 260), Task 10 Step 9 `runCodex` and `runAgent` (~lines 5955–6030), Task 16 Step 9 `sandboxSpec` (~line 10143), Task 16 Step 10 tests, Task 10 Step 7 tests
- Modify: `docs/superpowers/specs/2026-08-06-overseer-design.md` — Sandbox mount table row (~line 460)

**Interfaces:**
- Consumes: `config.Config.DataDir`, `store.Task.Slug`.
- Produces: `(config.Config).VerdictsDir() string`; `(*Engine).verdictDir(task store.Task) string`. `runCodex`'s signature is unchanged.

- [ ] **Step 1: Add the config helper**

In Task 1 Step 4, find:

```go
// WorktreesDir is where task worktrees are created.
func (c Config) WorktreesDir() string { return filepath.Join(c.DataDir, "worktrees") }
```

Replace with:

```go
// WorktreesDir is where task worktrees are created.
func (c Config) WorktreesDir() string { return filepath.Join(c.DataDir, "worktrees") }

// VerdictsDir holds the reviewer's --output-last-message files. It is
// deliberately separate from RunsDir: the run directory is mounted into the
// implementing agent's sandbox, and a file the implementer can write is a
// file it can forge.
func (c Config) VerdictsDir() string { return filepath.Join(c.DataDir, "verdicts") }
```

- [ ] **Step 2: Move and clear the verdict file**

In Task 10 Step 9, find:

```go
func (e *Engine) runDir(task store.Task) string {
	return filepath.Join(e.Cfg.RunsDir(), task.Slug)
}
```

Replace with:

```go
func (e *Engine) runDir(task store.Task) string {
	return filepath.Join(e.Cfg.RunsDir(), task.Slug)
}

// verdictDir holds this task's Codex last-message files. Kept out of runDir
// because runDir is mounted read-write into the implementing agent's sandbox
// so that transcripts and per-task agent state have somewhere to live; the
// verdict that decides whether that agent's work is approved must not be
// writable by it.
func (e *Engine) verdictDir(task store.Task) string {
	return filepath.Join(e.Cfg.VerdictsDir(), task.Slug)
}
```

Then find the head of `runCodex`:

```go
func (e *Engine) runCodex(ctx context.Context, task *store.Task, phase, prompt string) (*loop.Outcome, error) {
	lastPath := filepath.Join(e.runDir(*task),
		fmt.Sprintf("%s-%d-verdict.json", phase, task.Iteration))
	args := agent.CodexArgs(agent.CodexOpts{
		Prompt:          prompt,
		SchemaPath:      e.SchemaPath,
		LastMessagePath: lastPath,
	})
	res, step, err := e.runAgent(ctx, task, phase, "codex", e.Codex, args)
	if err != nil {
		return nil, err
	}
	if res.ErrMsg != "" {
		return &loop.Outcome{Failed: true, ErrMsg: res.ErrMsg}, nil
	}

	raw, err := os.ReadFile(lastPath)
```

Replace it with:

```go
func (e *Engine) runCodex(ctx context.Context, task *store.Task, phase, prompt string) (*loop.Outcome, error) {
	lastPath := filepath.Join(e.verdictDir(*task),
		fmt.Sprintf("%s-%d-verdict.json", phase, task.Iteration))
	if err := sandbox.EnsureDirs(e.verdictDir(*task)); err != nil {
		return nil, err
	}
	// Cleared before every invocation. The path is derived from the phase and
	// the iteration, so a re-dispatched review after a crash — or the stricter
	// re-ask below — targets a path that may already hold a file. A Codex run
	// that exits zero without writing would otherwise have the previous
	// contents read as this round's verdict, which is exactly the silent
	// approval this whole path exists to prevent.
	if err := os.Remove(lastPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("clear stale verdict %s: %w", lastPath, err)
	}
	args := agent.CodexArgs(agent.CodexOpts{
		Prompt:          prompt,
		SchemaPath:      e.SchemaPath,
		LastMessagePath: lastPath,
	})
	res, step, err := e.runAgent(ctx, task, phase, "codex", e.Codex, args)
	if err != nil {
		return nil, err
	}
	if res.ErrMsg != "" {
		return &loop.Outcome{Failed: true, ErrMsg: res.ErrMsg}, nil
	}

	raw, err := os.ReadFile(lastPath)
```

Then find, inside the re-ask block:

```go
		args = agent.CodexArgs(agent.CodexOpts{
			Prompt: retryPrompt, SchemaPath: e.SchemaPath, LastMessagePath: lastPath,
		})
```

Replace with:

```go
		// Same reasoning as above: without this the re-ask would re-read the
		// first attempt's unparseable output and the retry would be a no-op.
		if err := os.Remove(lastPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("clear stale verdict %s: %w", lastPath, err)
		}
		args = agent.CodexArgs(agent.CodexOpts{
			Prompt: retryPrompt, SchemaPath: e.SchemaPath, LastMessagePath: lastPath,
		})
```

Add `"errors"` and `"overseer/internal/sandbox"` to `engine.go`'s import block. (`sandbox` is added anyway by Task 16 Step 9; when applying Task 7 before Task 16 in the document, add it in both places and keep the import list a single sorted block.)

- [ ] **Step 3: Mount the verdict directory for the reviewer only**

In Task 16 Step 9, find:

```go
	// Codex reads the schema and writes its last-message file itself, so
	// both need mounts. Only this task's run directory is exposed — never
	// the whole data directory, which holds the database.
	spec = spec.
		Add(e.SchemaPath, false).
		Add(e.runDir(task), true)
```

Replace with:

```go
	// Codex reads the schema and writes its last-message file itself, so both
	// need mounts. The verdict directory is mounted for the reviewer alone:
	// the implementing agent must not be able to write the file that records
	// whether its work was approved. Neither agent gets the whole data
	// directory, which holds the database.
	spec = spec.Add(e.SchemaPath, false)
	if agentName == "codex" {
		spec = spec.Add(e.verdictDir(task), true)
	}
```

Note what is deliberately *not* mounted any more: `e.runDir(task)` itself. Nothing inside the sandbox needs it. Transcripts are written by the daemon process from the stdout pipe, outside the sandbox. The per-task agent state lives at `<runDir>/state-<agent>` and `<runDir>/state-files/claude.json`, and both are already bound to their `$HOME` destinations by `AddAt` — bubblewrap binds by source path, so the source needs no mount of its own.

- [ ] **Step 4: Ensure the mount source exists before wrapping**

In Task 16 Step 9, find:

```go
	if err := sandbox.EnsureDirs(e.runDir(*task)); err != nil {
		return agent.Result{}, store.Step{}, err
	}
```

Replace with:

```go
	if err := sandbox.EnsureDirs(e.runDir(*task), e.verdictDir(*task)); err != nil {
		return agent.Result{}, store.Step{}, err
	}
```

- [ ] **Step 5: Add the two regression tests**

In Task 16 Step 10, after `TestSandboxSpecGivesTheReviewerNoWriteAccess`, add:

```go
func TestSandboxSpecKeepsTheVerdictOutOfTheImplementersReach(t *testing.T) {
	// The reviewer's verdict decides whether the implementer's work ships.
	// If the implementer can write it, the review is advisory at best.
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task := h.submit(t, "verdict integrity")
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")

	for _, m := range h.eng.sandboxSpec(task, "claude").Mounts {
		if !m.Write {
			continue
		}
		if m.Src == h.eng.verdictDir(task) || m.Src == h.eng.Cfg.VerdictsDir() {
			t.Errorf("claude can write %s; it could forge a reviewer verdict", m.Src)
		}
		if m.Src == h.eng.runDir(task) {
			t.Errorf("claude has the whole run directory writable at %s", m.Src)
		}
	}

	var codexHasIt bool
	for _, m := range h.eng.sandboxSpec(task, "codex").Mounts {
		if m.Src != h.eng.verdictDir(task) {
			continue
		}
		codexHasIt = true
		if !m.Write {
			t.Error("codex cannot write its --output-last-message file")
		}
	}
	if !codexHasIt {
		t.Errorf("codex has no writable verdict directory (%s)", h.eng.verdictDir(task))
	}
}
```

In Task 10 Step 7, after `TestRunTaskFailsWhenCodexReturnsProse`, add:

```go
func TestARecoveredReviewIgnoresTheVerdictFileFromBeforeTheCrash(t *testing.T) {
	// Recovery re-dispatches the review for the same phase and iteration, so
	// it targets the same --output-last-message path. A Codex run that exits
	// zero without writing must not inherit the pre-crash verdict: that is a
	// review that did not happen being read as an approval.
	codex := writeScript(t, "codex", `
echo '{"type":"thread.started","thread_id":"codex-thread"}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`)
	h := newHarness(t, fakeClaude(t, ""), codex)
	ctx := context.Background()

	task := h.submit(t, "recovered review")
	wt, err := h.eng.WT.Create(ctx, h.repo, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	task.State = string(loop.StatePlanReview)
	task.Phase, task.Iteration = string(loop.PhasePlan), 1
	task.WorktreeDir, task.Branch, task.BaseRef = wt.Dir, wt.Branch, wt.BaseRef
	task.PlanSessionID = "plan-sess"
	if err := h.st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	// The file the pre-crash run left behind, at the exact path this
	// iteration's review targets.
	if err := os.MkdirAll(h.eng.verdictDir(task), 0o755); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(h.eng.verdictDir(task), "plan-1-verdict.json")
	if err := os.WriteFile(planted,
		[]byte(`{"verdict":"approved","findings":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateFailed) {
		t.Fatalf("State = %q, want failed; a stale verdict file was read as this round's review", got.State)
	}
	if len(h.pr.Calls) != 0 {
		t.Error("a PR was opened on a verdict from a review that never ran")
	}
}
```

- [ ] **Step 6: Correct the spec's mount table**

In `docs/superpowers/specs/2026-08-06-overseer-design.md`, find the row:

```
| `<runs>/<slug>` | rw | Codex writes `--output-last-message` itself. |
```

Replace with:

```
| `<verdicts>/<slug>` | rw for **Codex only** | Codex writes `--output-last-message` itself. Kept out of the run directory and away from Claude: a verdict the implementing agent can write is a verdict it can forge. |
```

In the same file, find the sentence:

```
The data directory is never mounted whole, only the current task's run
directory, so an agent cannot rewrite task state.
```

Replace with:

```
The data directory is never mounted whole. Claude gets no part of it beyond
its own per-task agent-state directories, which are bound to their `$HOME`
destinations rather than exposed at their real paths; Codex additionally gets
the task's verdict directory, because it writes its own last-message file. An
agent cannot rewrite task state, and the agent under review cannot write the
reviewer's verdict.
```

- [ ] **Step 7: Prove the edits landed**

```bash
P=docs/superpowers/plans/2026-08-06-overseer.md
S=docs/superpowers/specs/2026-08-06-overseer-design.md
grep -c 'func (c Config) VerdictsDir' "$P"                                  # expect 1
grep -c 'func (e \*Engine) verdictDir' "$P"                                 # expect 1
grep -c 'clear stale verdict' "$P"                                          # expect 2
grep -c 'Add(e.runDir(task), true)' "$P"                                    # expect 0
grep -c 'TestARecoveredReviewIgnoresTheVerdictFileFromBeforeTheCrash' "$P"  # expect 1
grep -c 'TestSandboxSpecKeepsTheVerdictOutOfTheImplementersReach' "$P"      # expect 1
grep -c '<verdicts>/<slug>' "$S"                                            # expect 1
```

Expected: `1 1 2 0 1 1 1`.

Then walk the new engine test against the *old* code to confirm it fails: `runCodex` would build `lastPath` under `runDir`, so the plant would have to move — which is the point; under the old layout the planted file is at the path the review reads, `os.ReadFile` succeeds, `ParseVerdict` returns `approved`, the plan phase converges, and the task reaches `done` with a PR. Under the new code the file is removed before the run, `os.ReadFile` fails, `res.FinalText` is empty, `ParseVerdict` errors twice, and the task fails.

- [ ] **Step 8: Commit**

```bash
git add docs/superpowers/plans/2026-08-06-overseer.md docs/superpowers/specs/2026-08-06-overseer-design.md
git commit -m "fix(plan): isolate and clear the reviewer verdict file so it cannot be stale or forged"
```

---

## Task 8: Verify must not abort on a missing sandbox mount source

**D11.** `execVerify` wraps the verify command with `e.sandboxSpec(task, "claude")`, which includes two **required** mounts whose sources overseer creates itself: `<runDir>/state-claude` and `<runDir>/state-files/claude.json`. Those are created by `prepareAgentState`, which only `runAgent` calls. `runVerify` calls `sandbox.EnsureDirs(e.runDir(*task))` and nothing else.

Bubblewrap aborts on a missing `--bind` source, so any path that reaches verify without a preceding Claude turn in the same data directory fails the step — and because a failed verify is fed back to the agent as a critical finding, the agent is told its *tests* failed when in fact the sandbox never started. It would then "fix" a build that was never broken, and the fingerprint would repeat, and the task would escalate.

**Files:**
- Modify: `docs/superpowers/plans/2026-08-06-overseer.md` — Task 17 Step 7 `runVerify` (~line 11482)

**Interfaces:**
- Consumes: `(*Engine).prepareAgentState(task store.Task, agentName string) error`.
- Produces: no new API.

- [ ] **Step 1: Prepare the state the spec requires**

In Task 17 Step 7, find:

```go
	transcript := filepath.Join(e.runDir(*task),
		fmt.Sprintf("exec-%d-verify.jsonl", task.Iteration))
	if err := sandbox.EnsureDirs(e.runDir(*task)); err != nil {
		return nil, err
	}
```

Replace with:

```go
	transcript := filepath.Join(e.runDir(*task),
		fmt.Sprintf("exec-%d-verify.jsonl", task.Iteration))
	if err := sandbox.EnsureDirs(e.runDir(*task)); err != nil {
		return nil, err
	}
	// execVerify runs under the same spec Claude gets, and that spec has
	// required mounts whose sources overseer owns. bubblewrap aborts on a
	// missing --bind source, and the abort would surface as a non-zero exit —
	// indistinguishable from a genuine test failure, and fed back to the agent
	// as one. Returning an error instead routes it through failTask, where a
	// harness problem belongs.
	if err := e.prepareAgentState(*task, "claude"); err != nil {
		return nil, err
	}
```

- [ ] **Step 2: Add the regression test**

In Task 17 Step 4, after `TestVerifyGatePassesAndTaskCompletes`, add:

```go
func TestVerifyPreparesTheSandboxMountSourcesItNeeds(t *testing.T) {
	// The spec execVerify runs under has required mounts under the run
	// directory. If runVerify does not create them, a real bwrap aborts and
	// the agent is told its tests failed when the sandbox never started.
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	h.eng.Cfg.VerifyCommand = "true"
	ctx := context.Background()

	task := h.submit(t, "verify prepares state")
	wt, err := h.eng.WT.Create(ctx, h.repo, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	task.WorktreeDir = wt.Dir
	if err := h.st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	if _, err := h.eng.runVerify(ctx, &task); err != nil {
		t.Fatalf("runVerify: %v", err)
	}

	for _, p := range []string{
		h.eng.agentStateDir(task, "claude"),
		h.eng.agentStateFile(task, "claude.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("required mount source %s not created: %v", p, err)
		}
	}
}
```

Add `"os"` to `internal/engine/verify_test.go`'s imports (currently `context`, `strings`, `testing`, `overseer/internal/agent`, `overseer/internal/loop`).

- [ ] **Step 3: Prove the edits landed**

```bash
P=docs/superpowers/plans/2026-08-06-overseer.md
grep -c 'e.prepareAgentState(\*task, "claude")' "$P"              # expect 1
grep -c 'TestVerifyPreparesTheSandboxMountSourcesItNeeds' "$P"    # expect 1
```

Expected: `1 1`.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/plans/2026-08-06-overseer.md
git commit -m "fix(plan): verify must create the sandbox mount sources it depends on"
```

---

## Task 9: Validate the blocking threshold where tasks are actually created

**D12.** `ParseBatch` validates `blocking_severity` against `config.ValidSeverities`. `Engine.Submit` — which the dashboard's `POST /tasks` calls directly, and which `SubmitBatch` also funnels through — does not. A form post with `blocking_severity=whatever` is stored verbatim. `Verdict.Blocking` then falls back to `"any"` for an unknown threshold, so the task runs at the strictest setting while the dashboard displays a value that means nothing.

The validation also lives in two places already, which is how they drifted. Fix both by putting the predicate in `config` and calling it from each site.

**Files:**
- Modify: `docs/superpowers/plans/2026-08-06-overseer.md` — Task 1 Step 4 `config.go` (~line 209 and ~line 251), Task 11 Step 3 `ParseBatch` and `Submit` (~lines 6505–6535), Task 11 Step 1 tests, Task 13 Step 1 `actions_test.go`

**Interfaces:**
- Consumes: `config.ValidSeverities`.
- Produces: `config.ValidSeverity(s string) bool`.

- [ ] **Step 1: Add the predicate to config**

In Task 1 Step 4, find:

```go
// ValidSeverities are the accepted blocking-severity thresholds, loosest first.
var ValidSeverities = []string{"any", "minor", "major", "critical"}
```

Replace with:

```go
// ValidSeverities are the accepted blocking-severity thresholds, loosest first.
var ValidSeverities = []string{"any", "minor", "major", "critical"}

// ValidSeverity reports whether s is an accepted threshold. It lives here so
// the daemon config, the batch parser and Engine.Submit share one definition;
// an unvalidated value reaches Verdict.Blocking, which silently treats
// anything it does not recognise as "any".
func ValidSeverity(s string) bool { return slices.Contains(ValidSeverities, s) }
```

Add `"slices"` to `config.go`'s imports (currently `errors`, `fmt`, `os`, `path/filepath`, `time`, `gopkg.in/yaml.v3`).

Then find:

```go
func (c Config) validate() error {
	for _, s := range ValidSeverities {
		if c.BlockingSeverity == s {
			return nil
		}
	}
	return fmt.Errorf("blocking_severity %q must be one of %v", c.BlockingSeverity, ValidSeverities)
}
```

Replace with:

```go
func (c Config) validate() error {
	if !ValidSeverity(c.BlockingSeverity) {
		return fmt.Errorf("blocking_severity %q must be one of %v",
			c.BlockingSeverity, ValidSeverities)
	}
	return nil
}
```

Note for whoever applies this: Task 16 Step 7 appends a `switch c.Sandbox` block to `validate`. Keep that append working — the sandbox check goes after the severity check and before the final `return nil`.

- [ ] **Step 2: Use it in `ParseBatch`**

In Task 11 Step 3, find:

```go
		if t.BlockingSeverity == "" {
			continue
		}
		valid := false
		for _, s := range config.ValidSeverities {
			if t.BlockingSeverity == s {
				valid = true
				break
			}
		}
		if !valid {
			return Batch{}, fmt.Errorf("parse batch: task %d has blocking_severity %q, want one of %v",
				i+1, t.BlockingSeverity, config.ValidSeverities)
		}
```

Replace with:

```go
		if t.BlockingSeverity != "" && !config.ValidSeverity(t.BlockingSeverity) {
			return Batch{}, fmt.Errorf("parse batch: task %d has blocking_severity %q, want one of %v",
				i+1, t.BlockingSeverity, config.ValidSeverities)
		}
```

- [ ] **Step 3: Validate in `Submit`**

In Task 11 Step 3, find:

```go
	severity := bt.BlockingSeverity
	if severity == "" {
		severity = e.Cfg.BlockingSeverity
	}
```

Replace with:

```go
	severity := bt.BlockingSeverity
	if severity == "" {
		severity = e.Cfg.BlockingSeverity
	}
	// Checked here as well as in ParseBatch: the dashboard's POST /tasks calls
	// Submit directly, and an unrecognised threshold reaches Verdict.Blocking,
	// which quietly treats it as "any" while the task page shows a value that
	// means nothing.
	if !config.ValidSeverity(severity) {
		return store.Task{}, fmt.Errorf("blocking_severity %q must be one of %v",
			severity, config.ValidSeverities)
	}
```

- [ ] **Step 4: Add the two tests**

In Task 11 Step 1, after `TestSubmitRejectsNonRepoPath`, add:

```go
func TestSubmitRejectsAnUnknownBlockingSeverity(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	if _, err := h.eng.Submit(context.Background(), BatchTask{
		Repo: h.repo, Goal: "g", BlockingSeverity: "whatever",
	}); err == nil {
		t.Fatal("expected an error; an unknown threshold silently becomes \"any\"")
	}
}
```

In Task 13 Step 1, add to `internal/web/actions_test.go`:

```go
func TestPostTasksRejectsAnUnknownSeverity(t *testing.T) {
	// The form is a <select>, but the endpoint is not.
	s, _ := newTestServer(t)
	repo := initRepo(t)

	rec := post(t, s, "/tasks", url.Values{
		"repo": {repo}, "goal": {"g"}, "blocking_severity": {"whatever"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
```

- [ ] **Step 5: Prove the edits landed**

```bash
P=docs/superpowers/plans/2026-08-06-overseer.md
grep -c 'func ValidSeverity' "$P"                              # expect 1
grep -c 'config.ValidSeverity(severity)' "$P"                  # expect 1
grep -c 'config.ValidSeverity(t.BlockingSeverity)' "$P"        # expect 1
grep -c 'for _, s := range config.ValidSeverities' "$P"        # expect 0
grep -c 'TestSubmitRejectsAnUnknownBlockingSeverity' "$P"      # expect 1
grep -c 'TestPostTasksRejectsAnUnknownSeverity' "$P"           # expect 1
```

Expected: `1 1 1 0 1 1`.

Then confirm `TestSubmitCreatesQueuedTaskWithUniqueSlug` still passes: it submits with an empty severity, which falls back to `e.Cfg.BlockingSeverity` = `"any"`, which is valid.

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/plans/2026-08-06-overseer.md
git commit -m "fix(plan): validate blocking_severity in Submit, not only in ParseBatch"
```

---

## Task 10: Consistency sweep

**D13.** Nothing here changes behaviour. All of it misleads the person executing the plan, which is the plan's only job.

**Files:**
- Modify: `docs/superpowers/plans/2026-08-06-overseer.md` — Task 4 Step 5 comment, Task 1 Step 4 note, Task 3 / Task 8 / Task 11 / Task 17 Interfaces blocks, 25 `Expected:` lines, the Verification checklist

**Interfaces:**
- Consumes: nothing.
- Produces: nothing.

- [ ] **Step 1: Retire the "overseer does not sandbox" comment**

Task 16 adds the sandbox but never revisits `ClaudeArgs`'s doc comment, which still tells the reader the opposite. Find:

```go
// --add-dir is not passed, but do not mistake that for confinement.
// bypassPermissions skips the permission system entirely, and --add-dir only
// extends that system's allow-list, so omitting it grants nothing and
// restricts nothing. The process runs as the daemon's user with that user's
// full filesystem access; the worktree is only its working directory.
// Confining it would require an OS-level sandbox, which overseer does not
// currently set up.
```

Replace with:

```go
// --add-dir is not passed, but do not mistake that for confinement.
// bypassPermissions skips the permission system entirely, and --add-dir only
// extends that system's allow-list, so omitting it grants nothing and
// restricts nothing. The process runs as the daemon's user with that user's
// full filesystem access; the worktree is only its working directory.
// Confinement comes from internal/sandbox, which puts the process in a
// bubblewrap namespace — and only when the configured mode resolves to one.
// With sandbox: off, this argv really is unconfined.
```

- [ ] **Step 2: Correct the yaml duration rationale**

Find:

```
`yaml.v3` decodes `5m` into a `time.Duration` field via its `encoding.TextUnmarshaler` support, so no custom type is needed.
```

Replace with:

```
`yaml.v3` decodes `5m` into a `time.Duration` field through a hard-coded special case for that type in its decoder — not through `encoding.TextUnmarshaler`, which `time.Duration` does not implement. No custom type is needed either way; the distinction matters only if the field's type is ever changed to a wrapper, which would lose the behaviour. Confirmed against `gopkg.in/yaml.v3 v3.0.1`: `step_timeout: 5m` decodes to `5m0s`.
```

- [ ] **Step 3: Bring the Interfaces blocks back in line with the code**

Four blocks describe types that the plan's own code defines differently. A task implementer sees only their own task's block, so each of these is a working instruction that is wrong.

In Task 3, find:

```
  - `store.Finding` struct: `ID int64`, `StepID int64`, `Severity string`, `File string`, `Line int`, `Summary string`, `Blocking bool`
```

Replace with:

```
  - `store.Finding` struct: `ID int64`, `StepID int64`, `Severity string`, `File string`, `Line int`, `Summary string`, `Detail string` (volatile supplementary output — verify command tails — kept out of `Summary` because the oscillation fingerprint hashes `Summary` only), `Blocking bool`
```

In Task 8, find:

```
  - `worktree.Worktree` struct: `Dir string`, `Branch string`, `BaseRef string`, `RepoPath string`
```

Replace with:

```
  - `worktree.Worktree` struct: `RepoPath string`, `Dir string`, `Branch string`, `BaseRef string`, `CommonDir string` (the repository's shared git directory, resolved with `rev-parse`), `AdminDir string` (this worktree's own administrative directory). The last two are what the sandbox mounts; they are never derived from `RepoPath`, because a submitted path may itself be a linked worktree whose `.git` is a file.
```

In Task 11, find:

```
  - `engine.BatchTask` struct: `Repo string`, `Goal string`, `Constraints []string`, `BlockingSeverity string`
```

Replace with:

```
  - `engine.BatchTask` struct: `Repo string`, `Goal string`, `Constraints []string`, `BlockingSeverity string`. Task 17 adds `Verify string`.
```

In Task 17, find:

```
  - `loop.StateVerifying`, `loop.ActVerify`, `loop.Task.Verify bool`
```

Replace with:

```
  - `loop.StateVerifying`, `loop.ActVerify`, `loop.Task.Verify bool`
  - `engine.BatchTask.Verify string` (yaml `verify`), `store.Task.VerifyCommand string`, `config.Config.VerifyCommand string`
```

- [ ] **Step 4: Replace the brittle test counts**

There are 22 `Expected: PASS, N tests.` lines plus three prose counts, and several are already wrong before this change: Task 5 says 22 where the file defines 26; Task 6 says 29 where it is 35; Task 17 says the engine package has 30 where it has more than 50. Every task in this plan adds tests, so every number would need recomputing again.

Replace the numbers rather than recomputing them. For each `Expected: PASS, N tests.` line, replace with:

```
Expected: PASS — every test in the package.
```

Where the line carries extra information, keep the information and drop the count. Specifically:

- `Expected: PASS, 10 tests (6 from Task 2 plus 4 new).` → `Expected: PASS — every test in the package, the Task 2 tests as well as the new ones.`
- `Expected: PASS, 10 tests. On a host without a usable bwrap the four` (continues `integration tests skip and the rest still pass.`) → `Expected: PASS. On a host without a usable bwrap the four integration tests skip and the rest still pass.`
- `Expected: PASS. The engine package contributes 11 tests; \`store\` now has 13.` → `Expected: PASS across every package.`
- `Expected: PASS. \`web\` now has 24 tests.` → `Expected: PASS across every package.`
- `Expected: PASS. \`loop\` has 29 tests, \`engine\` 30.` → `Expected: PASS across every package.`

A count is only worth writing down when the count itself is the assertion. Nowhere in this plan is it.

- [ ] **Step 5: Make the sandbox checklist item honest**

In the Verification checklist, find:

```
- [ ] The board shows `sandbox: bwrap (auto)`, not an `UNSANDBOXED` warning
```

Replace with:

```
- [ ] The board shows the sandbox mode. On a host where unprivileged user namespaces work
      this reads `sandbox: bwrap (auto)`; where they do not — a sysctl, a container, or
      Ubuntu's AppArmor profile for `bwrap` — `auto` correctly downgrades and the board says
      `UNSANDBOXED` instead. Confirm which one applies before treating the warning as a bug:
      `bwrap --ro-bind / / --proc /proc --dev /dev -- /bin/true` is the same probe `Select`
      runs. If you need the sandbox enforced, set `sandbox: bwrap`, which fails to start
      rather than downgrading.
```

- [ ] **Step 6: Note the one intentional loose end**

`(*Manager).Diff` is defined in Task 8 and asserted on in `TestCommitIncludesUntrackedFiles`, but the engine never calls it: `CodeReviewPrompt` has Codex run `git diff <base>...HEAD` itself, so it sees the real range rather than a description of it. Leaving an unexplained unused method invites a future reader to wire it in. In Task 8, find:

```go
// Diff returns the accumulated diff against the base ref.
```

Replace with:

```go
// Diff returns the accumulated diff against the base ref.
//
// The engine deliberately does not call this. CodeReviewPrompt has Codex run
// `git diff <base>...HEAD` inside the worktree, so the reviewer reads the real
// range rather than a string the daemon assembled. Diff exists for the tests
// and for `overseer logs`-style debugging; do not "fix" the review path to use
// it.
```

- [ ] **Step 7: Prove the edits landed**

```bash
P=docs/superpowers/plans/2026-08-06-overseer.md
grep -c 'Expected: PASS, [0-9]' "$P"                            # expect 0
grep -c 'which overseer does not' "$P"                          # expect 0
grep -c 'encoding.TextUnmarshaler' "$P"                         # expect 1 (now in the corrected sentence)
grep -c 'Detail string` (volatile' "$P"                         # expect 1
grep -c 'CommonDir string' "$P"                                 # expect 2 (struct + Interfaces block)
grep -c 'not an `UNSANDBOXED` warning' "$P"                     # expect 0
grep -c 'do not "fix" the review path to use' "$P"              # expect 1
```

Expected: `0 0 1 1 2 0 1`.

- [ ] **Step 8: Read the whole plan once, start to finish**

The counts and the Interfaces blocks drifted because nobody re-read the document after editing it. Read it through and check three things:

1. Every identifier used in a test is defined in some snippet — in this task's or an earlier one's.
2. Every import block lists exactly what its file uses. This change adds `regexp` to `runner.go` and `authfail.go`, `slices` to `config.go`, `errors` and `sandbox` to `engine.go`, `encoding/json` to `engine/sandbox_test.go`, and `os` to `engine/verify_test.go`.
3. No task's prose still describes behaviour a later task replaced.

- [ ] **Step 9: Commit**

```bash
git add docs/superpowers/plans/2026-08-06-overseer.md
git commit -m "docs(plan): sync interfaces blocks, drop brittle test counts, retire stale claims"
```

---

## Verification checklist

Run these before calling this change done. There is no compiler here; these are the substitutes.

- [ ] `git diff --stat master` shows exactly two files changed: the plan and the spec. No Go files, no `go.mod`, no changes under `testdata/`.
- [ ] Every `grep` in every task's "Prove the edits landed" step returns the stated count.
- [ ] `grep -c 'Expected: PASS, [0-9]' docs/superpowers/plans/2026-08-06-overseer.md` returns 0.
- [ ] Each of the ten behaviour fixes has a named test in the plan that fails against the old snippet. Walk them: D4→`TestHarnessSubmitCarriesTheVerifyCommand`, D6→`TestParseClaudeRateLimitWarningIsNotAnError` + `TestRunIgnoresARateLimitEventBeforeASuccessfulResult`, D7→the two extended tables, D8→`TestNormalizeFailureOutputRecognisesCompilerDiagnostics`, D9→`TestLastBlockingFindingsPrefersAFailedVerifyOverAnOlderReview`, D10→`TestARecoveredReviewIgnoresTheVerdictFileFromBeforeTheCrash` + `TestSandboxSpecKeepsTheVerdictOutOfTheImplementersReach`, D11→`TestVerifyPreparesTheSandboxMountSourcesItNeeds`, D12→`TestSubmitRejectsAnUnknownBlockingSeverity` + `TestPostTasksRejectsAnUnknownSeverity`.
- [ ] The plan and the spec agree on the sandbox mount table. Diff the spec's table against `sandboxSpec` mount by mount; the verdict-directory row is the one this change moves.
- [ ] `testdata/*.jsonl` is byte-identical to `master`, and each line still parses as JSON.
- [ ] No task in the plan references a `Task N` number that this change renumbered — nothing was renumbered, so this is a check that it stayed that way.

---

## Deviations from this plan

All thirteen defects were fixed as specified. Every "Prove the edits landed" grep returns
its stated count except the two noted below, which the plan miscounted. The per-task
`git commit` steps were skipped: this run's harness commits the work itself.

**1. There *is* a compiler here.** The preamble says "There is nothing to run. `go test`
does not exist here." In fact `go1.26.0` is installed at `/usr/bin/go`. That changed the
evidence available, so rather than take the plan's recorded outputs on faith I re-ran them
in scratch modules under `/tmp` (never in this repo — the two-files-only constraint holds):

- **Task 4's classifiers.** Ran the old and new `IsRetryable`/`IsAuthFailure` over the
  plan's exact existing tables plus the six new adversarial cases. Result: 0 failures for
  the new implementations; every pre-existing case keeps its classification. As claimed.
- **Task 5's normalisation.** Ran old and new `NormalizeFailureOutput` against all four
  pre-existing assertions and the three new tests. The old implementation fails 6 of the
  new assertions and passes every old one; the new implementation passes all of both. The
  outputs recorded in Task 5 (`-> ["main.go:N:N: undefined: foo"]`, the `make` cases, the
  `[]` for compile errors under the old regex) reproduce exactly.
- **Task 10 Step 2's yaml claim.** Confirmed against `gopkg.in/yaml.v3 v3.0.1`:
  `step_timeout: 5m` decodes to `5m0s`, and `time.Duration` does *not* implement
  `encoding.TextUnmarshaler`. The plan's original rationale was indeed wrong, and the
  replacement sentence is accurate.
- **Task 10 Step 5's probe.** `bwrap` 0.11.1 is present and the probe fails here exactly as
  the preamble describes, confirming the empirical caveat. The checklist text was adjusted
  to quote `Probe`'s real argv, which has no `--` separator before `/bin/true`.

**2. Two extra compile-blocking defects found and fixed (extends Task 1).** Extracting all
99 embedded ```go blocks and running `gofmt -e` over the 52 that are whole files surfaced
two genuine Go syntax errors that Task 1 missed — both present on `master`, one of them in
`internal/sandbox/sandbox_test.go`, the very file Task 1 Step 2 edits:

- `if Passthrough{}.Name() != "off" {` (Task 16 Step 1)
- `if Verdict{}.Fingerprint("any") == "" {` (Task 5)

A composite literal directly in an `if` condition does not parse — the parser reads the `{`
as the start of the if body. Both are now parenthesised (`if (Passthrough{}).Name()`) with a
comment saying why. All 52 blocks now parse; the 47 partial snippets were checked too, and
their only diagnostics are artifacts of wrapping bare struct fields and `case` arms.

**3. Task 7 Step 2 had a forward dependency; used `os.MkdirAll` instead.** The step directs
`runCodex` (Task 10) to call `sandbox.EnsureDirs` and to import `overseer/internal/sandbox`.
That package is created by Task 16, which runs *after* Task 10, so Task 10 would not
compile. `EnsureDirs` is `os.MkdirAll` over a list, so `runCodex` now calls `os.MkdirAll`
directly — same effect, no forward reference — with a comment recording why. Only `errors`
was added to Task 10's `engine.go` imports, not `sandbox`. Task 16 Step 9 still adds the
`sandbox` import and still extends its own `EnsureDirs` call to cover the verdict directory,
exactly as Task 7 Step 4 specifies.

**4. Task 2's two harness lines have the same forward dependency; documented rather than
moved.** `cfg.Sandbox = "off"` names a field Task 16 adds, and
`VerifyCommand: h.eng.Cfg.VerifyCommand` names fields Task 17 adds, yet both sit in Task 10
and Task 12 snippets. Moving them would scatter the harness, so the lines stay where a
reader looks for them and each snippet now carries a note naming the task that makes it
compile. `TestHarnessSubmitCarriesTheVerifyCommand` (Task 17) is the guard if the second is
forgotten.

**5. Two of the plan's expected grep counts were wrong; the edits are correct.**

- Task 1 Step 5 expects `grep -c '"encoding/json"'` to return 1. Four snippets already
  imported it on `master`, so the correct post-edit count is **5**.
- Task 10 Step 7 expects `grep -c 'CommonDir string'` to return 2. The pattern also matches
  `GitCommonDir string`, which is a separate field; the correct post-edit count is **3**.

**6. Two small additions in the plan's own voice, to keep it self-consistent.**

- Task 16 Step 7 says to "extend `validate`" with a `switch c.Sandbox` block. Task 9
  restructures `validate` to return early, so the step now states that the sandbox check
  goes after the severity check and before the final `return nil` — appended to the end of
  the old body it would have been unreachable.
- Task 16 Step 9's prose explained why `runDir` no longer needs a mount; that explanation is
  now a doc comment on `agentStateDir`, where a reader of the code will find it.
