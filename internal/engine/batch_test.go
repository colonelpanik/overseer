package engine

import (
	"context"
	"strings"
	"testing"
)

func TestParseBatch(t *testing.T) {
	raw := []byte(`
tasks:
  - repo: /home/kal/code/dc-planner
    goal: |
      Add CSV export to the rack inventory view.
    constraints:
      - Server-rendered, no new JS dependencies
  - repo: /home/kal/code/clanker
    goal: Replace the retry logic with a shared backoff helper.
    blocking_severity: major
`)
	b, err := ParseBatch(raw)
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if len(b.Tasks) != 2 {
		t.Fatalf("Tasks = %d, want 2", len(b.Tasks))
	}
	if !strings.Contains(b.Tasks[0].Goal, "CSV export") {
		t.Errorf("goal 0 = %q", b.Tasks[0].Goal)
	}
	if len(b.Tasks[0].Constraints) != 1 {
		t.Errorf("constraints 0 = %v", b.Tasks[0].Constraints)
	}
	if b.Tasks[1].BlockingSeverity != "major" {
		t.Errorf("severity 1 = %q, want major", b.Tasks[1].BlockingSeverity)
	}
	if b.Tasks[0].BlockingSeverity != "" {
		t.Errorf("severity 0 = %q, want empty so the daemon default applies", b.Tasks[0].BlockingSeverity)
	}
}

func TestParseBatchRejectsMissingFields(t *testing.T) {
	for name, raw := range map[string]string{
		"no repo":      "tasks:\n  - goal: do a thing\n",
		"no goal":      "tasks:\n  - repo: /tmp/x\n",
		"no tasks":     "tasks: []\n",
		"bad severity": "tasks:\n  - repo: /tmp/x\n    goal: g\n    blocking_severity: huge\n",
	} {
		if _, err := ParseBatch([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseBatchReportsEmptyDocumentAsNoTasks(t *testing.T) {
	// gopkg.in/yaml.v3 decodes a document with no YAML content at all to
	// io.EOF rather than a nil error with a zero-value Batch, unlike
	// "tasks: []" which decodes cleanly and is only caught by the
	// len(b.Tasks) == 0 check. ParseBatch has a guard translating that EOF
	// into the same "no tasks" error; without it this must not crash or be
	// silently accepted.
	for name, raw := range map[string]string{
		"completely empty":             "",
		"whitespace only (no content)": "   \n\n   \n",
	} {
		_, err := ParseBatch([]byte(raw))
		if err == nil {
			t.Errorf("%s: expected an error, got none", name)
			continue
		}
		if !strings.Contains(err.Error(), "no tasks") {
			t.Errorf("%s: err = %q, want it to mention \"no tasks\"", name, err)
		}
	}
}

func TestParseBatchRejectsMaxParallel(t *testing.T) {
	// max_parallel is daemon config; accepting it in a batch would let a
	// second submit change a run already in flight.
	raw := []byte("max_parallel: 9\ntasks:\n  - repo: /tmp/x\n    goal: g\n")
	if _, err := ParseBatch(raw); err == nil {
		t.Fatal("expected an error naming max_parallel")
	}
}

func TestSubmitCreatesQueuedTaskWithUniqueSlug(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	first, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "Add CSV export"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if first.State != "queued" {
		t.Errorf("State = %q, want queued", first.State)
	}
	if first.Slug != "add-csv-export" {
		t.Errorf("Slug = %q", first.Slug)
	}
	if first.BlockingSeverity != "any" {
		t.Errorf("BlockingSeverity = %q, want the daemon default", first.BlockingSeverity)
	}

	second, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "Add CSV export"})
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if second.Slug == first.Slug {
		t.Errorf("both tasks got slug %q; slugs must be unique", second.Slug)
	}
}

func TestSubmitJoinsConstraints(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task, err := h.eng.Submit(context.Background(), BatchTask{
		Repo: h.repo, Goal: "g", Constraints: []string{"one", "two"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(task.Constraints, want) {
			t.Errorf("Constraints = %q, want it to include %q", task.Constraints, want)
		}
	}
}

func TestSubmitRejectsNonRepoPath(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	if _, err := h.eng.Submit(context.Background(), BatchTask{
		Repo: t.TempDir(), Goal: "g",
	}); err == nil {
		t.Fatal("expected an error for a path that is not a git repository")
	}
}
