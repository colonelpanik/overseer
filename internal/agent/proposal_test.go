package agent

import (
	"strings"
	"testing"
)

// oneTask renders a complete task object, so each test can vary a single
// field rather than restating the whole schema.
func oneTask(key, extra string) string {
	return `{"key":"` + key + `","goal":"Do the thing","constraints":[],` +
		`"verify":"go test ./...","blocking_severity":"any","cost_cap":8,` +
		`"depends_on":[],"rationale":"because","evidence":["a.go:1"]` + extra + `}`
}

func wrap(tasks ...string) []byte {
	return []byte(`{"tasks":[` + strings.Join(tasks, ",") + `]}`)
}

func TestParseProposalReadsACompleteResponse(t *testing.T) {
	got, err := ParseProposal(wrap(oneTask("wal-mode", "")), 12)
	if err != nil {
		t.Fatalf("ParseProposal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("tasks = %d, want 1", len(got))
	}
	task := got[0]
	if task.KeyOrEmpty() != "wal-mode" || task.GoalOrEmpty() != "Do the thing" {
		t.Errorf("task = %+v", task)
	}
	if task.VerifyOrEmpty() != "go test ./..." {
		t.Errorf("verify = %q", task.VerifyOrEmpty())
	}
	if task.SeverityOrDefault() != "any" || task.CostCapOrZero() != 8 {
		t.Errorf("severity/cap = %q/%v", task.SeverityOrDefault(), task.CostCapOrZero())
	}
}

func TestParseProposalStripsAMarkdownFence(t *testing.T) {
	// The prompt asks for bare JSON, and the model fences it anyway often
	// enough that refusing would fail runs for no benefit.
	raw := "```json\n" + string(wrap(oneTask("k", ""))) + "\n```"
	if _, err := ParseProposal([]byte(raw), 12); err != nil {
		t.Errorf("a fenced response should still parse: %v", err)
	}
}

func TestParseProposalRejectsMalformedResponses(t *testing.T) {
	cases := map[string]string{
		"not json":        `sure! here is the list`,
		"missing tasks":   `{}`,
		"empty tasks":     `{"tasks":[]}`,
		"unknown field":   `{"tasks":[],"summary":"hi"}`,
		"trailing object": string(wrap(oneTask("k", ""))) + `{"tasks":[]}`,
		"null tasks":      `{"tasks":null}`,
	}
	for name, raw := range cases {
		if _, err := ParseProposal([]byte(raw), 12); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseProposalRejectsAnIncompleteTask(t *testing.T) {
	cases := map[string][]byte{
		"no key": wrap(`{"goal":"g","constraints":[],"verify":null,` +
			`"blocking_severity":"any","cost_cap":null,"depends_on":[],` +
			`"rationale":"r","evidence":[]}`),
		"blank key": wrap(`{"key":"  ","goal":"g","constraints":[],"verify":null,` +
			`"blocking_severity":"any","cost_cap":null,"depends_on":[],` +
			`"rationale":"r","evidence":[]}`),
		"no goal": wrap(`{"key":"k","constraints":[],"verify":null,` +
			`"blocking_severity":"any","cost_cap":null,"depends_on":[],` +
			`"rationale":"r","evidence":[]}`),
		"blank goal": wrap(`{"key":"k","goal":"   ","constraints":[],"verify":null,` +
			`"blocking_severity":"any","cost_cap":null,"depends_on":[],` +
			`"rationale":"r","evidence":[]}`),
	}
	for name, raw := range cases {
		if _, err := ParseProposal(raw, 12); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseProposalRejectsADuplicateKey(t *testing.T) {
	// depends_on is resolved by key, so two tasks sharing one makes every
	// dependency naming it ambiguous.
	_, err := ParseProposal(wrap(oneTask("same", ""), oneTask("same", "")), 12)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %v, want it to name the duplicate", err)
	}
}

func TestParseProposalRejectsAnUnknownSeverity(t *testing.T) {
	raw := wrap(`{"key":"k","goal":"g","constraints":[],"verify":null,` +
		`"blocking_severity":"whenever","cost_cap":null,"depends_on":[],` +
		`"rationale":"r","evidence":[]}`)
	if _, err := ParseProposal(raw, 12); err == nil {
		t.Error("expected an error for an unknown blocking_severity")
	}
}

func TestParseProposalRejectsANegativeCap(t *testing.T) {
	raw := wrap(`{"key":"k","goal":"g","constraints":[],"verify":null,` +
		`"blocking_severity":"any","cost_cap":-3,"depends_on":[],` +
		`"rationale":"r","evidence":[]}`)
	if _, err := ParseProposal(raw, 12); err == nil {
		t.Error("expected an error for a negative cost_cap")
	}
}

func TestParseProposalOnlyAllowsBackwardDependencies(t *testing.T) {
	// Backward-only is what makes a cycle impossible before any of this
	// reaches the scheduler, where a cycle is a set of tasks that can only
	// wait for each other.
	backward := wrap(oneTask("first", ""),
		`{"key":"second","goal":"g","constraints":[],"verify":null,`+
			`"blocking_severity":"any","cost_cap":null,"depends_on":["first"],`+
			`"rationale":"r","evidence":[]}`)
	if _, err := ParseProposal(backward, 12); err != nil {
		t.Errorf("a backward dependency should be accepted: %v", err)
	}

	forward := wrap(
		`{"key":"first","goal":"g","constraints":[],"verify":null,`+
			`"blocking_severity":"any","cost_cap":null,"depends_on":["second"],`+
			`"rationale":"r","evidence":[]}`,
		oneTask("second", ""))
	if _, err := ParseProposal(forward, 12); err == nil {
		t.Error("a forward dependency should be rejected")
	}

	self := wrap(`{"key":"k","goal":"g","constraints":[],"verify":null,` +
		`"blocking_severity":"any","cost_cap":null,"depends_on":["k"],` +
		`"rationale":"r","evidence":[]}`)
	if _, err := ParseProposal(self, 12); err == nil {
		t.Error("a self-dependency should be rejected")
	}

	unknown := wrap(`{"key":"k","goal":"g","constraints":[],"verify":null,` +
		`"blocking_severity":"any","cost_cap":null,"depends_on":["nowhere"],` +
		`"rationale":"r","evidence":[]}`)
	if _, err := ParseProposal(unknown, 12); err == nil {
		t.Error("a dependency on a key that does not exist should be rejected")
	}
}

func TestParseProposalEnforcesTheTaskBudget(t *testing.T) {
	// Truncating silently would hide that the model ignored the brief, and
	// the operator would never know the list they are reviewing is a
	// fragment of what came back.
	var tasks []string
	for _, k := range []string{"a", "b", "c", "d"} {
		tasks = append(tasks, oneTask(k, ""))
	}
	if _, err := ParseProposal(wrap(tasks...), 3); err == nil {
		t.Error("expected an error when more tasks come back than were allowed")
	}
	if _, err := ParseProposal(wrap(tasks...), 4); err != nil {
		t.Errorf("exactly the budget should be accepted: %v", err)
	}
	if _, err := ParseProposal(wrap(tasks...), 0); err != nil {
		t.Errorf("a zero budget should mean no limit: %v", err)
	}
}

func TestProposedTaskDefaultsAreUsable(t *testing.T) {
	// Nulls are on-schema for the optional fields, and every reader has to
	// get a value it can put straight into a task row.
	raw := wrap(`{"key":"k","goal":"g","constraints":[],"verify":null,` +
		`"blocking_severity":"any","cost_cap":null,"depends_on":[],` +
		`"rationale":"r","evidence":[]}`)
	got, err := ParseProposal(raw, 12)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].VerifyOrEmpty() != "" {
		t.Errorf("verify = %q, want empty", got[0].VerifyOrEmpty())
	}
	if got[0].CostCapOrZero() != 0 {
		t.Errorf("cap = %v, want 0 so the daemon default applies", got[0].CostCapOrZero())
	}
	if got[0].SeverityOrDefault() != "any" {
		t.Errorf("severity = %q, want any", got[0].SeverityOrDefault())
	}
}

func TestParseActionsAcceptsAConversationThatDecidedNothing(t *testing.T) {
	// A chat that has been asked to produce actions before anything was
	// agreed has correctly produced none. That is a different outcome from
	// an analysis returning nothing, which means the analysis failed.
	got, err := ParseActions([]byte(`{"tasks":[]}`), 12)
	if err != nil {
		t.Fatalf("ParseActions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tasks = %d, want 0", len(got))
	}
}

func TestParseActionsIsAsStrictAsParseProposalAboutEverythingElse(t *testing.T) {
	// Moving the emptiness rule out of the shared validator must not have
	// loosened any of the rules that stop money being spent on nonsense.
	cases := map[string]string{
		"not json":        `sure! here is the list`,
		"missing tasks":   `{}`,
		"unknown field":   `{"tasks":[],"summary":"hi"}`,
		"trailing object": string(wrap(oneTask("k", ""))) + `{"tasks":[]}`,
		"null tasks":      `{"tasks":null}`,
		"forward dep": string(wrap(
			`{"key":"a","goal":"g","constraints":[],"verify":null,`+
				`"blocking_severity":"any","cost_cap":null,"depends_on":["b"],`+
				`"rationale":"r","evidence":[]}`,
			oneTask("b", ""))),
		"duplicate key": string(wrap(oneTask("k", ""), oneTask("k", ""))),
	}
	for name, raw := range cases {
		if _, err := ParseActions([]byte(raw), 12); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestAnEmptyTaskListStillFailsAnAnalysisAndADesign(t *testing.T) {
	// The rule moved out of ValidateProposedTasks and into these two callers;
	// if it went missing on the way, an analysis that read nothing would
	// present an empty review list as a success.
	if _, err := ParseProposal([]byte(`{"tasks":[]}`), 12); err == nil {
		t.Error("an analysis proposing nothing should be an error")
	}
	if _, _, err := ParseDesign([]byte(`{"design":"# Design","tasks":[]}`), 12); err == nil {
		t.Error("a design with nothing to build should be an error")
	}
}

func TestProposalSchemaIsEmbedded(t *testing.T) {
	// The prompt pastes this in verbatim; an empty schema would silently
	// become a prompt with no contract in it.
	if len(ProposalSchema) == 0 {
		t.Fatal("ProposalSchema is empty")
	}
	for _, want := range []string{"blocking_severity", "depends_on", "evidence"} {
		if !strings.Contains(string(ProposalSchema), want) {
			t.Errorf("schema is missing %q", want)
		}
	}
}

func TestParseProposalReadsTheSubject(t *testing.T) {
	body := `{"tasks":[{"key":"cache","subject":"Cache the rack inventory query",` +
		`"goal":"Add a cached projection of the rack inventory query. It recomputes the whole join per request.",` +
		`"constraints":[],"verify":"go test ./...","blocking_severity":"any",` +
		`"cost_cap":null,"depends_on":[],"rationale":"why","evidence":["a.go:1"]}]}`
	tasks, err := ParseProposal([]byte(body), 12)
	if err != nil {
		t.Fatalf("ParseProposal: %v", err)
	}
	if got := tasks[0].SubjectOrEmpty(); got != "Cache the rack inventory query" {
		t.Errorf("subject = %q, want the one in the response", got)
	}
}

func TestParseProposalAcceptsAResponseWithNoSubject(t *testing.T) {
	// The strictness this parser is otherwise famous for exists because what
	// comes back drives money being spent. A subject drives a title, so a
	// model that omits it must not cost the operator the whole analysis: the
	// engine derives one from the goal instead.
	body := `{"tasks":[{"key":"cache","goal":"Do the thing.","constraints":[],` +
		`"verify":null,"blocking_severity":"any","cost_cap":null,` +
		`"depends_on":[],"rationale":"why","evidence":[]}]}`
	tasks, err := ParseProposal([]byte(body), 12)
	if err != nil {
		t.Fatalf("ParseProposal: %v", err)
	}
	if got := tasks[0].SubjectOrEmpty(); got != "" {
		t.Errorf("subject = %q, want empty so the caller can derive one", got)
	}
}

func TestParseDesignReadsTheSubject(t *testing.T) {
	body := `{"design":"# It","tasks":[{"key":"scaffold","subject":"Scaffold the module",` +
		`"goal":"Create the module layout and a passing test command.","constraints":[],` +
		`"verify":"go test ./...","blocking_severity":"any","cost_cap":null,` +
		`"depends_on":[],"rationale":"why","evidence":[]}]}`
	_, tasks, err := ParseDesign([]byte(body), 12)
	if err != nil {
		t.Fatalf("ParseDesign: %v", err)
	}
	if got := tasks[0].SubjectOrEmpty(); got != "Scaffold the module" {
		t.Errorf("subject = %q, want the one in the response", got)
	}
}
