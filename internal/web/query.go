package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Query is the dashboard's whole UI state, carried in the URL.
//
// Everything the operator can change — selection, filter, which step is
// expanded, which right-hand tab is showing — lives here rather than in
// browser memory, for two reasons. The daemon reloads the page on every state
// event, so any state held only in the DOM would be thrown away several times
// a minute; and a URL that carries the view means a link to "the escalated
// task with its fingerprint open" is just a link.
type Query struct {
	Sel int64
	// Repo narrows the board to one repository, and names which repository the
	// backlog panel is showing. Zero is every repository.
	Repo   int64
	Filter string
	Search string
	Group  bool
	Tab    string
	File   string
	// Step is the index of the expanded timeline step. StepSet distinguishes
	// "the operator collapsed everything" from "the operator has not touched
	// the timeline", which is what lets the page open the step worth reading
	// without overriding a deliberate collapse on the next reload.
	Step    int
	StepSet bool
	Overlay string
	Bulk    []int64
	// NoToast suppresses the parked-task nudge once the operator has
	// dismissed it.
	NoToast bool
	// Saved marks the redirect after a settings write, so the pane can say so
	// without a second source of truth for "did that work".
	Saved bool
	// Wizard is the open repository analysis, or zero for none. It is part of
	// the URL for the same reason everything else here is: the analysis takes
	// minutes, the page reloads on every state event while it runs, and a
	// wizard that lived in the DOM would be closed by the first task that
	// changed state underneath it.
	Wizard int64
}

// Tab values for the right-hand pane.
const (
	TabDiff     = "diff"
	TabFindings = "findings"
	TabLive     = "live"
)

// Filter values for the task list.
const (
	FilterAll       = "all"
	FilterAttention = "attention"
	FilterRunning   = "running"
	FilterDone      = "done"
)

// ParseQuery reads the view state out of a request. Every field has a usable
// default, so a bare "/" and a hand-edited URL both render.
func ParseQuery(r *http.Request) Query {
	v := r.URL.Query()
	q := Query{
		Filter:  FilterAll,
		Group:   true,
		Tab:     TabDiff,
		Step:    -1,
		Search:  strings.TrimSpace(v.Get("q")),
		File:    v.Get("file"),
		Overlay: v.Get("overlay"),
		NoToast: v.Get("toast") == "0",
	}
	if s := v.Get("sel"); s != "" {
		q.Sel, _ = strconv.ParseInt(s, 10, 64)
	}
	if s := v.Get("wizard"); s != "" {
		q.Wizard, _ = strconv.ParseInt(s, 10, 64)
	}
	if s := v.Get("repo"); s != "" {
		q.Repo, _ = strconv.ParseInt(s, 10, 64)
	}
	switch f := v.Get("filter"); f {
	case FilterAttention, FilterRunning, FilterDone, FilterAll:
		q.Filter = f
	}
	if v.Has("group") {
		q.Group = v.Get("group") == "1"
	}
	switch t := v.Get("tab"); t {
	case TabDiff, TabFindings, TabLive:
		q.Tab = t
	}
	if s := v.Get("step"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			q.Step, q.StepSet = n, true
		}
	}
	switch q.Overlay {
	case "cli", "add", "settings", "analyses", "repos", "backlog":
	default:
		q.Overlay = ""
	}
	q.Saved = v.Get("saved") == "1"
	for _, s := range strings.Split(v.Get("bulk"), ",") {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil && id > 0 {
			q.Bulk = append(q.Bulk, id)
		}
	}
	return q
}

// URL renders the query as a link, with zero or more key/value overrides
// applied first. Templates call it directly:
//
//	<a href="{{$.Q.URL "sel" .ID "tab" "findings"}}">
//
// An odd number of arguments, or a key that is not a string, is ignored
// rather than panicking inside a template.
func (q Query) URL(pairs ...any) string {
	for i := 0; i+1 < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			continue
		}
		q.set(key, pairs[i+1])
	}

	v := url.Values{}
	if q.Sel != 0 {
		v.Set("sel", strconv.FormatInt(q.Sel, 10))
	}
	if q.Filter != "" && q.Filter != FilterAll {
		v.Set("filter", q.Filter)
	}
	if q.Search != "" {
		v.Set("q", q.Search)
	}
	if !q.Group {
		v.Set("group", "0")
	}
	if q.Tab != "" && q.Tab != TabDiff {
		v.Set("tab", q.Tab)
	}
	if q.File != "" {
		v.Set("file", q.File)
	}
	if q.StepSet {
		v.Set("step", strconv.Itoa(q.Step))
	}
	if q.Overlay != "" {
		v.Set("overlay", q.Overlay)
	}
	if q.Wizard != 0 {
		v.Set("wizard", strconv.FormatInt(q.Wizard, 10))
	}
	if q.Repo != 0 {
		v.Set("repo", strconv.FormatInt(q.Repo, 10))
	}
	if q.Saved {
		v.Set("saved", "1")
	}
	if q.NoToast {
		v.Set("toast", "0")
	}
	if len(q.Bulk) > 0 {
		ids := make([]string, len(q.Bulk))
		for i, id := range q.Bulk {
			ids[i] = strconv.FormatInt(id, 10)
		}
		v.Set("bulk", strings.Join(ids, ","))
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

func (q *Query) set(key string, val any) {
	switch key {
	case "sel":
		q.Sel = toInt64(val)
		// A different task's step indices and diff files mean nothing here.
		q.Step, q.StepSet = -1, false
		q.File = ""
	case "filter":
		q.Filter, _ = val.(string)
	case "q":
		q.Search, _ = val.(string)
	case "group":
		q.Group = toBool(val)
	case "tab":
		q.Tab, _ = val.(string)
	case "file":
		q.File, _ = val.(string)
	case "step":
		q.Step, q.StepSet = int(toInt64(val)), true
	case "overlay":
		q.Overlay, _ = val.(string)
	case "repo":
		q.Repo = toInt64(val)
		// A task selected in one repository means nothing while looking at
		// another, and its step and file indices mean less.
		q.Sel, q.Step, q.StepSet, q.File = 0, -1, false, ""
	case "wizard":
		q.Wizard = toInt64(val)
		// The wizard and the queue dialog are both full-screen; opening one
		// closes the other rather than stacking them.
		if q.Wizard != 0 {
			q.Overlay = ""
		}
	case "bulk":
		q.Bulk = toggleID(q.Bulk, toInt64(val))
	case "clearbulk":
		q.Bulk = nil
	case "toast":
		q.NoToast = !toBool(val)
	case "saved":
		q.Saved = toBool(val)
	}
}

// HasBulk reports whether an ID is in the bulk selection.
func (q Query) HasBulk(id int64) bool {
	for _, x := range q.Bulk {
		if x == id {
			return true
		}
	}
	return false
}

// BulkCSV renders the selection for a form field.
func (q Query) BulkCSV() string {
	ids := make([]string, len(q.Bulk))
	for i, id := range q.Bulk {
		ids[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(ids, ",")
}

func toggleID(ids []int64, id int64) []int64 {
	if id == 0 {
		return ids
	}
	out := make([]int64, 0, len(ids)+1)
	found := false
	for _, x := range ids {
		if x == id {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		out = append(out, id)
	}
	return out
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

func toBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "1" || b == "true"
	}
	return false
}
