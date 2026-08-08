package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"overseer/internal/store"
)

// TestWritePreview is a development aid, not a check: it seeds a store that
// looks like a real run and writes the rendered pages out so they can be
// opened in a browser. It only runs when OVERSEER_PREVIEW_DIR is set.
func TestWritePreview(t *testing.T) {
	out := os.Getenv("OVERSEER_PREVIEW_DIR")
	if out == "" {
		t.Skip("set OVERSEER_PREVIEW_DIR to render the dashboard to disk")
	}
	s, st := newTestServer(t)
	seedPreview(t, st)

	if err := os.WriteFile(filepath.Join(out, "style.css"), s.css, 0o644); err != nil {
		t.Fatal(err)
	}
	seedPreviewProposals(t, st)

	pages := map[string]string{
		"board.html":         "/",
		"detail.html":        "/?sel=1&tab=diff",
		"findings.html":      "/?sel=2&tab=findings",
		"live.html":          "/?sel=1&tab=live",
		"blocked.html":       "/?sel=4&tab=findings",
		"add.html":           "/?overlay=add",
		"cli.html":           "/?overlay=cli",
		"empty.html":         "/?q=nothingmatchesthis",
		"flat.html":          "/?group=0&filter=attention",
		"bulk.html":          "/?filter=attention&bulk=2,5",
		"wizard-source.html": "/?wizard=-1",
		"wizard-focus.html":  "/?wizard=1",
		"wizard-run.html":    "/?wizard=2",
		"wizard-review.html": "/?wizard=3",
		"wizard-failed.html": "/?wizard=4",
		"settings.html":      "/?overlay=settings",
		"analyses.html":      "/?overlay=analyses",
	}
	for name, path := range pages {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s -> %d", path, rec.Code)
		}
		body := rec.Body.String()
		if err := os.WriteFile(filepath.Join(out, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("wrote %d pages to %s", len(pages), out)
}

func seedPreview(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()

	type fixture struct {
		slug, repo, goal, state, phase string
		iter                           int
		branch, pr, err, verify        string
		cap                            float64
		// rounds is the blocking findings raised by each review round.
		rounds [][]string
	}
	fixtures := []fixture{
		{
			slug: "csv-export", repo: "/home/kal/code/dc-planner",
			goal:  "Add CSV export to the rack inventory view.",
			state: "executing", phase: "exec", iter: 3,
			branch: "overseer/csv-export", verify: "go test ./...", cap: 8,
			rounds: [][]string{
				{
					"Plan writes the CSV synchronously in the handler; the inventory view can hold 40k rows.",
					"No mention of the existing csvutil package.",
					"Step 4 and step 5 are the same change described twice.",
				},
				{},
				{"internal/web: undefined csvutil.StreamRows"},
				{},
				{
					"Export ignores the active column filter — the CSV is not what the user is looking at.",
					"Header row capitalisation differs from the table header.",
				},
			},
		},
		{
			slug: "backoff-helper", repo: "/home/kal/code/clanker",
			goal:  "Replace the hand-rolled retry logic with a shared backoff helper.",
			state: "escalated", phase: "exec", iter: 10,
			err: "oscillating: the same three findings returned at iterations 7, 9 and 10",
			cap: 8,
			rounds: [][]string{
				{"backoff jitter bounds are half, not full", "context cancellation is dropped on retry"},
				{"context cancellation is dropped on retry", "naming: attemptN reads as a count"},
				{"backoff jitter bounds are half, not full", "naming: attemptN reads as a count"},
				{"context cancellation is dropped on retry"},
				{"backoff jitter bounds are half, not full", "naming: attemptN reads as a count"},
				{"backoff jitter bounds are half, not full", "naming: attemptN reads as a count"},
			},
		},
		{
			slug: "sqlite-wal", repo: "/home/kal/code/overseer",
			goal:  "Enable WAL mode and a busy timeout on the store connection.",
			state: "done", phase: "exec", iter: 2,
			pr:  "https://github.com/colonelpanik/overseer/pull/482",
			cap: 8,
			rounds: [][]string{
				{"The busy timeout is set after the first query, not on open."},
				{},
			},
		},
		{
			slug: "config-schema", repo: "/home/kal/code/overseer",
			goal:  "Validate config.yaml against a schema at startup.",
			state: "queued", cap: 8,
		},
		{
			slug: "verda-cli-auth", repo: "/home/kal/code/verda-cli",
			goal:  "Move token storage out of the config file into the OS keyring.",
			state: "failed", phase: "exec", iter: 4,
			err: "git push rejected — the remote has diverged. The worktree is preserved.",
			cap: 8,
			rounds: [][]string{
				{"Token is written to the keyring but never removed on logout."},
				{},
			},
		},
		{
			slug: "retry-jitter", repo: "/home/kal/code/rack-metrics",
			goal:  "Add full jitter to the collector retry schedule.",
			state: "escalated", phase: "plan", iter: 10,
			err: "oscillating: one naming nit has recurred four times",
			cap: 4,
			rounds: [][]string{
				{"naming: backoffFor should say what it returns", "jitter is half, not full"},
				{"naming: backoffFor should say what it returns"},
				{"naming: backoffFor should say what it returns"},
				{"naming: backoffFor should say what it returns"},
			},
		},
	}

	for _, f := range fixtures {
		verify := f.verify
		task, err := st.CreateTask(ctx, store.Task{
			Slug: f.slug, RepoPath: f.repo, Goal: f.goal, State: f.state,
			Phase: f.phase, Iteration: f.iter, MaxIterations: 10,
			BlockingSeverity: "any", Branch: f.branch, PRURL: f.pr,
			ErrMsg: f.err, VerifyCommand: verify, CostCapUSD: f.cap,
			PlanSessionID: "9d2b41f7-5c08-4a31-b6de-71ac0e3f5b2c",
			WorktreeDir:   "/home/kal/.overseer/worktrees/" + f.slug,
		})
		if err != nil {
			t.Fatal(err)
		}

		for i, findings := range f.rounds {
			phase := "plan"
			if i >= 2 {
				phase = "exec"
			}
			iter := i + 1

			claude, err := st.StartStep(ctx, store.Step{
				TaskID: task.ID, Phase: phase, Iteration: iter, Agent: "claude",
			})
			if err != nil {
				t.Fatal(err)
			}
			claude.CostUSD = 0.41
			claude.InputTokens, claude.OutputTokens = 41200, 1800
			if err := st.FinishStep(ctx, claude, nil); err != nil {
				t.Fatal(err)
			}

			agent := "codex"
			if i == 2 {
				agent = "verify"
			}
			review, err := st.StartStep(ctx, store.Step{
				TaskID: task.ID, Phase: phase, Iteration: iter, Agent: agent,
			})
			if err != nil {
				t.Fatal(err)
			}
			review.Verdict = "approved"
			if len(findings) > 0 {
				review.Verdict = "changes_requested"
				review.ExitCode = 1
			}
			review.CostUSD = 0.09
			var rows []store.Finding
			for j, summary := range findings {
				sev := []string{"major", "minor", "nit", "critical"}[j%4]
				rows = append(rows, store.Finding{
					Severity: sev, Summary: summary, Blocking: true,
					File: "internal/web/export.go", Line: 61 + j*17,
				})
			}
			if i == 4 {
				rows = append(rows, store.Finding{
					Severity: "nit", Summary: "Comment says rows, means records.",
					File: "internal/csvutil/stream.go", Line: 13,
				})
			}
			if err := st.FinishStep(ctx, review, rows); err != nil {
				t.Fatal(err)
			}
		}

		// A live step for the task the preview opens on.
		if f.slug == "csv-export" {
			path := filepath.Join(t.TempDir(), "live.jsonl")
			writeLiveTranscript(t, path)
			if _, err := st.StartStep(ctx, store.Step{
				TaskID: task.ID, Phase: "exec", Iteration: 3, Agent: "claude",
				TranscriptPath: path,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// config-schema waits on sqlite-wal, which is done, and on verda-cli-auth,
	// which failed — the case where the blocked banner has something to say.
	if err := st.SetTaskDeps(ctx, 4, []int64{3, 5}); err != nil {
		t.Fatal(err)
	}
	// Age the tasks so the elapsed column is not all "<1s".
	for id := int64(1); id <= 6; id++ {
		task, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		task.CreatedAt = time.Now().Add(-time.Duration(id) * 17 * time.Minute)
		if _, err := st.DB().ExecContext(ctx,
			`UPDATE tasks SET created_at=? WHERE id=?`,
			task.CreatedAt.Format(time.RFC3339Nano), id); err != nil {
			t.Fatal(err)
		}
	}
}

// seedPreviewProposals creates one proposal per wizard step so the overlay can
// be looked at in every state without waiting for a real analysis.
func seedPreviewProposals(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	repo := "/home/kal/code/dc-planner"

	// 1: focus step.
	if _, err := st.CreateProposal(ctx, store.Proposal{
		RepoPath: repo, State: store.ProposalDraft, Model: "claude-sonnet-5",
		MaxTasks: 12, Detected: "Go · go test ./... · default branch main · 412 tracked files",
	}); err != nil {
		t.Fatal(err)
	}

	// 2: analysing, with a live transcript.
	path := filepath.Join(t.TempDir(), "analysis.jsonl")
	writeAnalysisTranscript(t, path)
	if _, err := st.CreateProposal(ctx, store.Proposal{
		RepoPath: repo, State: store.ProposalAnalysing, Model: "claude-sonnet-5",
		MaxTasks: 12, Detected: "Go · go test ./...", TranscriptPath: path,
		Focus: []string{"test coverage", "tech debt"}, CostUSD: 0.18,
	}); err != nil {
		t.Fatal(err)
	}

	// 3: ready to review.
	ready, err := st.CreateProposal(ctx, store.Proposal{
		RepoPath: repo, State: store.ProposalReady, Model: "claude-sonnet-5",
		MaxTasks: 12, Detected: "Go · go test ./...", CostUSD: 0.41,
		Focus: []string{"test coverage", "tech debt"},
		Notes: "leave the vendored directory alone",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := []store.ProposalTask{
		{
			Key: "wal-mode", Goal: "Enable WAL mode and a busy timeout on the store connection.",
			Constraints: []string{"Follow the existing repo_*.go pattern"},
			Verify:      "go test ./...", Severity: "any", CostCap: 8, Selected: true,
			Rationale: "store.go opens the database without a busy timeout, so two workers racing produce SQLITE_BUSY rather than waiting.",
			Evidence:  []string{"internal/store/store.go:33"},
		},
		{
			Key: "config-schema", Goal: "Validate config.yaml against a schema at startup.",
			Verify: "go test ./...", Severity: "any", CostCap: 8, Selected: true,
			DependsOn: []string{"wal-mode"},
			Rationale: "config.go unmarshals straight into the struct, so a typo in a key is silently ignored.",
			Evidence:  []string{"internal/config/config.go:97"},
		},
		{
			Key: "readme", Goal: "Rewrite the README around the new subcommand layout.",
			Severity: "minor", Selected: false,
			Rationale: "The README documents three flags that no longer exist.",
			Evidence:  []string{"README.md:41"},
		},
	}
	if err := st.ReplaceProposalTasks(ctx, ready.ID, rows); err != nil {
		t.Fatal(err)
	}

	// 5: partly queued — the case the history list exists for.
	partial, err := st.CreateProposal(ctx, store.Proposal{
		RepoPath: "/home/kal/code/clanker", State: store.ProposalReady,
		Model: "claude-sonnet-5", MaxTasks: 12, CostUSD: 0.33,
		Focus: []string{"DRY / KISS"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceProposalTasks(ctx, partial.ID, []store.ProposalTask{
		{Key: "a", Goal: "Fold the three retry helpers into one.", Severity: "any",
			Selected: true, CreatedTaskID: 2},
		{Key: "b", Goal: "Drop the dead compatibility shim in cmd/.", Severity: "any", Selected: true},
		{Key: "c", Goal: "Collapse the duplicated table-driven tests.", Severity: "minor", Selected: true},
	}); err != nil {
		t.Fatal(err)
	}

	// 4: failed.
	if _, err := st.CreateProposal(ctx, store.Proposal{
		RepoPath: repo, State: store.ProposalFailed, Model: "claude-sonnet-5",
		MaxTasks: 12, CostUSD: 0.09,
		ErrMsg: `the analysis returned nothing usable: parse proposal: task "wal-mode" depends on "config-schema", which is not an earlier task`,
	}); err != nil {
		t.Fatal(err)
	}
}

func writeAnalysisTranscript(t *testing.T, path string) {
	t.Helper()
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"b71c"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Starting with the README and the build manifest."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"README.md"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"go.mod"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git ls-files | head -100"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"The store package opens SQLite without a busy timeout. Checking whether anything else relies on that."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"sql.Open"}}]}}`,
	}
	var body string
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeLiveTranscript(t *testing.T, path string) {
	t.Helper()
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"a41f","cwd":"/home/kal/code/dc-planner"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Reading the two findings from the exec i2 review."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"The filter is parsed in racks() but never reaches the exporter. I will lift parseRackFilter into a shared helper."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"internal/web/racks.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"internal/web/export.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Threading filter.Columns() into both the header and each row."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go build ./..."}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Now the capitalisation nit — reusing the table header labels."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./internal/web/..."}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok  \toverseer/internal/web\t2.041s"}]}}`,
	}
	var body string
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stderr, "live transcript at", path)
}
