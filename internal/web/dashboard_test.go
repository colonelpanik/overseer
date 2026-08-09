package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"overseer/internal/store"
)

func TestQueryRoundTripsThroughItsOwnURL(t *testing.T) {
	// Every control on the page is a link built by Query.URL and read back by
	// ParseQuery. If those two disagree, clicking a filter silently drops the
	// selection.
	start := Query{
		Sel: 7, Filter: FilterAttention, Search: "csv export", Group: false,
		Tab: TabFindings, File: "internal/web/export.go", Step: 3, StepSet: true,
		Overlay: "add", Bulk: []int64{2, 5}, NoToast: true,
	}
	req, err := http.NewRequest(http.MethodGet, start.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := ParseQuery(req)

	if got.Sel != start.Sel || got.Filter != start.Filter || got.Search != start.Search {
		t.Errorf("selection/filter/search = %+v, want %+v", got, start)
	}
	if got.Group != start.Group || got.Tab != start.Tab || got.File != start.File {
		t.Errorf("group/tab/file = %+v, want %+v", got, start)
	}
	if got.Step != start.Step || !got.StepSet || got.Overlay != start.Overlay {
		t.Errorf("step/overlay = %+v, want %+v", got, start)
	}
	if len(got.Bulk) != 2 || got.Bulk[0] != 2 || got.Bulk[1] != 5 {
		t.Errorf("bulk = %v, want [2 5]", got.Bulk)
	}
	if !got.NoToast {
		t.Error("a dismissed toast must stay dismissed across the reload")
	}
}

func TestQueryURLDropsAnotherTasksStepAndFile(t *testing.T) {
	// Step indices and file paths belong to one task. Carrying them across a
	// selection change would open an unrelated step on the next task.
	q := Query{Sel: 1, Step: 4, StepSet: true, File: "a.go", Tab: TabDiff}
	next := q.URL("sel", int64(2))
	if strings.Contains(next, "step=") || strings.Contains(next, "file=") {
		t.Errorf("URL = %q, want the step and file cleared", next)
	}
}

func TestQueryURLTogglesBulkMembership(t *testing.T) {
	q := Query{}
	added := ParseQuery(mustGet(t, q.URL("bulk", int64(3))))
	if len(added.Bulk) != 1 || added.Bulk[0] != 3 {
		t.Fatalf("bulk = %v, want [3]", added.Bulk)
	}
	removed := ParseQuery(mustGet(t, added.URL("bulk", int64(3))))
	if len(removed.Bulk) != 0 {
		t.Errorf("bulk = %v, want empty after toggling the same id off", removed.Bulk)
	}
}

func mustGet(t *testing.T, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestBoardShowsABlockedTaskAndWhatItIsWaitingFor(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	dep, err := st.CreateTask(ctx, store.Task{
		Slug: "sqlite-wal", RepoPath: "/repo", Goal: "WAL mode", State: "failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := st.CreateTask(ctx, store.Task{
		Slug: "config-schema", RepoPath: "/repo", Goal: "Validate config", State: "queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetTaskDeps(ctx, waiter.ID, []int64{dep.ID}); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, "blocked") {
		t.Error("a queued task with an unmet dependency should read as blocked, not queued")
	}
	// A dependency that failed will never reach done, so naming it is the
	// difference between "waiting" and "stuck forever".
	if !strings.Contains(body, "sqlite-wal (failed)") {
		t.Error("the board should name the dependency and its state")
	}

	detail := get(t, s, "/task/2").Body.String()
	if !strings.Contains(detail, "Blocked by a dependency") {
		t.Error("the detail pane should explain the block")
	}
	if !strings.Contains(detail, "/task/2/release") {
		t.Error("the detail pane should offer a way out")
	}
}

func TestBoardRaisesTheBudgetBannerAndOffersARoundNumber(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	task, err := st.CreateTask(ctx, store.Task{
		Slug: "spendy", RepoPath: "/repo", Goal: "Expensive work",
		State: "executing", CostCapUSD: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := st.StartStep(ctx, store.Step{TaskID: task.ID, Phase: "exec", Iteration: 1, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	step.CostUSD = 11.80
	if err := st.FinishStep(ctx, step, nil); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, "$11.80") || !strings.Contains(body, "$8.00 cap") {
		t.Error("the banner should state the spend and the cap it passed")
	}
	// The offered cap has to be above where the task already is, or pressing
	// it does nothing.
	if !strings.Contains(body, "Raise cap to $15.00") {
		t.Errorf("want an offer of $15.00, the next round number above the spend")
	}
	if !strings.Contains(body, "keeps running") {
		t.Error("the banner must say the cap is advisory; it does not stop the task")
	}
}

func TestNextCapAlwaysClearsBothTheSpendAndTheOldCap(t *testing.T) {
	cases := []struct{ spent, cap, want float64 }{
		{11.80, 8, 15},
		{2.00, 8, 10}, // under the cap already: still has to exceed it
		{20.00, 20, 25},
		{0, 0, 5},
	}
	for _, c := range cases {
		got := nextCap(c.spent, c.cap)
		if got != c.want {
			t.Errorf("nextCap(%.2f, %.2f) = %.2f, want %.2f", c.spent, c.cap, got, c.want)
		}
		if got <= c.spent || got <= c.cap {
			t.Errorf("nextCap(%.2f, %.2f) = %.2f must exceed both", c.spent, c.cap, got)
		}
	}
}

func TestEscalatedTaskOffersTheThresholdThatWouldLetItFinish(t *testing.T) {
	s, st := newTestServer(t)
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "nitpicked", RepoPath: "/r", Goal: "g", State: "escalated",
		Phase: "plan", Iteration: 10, MaxIterations: 10, BlockingSeverity: "any",
		ErrMsg: "oscillating on a naming nit",
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/task/1").Body.String()
	if !strings.Contains(body, "Only block on minor") {
		t.Error("a task parked on nits should offer the next threshold up")
	}
	if !strings.Contains(body, "/task/1/severity") {
		t.Error("the offer needs a route behind it")
	}
}

func TestPostSeverityAndCapAndReleaseChangeTheTask(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	dep, err := st.CreateTask(ctx, store.Task{Slug: "dep", RepoPath: "/r", Goal: "g", State: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(ctx, store.Task{
		Slug: "t", RepoPath: "/r", Goal: "g", State: "queued", BlockingSeverity: "any",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetTaskDeps(ctx, task.ID, []int64{dep.ID}); err != nil {
		t.Fatal(err)
	}

	if rec := post(t, s, "/task/2/severity", url.Values{"blocking_severity": {"major"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("severity: status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := post(t, s, "/task/2/cap", url.Values{"cost_cap": {"15"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("cap: status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := post(t, s, "/task/2/release", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("release: status = %d: %s", rec.Code, rec.Body.String())
	}

	got, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BlockingSeverity != "major" {
		t.Errorf("BlockingSeverity = %q, want major", got.BlockingSeverity)
	}
	if got.CostCapUSD != 15 {
		t.Errorf("CostCapUSD = %v, want 15", got.CostCapUSD)
	}
	if deps, _ := st.TaskDeps(ctx, task.ID); len(deps) != 0 {
		t.Errorf("deps = %v, want cleared by release", deps)
	}
}

func TestPostSeverityRejectsAnUnknownThreshold(t *testing.T) {
	s, st := newTestServer(t)
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "t", RepoPath: "/r", Goal: "g", State: "queued",
	}); err != nil {
		t.Fatal(err)
	}
	if rec := post(t, s, "/task/1/severity", url.Values{"blocking_severity": {"whenever"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestBulkContinueAppliesToEverySelectedTask(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	for _, slug := range []string{"a", "b"} {
		if _, err := st.CreateTask(ctx, store.Task{
			Slug: slug, RepoPath: "/r", Goal: "g", State: "escalated",
			Phase: "plan", Iteration: 10, MaxIterations: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}

	rec := post(t, s, "/tasks/bulk", url.Values{"ids": {"1,2"}, "action": {"continue"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	for id := int64(1); id <= 2; id++ {
		task, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == "escalated" {
			t.Errorf("task %d is still escalated after a bulk continue", id)
		}
		if task.MaxIterations != 20 {
			t.Errorf("task %d MaxIterations = %d, want 20", id, task.MaxIterations)
		}
	}
}

func TestBulkReportsTheTasksItCouldNotChange(t *testing.T) {
	// Silently redirecting to a board where half the selection did not move is
	// how an operator ends up believing they stopped something they did not.
	s, st := newTestServer(t)
	ctx := context.Background()
	if _, err := st.CreateTask(ctx, store.Task{
		Slug: "parked", RepoPath: "/r", Goal: "g", State: "escalated",
		Phase: "plan", Iteration: 10, MaxIterations: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, store.Task{
		Slug: "running", RepoPath: "/r", Goal: "g", State: "executing",
	}); err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/tasks/bulk", url.Values{"ids": {"1,2"}, "action": {"continue"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "task 2") {
		t.Errorf("body = %q, want it to name the task that did not change", rec.Body.String())
	}
}

func TestBulkRejectsGarbage(t *testing.T) {
	s, _ := newTestServer(t)
	for name, form := range map[string]url.Values{
		"no ids":         {"action": {"continue"}},
		"bad id":         {"ids": {"1,two"}, "action": {"continue"}},
		"unknown action": {"ids": {"1"}, "action": {"delete"}},
	} {
		if rec := post(t, s, "/tasks/bulk", form); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestPostTasksCarriesConstraintsVerifyCapAndDependencies(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	repo := initRepo(t)
	dep, err := st.CreateTask(ctx, store.Task{
		Slug: "first", RepoPath: repo, Goal: "first", State: "queued",
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/tasks", url.Values{
		"repo":              {repo},
		"goal":              {"Second thing"},
		"verify":            {"go test ./..."},
		"cost_cap":          {"12.50"},
		"blocking_severity": {"major"},
		"constraints":       {"No new dependencies\n\nServer-rendered only\n"},
		"depends_on":        {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	task, err := st.GetTaskBySlug(ctx, "second-thing")
	if err != nil {
		t.Fatal(err)
	}
	if task.VerifyCommand != "go test ./..." {
		t.Errorf("VerifyCommand = %q", task.VerifyCommand)
	}
	if task.CostCapUSD != 12.50 {
		t.Errorf("CostCapUSD = %v, want 12.5", task.CostCapUSD)
	}
	if task.BlockingSeverity != "major" {
		t.Errorf("BlockingSeverity = %q, want major", task.BlockingSeverity)
	}
	// Blank lines must not become empty constraints the agent has to read.
	if task.Constraints != "No new dependencies\nServer-rendered only" {
		t.Errorf("Constraints = %q", task.Constraints)
	}
	deps, err := st.TaskDeps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0] != dep.ID {
		t.Errorf("deps = %v, want [%d]", deps, dep.ID)
	}
}

func TestCLIOverlayRendersTheStatusTable(t *testing.T) {
	s, st := newTestServer(t)
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "csv-export", RepoPath: "/home/kal/code/dc-planner", Goal: "g",
		State: "executing", Phase: "exec", Iteration: 3, MaxIterations: 10,
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/?overlay=cli").Body.String()
	for _, want := range []string{"$ overseer status", "csv-export", "dc-planner", "3/10", "max_parallel"} {
		if !strings.Contains(body, want) {
			t.Errorf("CLI overlay missing %q", want)
		}
	}
}

func TestFilterAndSearchNarrowTheList(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	for _, f := range []struct{ slug, goal, state string }{
		{"a", "Add CSV export", "executing"},
		{"b", "Rewrite the README", "done"},
		{"c", "Fix the retry loop", "escalated"},
	} {
		if _, err := st.CreateTask(ctx, store.Task{
			Slug: f.slug, RepoPath: "/r", Goal: f.goal, State: f.state,
		}); err != nil {
			t.Fatal(err)
		}
	}

	attention := get(t, s, "/?filter=attention").Body.String()
	if !strings.Contains(attention, "Fix the retry loop") {
		t.Error("the attention filter should keep the escalated task")
	}
	if strings.Contains(attention, "Rewrite the README") {
		t.Error("the attention filter should drop the finished task")
	}

	searched := get(t, s, "/?q=readme").Body.String()
	if !strings.Contains(searched, "Rewrite the README") {
		t.Error("search should match the goal text, case-insensitively")
	}
	if strings.Contains(searched, "Add CSV export") {
		t.Error("search should drop what does not match")
	}
}

func TestTimelineOpensTheRunningStepAndTheOperatorCanCollapseIt(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	task, err := st.CreateTask(ctx, store.Task{
		Slug: "t", RepoPath: "/r", Goal: "g", State: "executing",
		Phase: "exec", Iteration: 2, MaxIterations: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	review, err := st.StartStep(ctx, store.Step{TaskID: task.ID, Phase: "exec", Iteration: 1, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	review.Verdict = "changes_requested"
	if err := st.FinishStep(ctx, review, []store.Finding{
		{Severity: "major", Summary: "the finding worth reading", Blocking: true},
	}); err != nil {
		t.Fatal(err)
	}

	opened := get(t, s, "/task/1").Body.String()
	if !strings.Contains(opened, "the finding worth reading") {
		t.Error("a task should open with its findings visible, not all collapsed")
	}
	// step=-1 is a deliberate collapse and must survive the next render.
	collapsed := get(t, s, "/?sel=1&step=-1").Body.String()
	if strings.Contains(collapsed, "the finding worth reading") {
		t.Error("an explicit collapse must not be overridden by the default")
	}
}

// Every assertion here is scoped to the element that renders the thing, closing
// tag included. A bare strings.Contains against the whole page cannot tell the
// row from the detail pane — and worse, a derived headline is a PREFIX of the
// goal it came from, so an unscoped check passes against a template that was
// never touched.
func TestTheTaskRowLeadsWithTheSubjectAndTheDetailKeepsTheGoal(t *testing.T) {
	s, st := newTestServer(t)
	const subject = "Cache the rack inventory query"
	const goal = "Add a cached projection of the rack inventory query. The view " +
		"recomputes the whole join on every request, which will not hold."
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "cache-the-rack-inventory-query", RepoPath: "/tmp/repo",
		Subject: subject, Goal: goal,
		State: "queued", MaxIterations: 10, BlockingSeverity: "any",
	}); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/?sel=1").Body.String()

	// The row. With the template unedited this span holds the whole paragraph,
	// so the closing tag is what gives the assertion its teeth.
	if want := `<span class="goal">` + subject + `</span>`; !strings.Contains(body, want) {
		t.Errorf("the task row does not lead with the subject; want %s", want)
	}
	if strings.Contains(body, `<span class="goal">Add a cached projection`) {
		t.Error("the task row is still rendering the whole goal as its headline")
	}
	// The detail heading, separately: the row could be right and this wrong.
	if want := "<h4>" + subject + "</h4>"; !strings.Contains(body, want) {
		t.Errorf("the detail heading is not the subject; want %s", want)
	}
	// And the goal is not lost — it is the body under that heading, which is
	// the whole point: one line to recognise it, five sentences to explain it.
	if want := `<p class="goalbody">` + goal + `</p>`; !strings.Contains(body, want) {
		t.Errorf("the detail pane does not carry the goal as the body; want %s", want)
	}
}

func TestTheTaskRowDerivesAHeadlineForATaskWithNoSubject(t *testing.T) {
	// Every task queued before this column existed.
	s, st := newTestServer(t)
	const goal = "Add a cached projection of the rack inventory query. " +
		"It recomputes the whole join per request."
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "old", RepoPath: "/tmp/repo", Goal: goal,
		State: "queued", MaxIterations: 10, BlockingSeverity: "any",
	}); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/?sel=1").Body.String()
	want := `<span class="goal">Add a cached projection of the rack inventory query</span>`
	if !strings.Contains(body, want) {
		t.Errorf("a task with no subject is not listed under one line; want %s", want)
	}
	// Belt and braces on the same point: the second sentence must not be in the
	// headline. Without the closing tag above, this test would pass unchanged
	// against the old template, since the derived headline is a prefix of it.
	if strings.Contains(body, `<span class="goal">`+goal) {
		t.Error("the row headline is still the whole goal")
	}
}

func TestBodyOfSuppressesAGoalThatIsOnlyItsHeadline(t *testing.T) {
	cases := []struct{ name, text, headline, want string }{
		// The commonest task there is: one sentence, typed by hand. The
		// headline is those same words without the full stop, so there is
		// nothing left for a body to add — and rendering the sentence twice,
		// once as the heading and once under it, would look like a bug.
		{"one sentence with a full stop", "Do the thing.", "Do the thing", ""},
		{"one sentence without one", "Do the thing", "Do the thing", ""},
		{"wrapped across lines", "Do the\n  thing.", "Do the thing", ""},
		// A wordy goal keeps its body: the first sentence became the headline
		// and the rest is the reason the body exists.
		{"more than one sentence", "First bit. Second bit.", "First bit",
			"First bit. Second bit."},
		// A subject the model wrote in its own words, not a prefix of the goal.
		{"a written subject", "Add a cached projection.", "Cache the join",
			"Add a cached projection."},
	}
	for _, c := range cases {
		if got := bodyOf(c.text, c.headline); got != c.want {
			t.Errorf("%s: bodyOf(%q, %q) = %q, want %q",
				c.name, c.text, c.headline, got, c.want)
		}
	}
}

func TestAShortGoalIsNotRenderedTwice(t *testing.T) {
	// The same thing through the whole render, because the helper being right
	// is only useful if the detail pane actually reads it.
	s, st := newTestServer(t)
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "do-the-thing", RepoPath: "/tmp/repo", Goal: "Do the thing.",
		State: "queued", MaxIterations: 10, BlockingSeverity: "any",
	}); err != nil {
		t.Fatal(err)
	}
	if body := get(t, s, "/?sel=1").Body.String(); strings.Contains(body, `class="goalbody"`) {
		t.Error("a one-sentence goal should not render a body under its own heading")
	}
}

func TestSearchMatchesTheSubject(t *testing.T) {
	s, st := newTestServer(t)
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "cache", RepoPath: "/tmp/repo", Subject: "Cache the rack inventory query",
		Goal:  "Something the search term does not appear in at all.",
		State: "queued", MaxIterations: 10, BlockingSeverity: "any",
	}); err != nil {
		t.Fatal(err)
	}
	if body := get(t, s, "/?q=inventory").Body.String(); !strings.Contains(body, "Cache the rack") {
		t.Error("searching should match a task's subject")
	}
}

func TestPostTasksAcceptsASubject(t *testing.T) {
	s, st := newTestServer(t)
	rec := post(t, s, "/tasks", url.Values{
		"repo":    {initRepo(t)},
		"subject": {"Cache the inventory join"},
		"goal":    {"Add a cached projection. It recomputes the whole join per request."},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	tasks, err := st.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Subject != "Cache the inventory join" {
		t.Fatalf("tasks = %+v, want the submitted subject", tasks)
	}
}
