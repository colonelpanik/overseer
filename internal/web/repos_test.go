package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"overseer/internal/store"
)

func TestReposOverlayListsWhatEachRepositoryCost(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	repo, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/widget", Detected: "Go · go test ./..."})
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(ctx, store.Task{
		Slug: "a", RepoID: repo.ID, RepoPath: repo.Path, Goal: "g", State: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := st.StartStep(ctx, store.Step{TaskID: task.ID, Phase: "exec", Agent: "code", Provider: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	step.CostUSD = 1.25
	if err := st.FinishStep(ctx, step, nil); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/?overlay=repos").Body.String()
	for _, want := range []string{"widget", "/src/widget", "Go · go test ./...", "$1.25"} {
		if !strings.Contains(body, want) {
			t.Errorf("Repos overlay does not mention %q", want)
		}
	}
	if !strings.Contains(body, "1 task") {
		t.Error("Repos overlay does not show the repository's task count")
	}
}

// The correction the whole accounting split exists for. Presenting
// subscription-covered CLI usage as money spent would be the dashboard lying.
func TestReposOverlaySeparatesReportedFromMetered(t *testing.T) {
	s, _ := newTestServer(t)
	body := get(t, s, "/?overlay=repos").Body.String()

	if !strings.Contains(body, "reported") || !strings.Contains(body, "metered") {
		t.Fatal("the two spend figures are not both labelled")
	}
	if !strings.Contains(body, "usage signal, not a bill") {
		t.Error("the overlay does not explain that reported usage is not money")
	}
	if !strings.Contains(body, "endpoint you configured") {
		t.Error("the overlay does not say what metered means")
	}
}

// The nav says "reported", not "run spend": the old label implied a bill.
func TestNavLabelsSpendHonestly(t *testing.T) {
	s, _ := newTestServer(t)
	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, ">reported<") {
		t.Error("the nav does not label its spend figure as reported usage")
	}
	if strings.Contains(body, "run spend") {
		t.Error("the nav still says \"run spend\", which reads as a bill")
	}
}

func TestRepoFilterNarrowsTheBoardAndSaysSo(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	a, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/gadget"})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		slug string
		repo store.Repo
		goal string
	}{
		{"in-widget", a, "Widget work here"},
		{"in-gadget", b, "Gadget work here"},
	} {
		if _, err := st.CreateTask(ctx, store.Task{
			Slug: spec.slug, RepoID: spec.repo.ID, RepoPath: spec.repo.Path,
			Goal: spec.goal, State: "queued",
		}); err != nil {
			t.Fatal(err)
		}
	}

	body := get(t, s, "/?repo="+strconv.FormatInt(a.ID, 10)).Body.String()
	if !strings.Contains(body, "Widget work here") {
		t.Error("the filtered board dropped the repository's own task")
	}
	if strings.Contains(body, "Gadget work here") {
		t.Error("the filtered board still shows another repository's task")
	}
	// A filter the operator cannot see is a board that looks empty for no
	// stated reason.
	if !strings.Contains(body, "repo-chip") {
		t.Error("the nav does not show which repository is being filtered to")
	}
}

// Two repositories whose directories share a basename used to merge into one
// group header whose totals belonged to neither.
func TestBoardGroupsByRepositoryNotByBasename(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	a, err := st.UpsertRepo(ctx, store.Repo{Path: "/a/widget"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.UpsertRepo(ctx, store.Repo{Path: "/b/vendor/widget"})
	if err != nil {
		t.Fatal(err)
	}
	for i, repo := range []store.Repo{a, b} {
		if _, err := st.CreateTask(ctx, store.Task{
			Slug:   fmt.Sprintf("t%d", i),
			RepoID: repo.ID, RepoPath: repo.Path,
			Goal: "g", State: "queued",
		}); err != nil {
			t.Fatal(err)
		}
	}

	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, "widget-2") {
		t.Errorf("the two repositories were merged under one header; want a widget and a widget-2 group")
	}
}

func TestBacklogPanelGroupsBySourceAndShowsRecurrence(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	repo, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.AddBacklogItem(ctx, store.BacklogItem{
			RepoID: repo.ID, Source: store.BacklogReview,
			Title:    "csvutil ignores the Flush error",
			Evidence: []string{"internal/csvutil/write.go:41"},
			Severity: "minor", OriginTaskID: 4,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddBacklogItem(ctx, store.BacklogItem{
		RepoID: repo.ID, Source: store.BacklogAnalysis, Title: "validate config at startup",
	}); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/?overlay=backlog&repo="+strconv.FormatInt(repo.ID, 10)).Body.String()
	for _, want := range []string{
		"From reviews", "From analyses",
		"csvutil ignores the Flush error", "seen 3×",
		"internal/csvutil/write.go:41", "raised reviewing task 4",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("backlog panel does not show %q", want)
		}
	}
}

func TestBacklogPanelExplainsWhereReviewItemsComeFrom(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBacklogItem(ctx, store.BacklogItem{
		RepoID: repo.ID, Source: store.BacklogReview, Title: "a nit",
	}); err != nil {
		t.Fatal(err)
	}

	// With no repository named, the panel lands on the one with items waiting.
	body := get(t, s, "/?overlay=backlog").Body.String()
	if !strings.Contains(body, "a nit") {
		t.Error("the panel did not fall back to the repository that has items")
	}
	if !strings.Contains(body, "blocking threshold") ||
		!strings.Contains(body, "deliberately did not act") {
		t.Error("the panel does not explain that these are findings the loop chose not to act on")
	}
}

func TestPostBacklogAddsAnItem(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, store.Repo{Path: initRepo(t)})
	if err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/backlog", url.Values{
		"repo_id": {strconv.FormatInt(repo.ID, 10)},
		"title":   {"tidy the config loader"},
		"detail":  {"it grew three responsibilities"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	items, err := st.ListBacklog(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "tidy the config loader" {
		t.Fatalf("items = %+v, want the one just added", items)
	}
	if items[0].Source != store.BacklogManual {
		t.Errorf("Source = %q, want manual", items[0].Source)
	}
}

func TestPostBacklogQueueCreatesATaskCarryingTheEvidence(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	repo, err := st.UpsertRepo(ctx, store.Repo{Path: initRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.AddBacklogItem(ctx, store.BacklogItem{
		RepoID: repo.ID, Source: store.BacklogReview,
		Title:    "the HTTP client has no timeout",
		Evidence: []string{"internal/fetch/client.go:18"},
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/backlog/"+strconv.FormatInt(item.ID, 10)+"/queue", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	// Straight to the task, because the operator just created work.
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/task/") {
		t.Errorf("Location = %q, want the new task", loc)
	}

	tasks, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].Goal != item.Title {
		t.Errorf("Goal = %q, want the item's title", tasks[0].Goal)
	}
	if !strings.Contains(tasks[0].Constraints, "internal/fetch/client.go:18") {
		t.Errorf("the evidence did not reach the task's constraints:\n%s", tasks[0].Constraints)
	}
}

func TestPostBacklogDismissAndReopen(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	repo, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.AddBacklogItem(ctx, store.BacklogItem{
		RepoID: repo.ID, Source: store.BacklogReview, Title: "a nit",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/backlog/" + strconv.FormatInt(item.ID, 10) + "/dismiss"

	if rec := post(t, s, path, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("dismiss status = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := st.GetBacklogItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.BacklogDismissed {
		t.Fatalf("State = %q, want dismissed", got.State)
	}

	if rec := post(t, s, path, url.Values{"reopen": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("reopen status = %d: %s", rec.Code, rec.Body.String())
	}
	got, err = st.GetBacklogItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.BacklogOpen {
		t.Errorf("State = %q, want open", got.State)
	}
}

func TestPostReposRegistersAndSetsDefaults(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	dir := initRepo(t)

	if rec := post(t, s, "/repos", url.Values{"path": {dir}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	repo, err := st.RepoByPath(ctx, dir)
	if err != nil {
		t.Fatalf("the repository was not registered: %v", err)
	}

	rec := post(t, s, "/repos", url.Values{
		"path": {dir}, "verify": {"make check"},
		"blocking_severity": {"major"}, "cost_cap": {"4"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.VerifyCommand != "make check" || got.BlockingSeverity != "major" || got.CostCapUSD != 4 {
		t.Errorf("defaults = %+v, want them written", got)
	}
}

// The bare add box posts only a path. Treating its absent fields as empty
// values would clear settings the operator never touched.
func TestPostReposWithoutDefaultsFieldsLeavesThemAlone(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	dir := initRepo(t)

	repo, err := st.UpsertRepo(ctx, store.Repo{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	repo.VerifyCommand = "make check"
	repo.CostCapUSD = 4
	if err := st.SaveRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}

	if rec := post(t, s, "/repos", url.Values{"path": {dir}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.VerifyCommand != "make check" || got.CostCapUSD != 4 {
		t.Errorf("defaults = %+v, want them untouched by a path-only add", got)
	}
}

func TestPostRepoArchiveAndRestore(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/repos/" + strconv.FormatInt(repo.ID, 10) + "/archive"

	if rec := post(t, s, path, url.Values{"archived": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("archive status = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Archived() {
		t.Fatal("the repository was not archived")
	}

	if rec := post(t, s, path, url.Values{"archived": {"0"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("restore status = %d: %s", rec.Code, rec.Body.String())
	}
	got, err = st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Archived() {
		t.Error("the repository is still archived after a restore")
	}
}

// Every state-changing route is behind the same-origin check, or a page on
// another site could queue work in this daemon.
func TestEveryRepoRouteRequiresSameOrigin(t *testing.T) {
	s, st := newTestServer(t)
	repo, err := st.UpsertRepo(context.Background(), store.Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.AddBacklogItem(context.Background(), store.BacklogItem{
		RepoID: repo.ID, Source: store.BacklogManual, Title: "x",
	})
	if err != nil {
		t.Fatal(err)
	}

	id := strconv.FormatInt(repo.ID, 10)
	itemID := strconv.FormatInt(item.ID, 10)
	for _, path := range []string{
		"/repos",
		"/repos/" + id + "/archive",
		"/backlog",
		"/backlog/" + itemID + "/queue",
		"/backlog/" + itemID + "/dismiss",
	} {
		rec := crossSitePost(t, s, path, url.Values{"path": {"/src/widget"}})
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s from another site = %d, want 403", path, rec.Code)
		}
	}
}

// Every class the templates use must have a rule in the one stylesheet.
//
// The dashboard is server-rendered with no build step, so a class that exists
// only in the template is not a broken import or a failed compile — it is a
// page that renders as unstyled boxes and looks, to whoever opens it, like the
// design was never applied.
func TestEveryTemplateClassIsStyled(t *testing.T) {
	tpl, err := templateFS.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	css, err := templateFS.ReadFile("templates/style.css")
	if err != nil {
		t.Fatal(err)
	}

	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.([a-zA-Z][\w-]*)`).FindAllStringSubmatch(string(css), -1) {
		defined[m[1]] = true
	}
	// Classes carried for behaviour rather than appearance.
	for _, name := range []string{"inline", "app", "holds-typing"} {
		defined[name] = true
	}

	var missing []string
	// Only literal class attributes; a value containing a template action is
	// not something this can read statically.
	for _, m := range regexp.MustCompile(`class="([^"{}]*)"`).FindAllStringSubmatch(string(tpl), -1) {
		for _, name := range strings.Fields(m[1]) {
			if !defined[name] {
				missing = append(missing, name)
			}
		}
	}
	if len(missing) > 0 {
		t.Errorf("classes used in the template with no rule in style.css: %v", missing)
	}
}

// The two repository surfaces are built from the board's row vocabulary rather
// than a private one, so a list of repositories reads like a list of tasks.
func TestRepoSurfacesReuseTheBoardsRowVocabulary(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	repo, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBacklogItem(ctx, store.BacklogItem{
		RepoID: repo.ID, Source: store.BacklogReview, Title: "a nit",
	}); err != nil {
		t.Fatal(err)
	}

	for _, page := range []string{"/?overlay=repos", "/?overlay=backlog"} {
		body := get(t, s, page).Body.String()
		for _, want := range []string{`class="listrow`, `<span class="rail">`, `class="dot"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not use the shared row shape (%s missing)", page, want)
			}
		}
	}
	// The backlog's group headers are the board's, not a second kind.
	if !strings.Contains(get(t, s, "/?overlay=backlog").Body.String(), `class="grouphead`) {
		t.Error("the backlog panel does not use the board's group header")
	}
}

// Analysing the same repository twice is the normal case, and the second time
// should not need its path typed again.
func TestWizardOffersRegisteredRepositoriesAsADropdown(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	if _, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/widget"}); err != nil {
		t.Fatal(err)
	}
	archived, err := st.UpsertRepo(ctx, store.Repo{Path: "/src/retired"})
	if err != nil {
		t.Fatal(err)
	}
	archived.ArchivedAt = time.Now().UTC()
	if err := st.SaveRepo(ctx, archived); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/?wizard=-1").Body.String()
	if !strings.Contains(body, `<select class="input" id="wrepo" name="repo">`) {
		t.Error("the wizard's first step does not offer a repository dropdown")
	}
	if !strings.Contains(body, `value="widget"`) {
		t.Error("a registered repository is missing from the dropdown")
	}
	// Archiving a repository means not starting new work on it.
	if strings.Contains(body, `value="retired"`) {
		t.Error("an archived repository is still offered for a new analysis")
	}
	// A repository not registered yet must still be reachable.
	if !strings.Contains(body, `name="repo_path"`) {
		t.Error("the wizard no longer accepts a path that is not registered yet")
	}
	if !strings.Contains(body, "Add a repository") {
		t.Error("the wizard does not link to adding a repository")
	}
}

// Before any repository exists there is nothing to choose from, so the first
// step has to stay a plain path field rather than an empty dropdown.
func TestWizardFallsBackToAPathFieldWithNoRepositories(t *testing.T) {
	s, _ := newTestServer(t)
	body := get(t, s, "/?wizard=-1").Body.String()
	if strings.Contains(body, `name="repo"`) {
		t.Error("an empty dropdown is offered when nothing is registered")
	}
	if !strings.Contains(body, `name="repo_path"`) {
		t.Error("the path field is missing")
	}
}

func TestAnalyseAcceptsASlugFromTheDropdownAndAPathFromTheField(t *testing.T) {
	ctx := context.Background()

	// By slug, from the dropdown.
	s, st := newTestServer(t)
	repo, err := st.UpsertRepo(ctx, store.Repo{Path: initRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	if rec := post(t, s, "/analyse", url.Values{"repo": {repo.Slug}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("by slug: status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	props, err := st.ListProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props[0].RepoID != repo.ID {
		t.Fatalf("proposals = %+v, want one against repo %d", props, repo.ID)
	}

	// By path, from the field beneath it.
	s2, st2 := newTestServer(t)
	dir := initRepo(t)
	if rec := post(t, s2, "/analyse", url.Values{"repo_path": {dir}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("by path: status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if _, err := st2.RepoByPath(ctx, dir); err != nil {
		t.Errorf("a path given to the wizard did not register its repository: %v", err)
	}

	// Nothing at all is still an error rather than a proposal against "".
	if rec := post(t, s2, "/analyse", url.Values{}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty submit = %d, want 400", rec.Code)
	}
}
