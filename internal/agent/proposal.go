package agent

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

//go:embed proposal.schema.json
var proposalSchemaFS embed.FS

// ProposalSchema is the JSON Schema the analysis prompt embeds verbatim.
//
// Unlike the verdict schema this is never handed to a CLI flag: `claude -p`
// has no --output-schema, only `codex exec` does. The schema is therefore
// documentation for the model and nothing more, and the actual guarantee comes
// from ParseProposal below. That difference is the reason this parser is as
// strict as it is.
var ProposalSchema = mustReadProposalSchema()

func mustReadProposalSchema() []byte {
	b, err := proposalSchemaFS.ReadFile("proposal.schema.json")
	if err != nil {
		panic("embedded proposal schema missing: " + err.Error())
	}
	return b
}

// DesignSchema is the architect's accept response: the design document and the
// tasks that build it.
//
// Built from ProposalSchema rather than written out again. The task half is the
// same contract whichever door it comes through — that is the whole reason
// ValidateProposedTasks is shared — and a second copy of it would drift the
// first time either was edited.
var DesignSchema = mustBuildDesignSchema()

func mustBuildDesignSchema() []byte {
	var s map[string]any
	if err := json.Unmarshal(ProposalSchema, &s); err != nil {
		panic("proposal schema is not valid JSON: " + err.Error())
	}
	props, ok := s["properties"].(map[string]any)
	if !ok {
		panic("proposal schema has no properties object")
	}
	props["design"] = map[string]any{
		"type": "string",
		"description": "The design, as a markdown document, written for someone who was not " +
			"part of the conversation: what this is, its shape, the decisions actually " +
			"taken and why, and anything ruled out on purpose. No task list.",
	}
	s["required"] = []any{"design", "tasks"}

	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		panic("build design schema: " + err.Error())
	}
	return out
}

// ProposedTask is one task an analysis suggests.
//
// The scalar fields are pointers so a missing key is distinguishable from a
// zero value. An absent "goal" decoding to "" would otherwise produce a task
// with no instruction in it, which the loop would happily start spending money
// on.
//
// Subject is the exception to the strictness the rest of this type is built
// for. It is the one line a task is listed under, and a response without one
// is accepted: the caller derives a subject from the goal instead. Failing a
// whole repository analysis over a missing title would trade thirty minutes of
// reading for a string we can compute.
type ProposedTask struct {
	Key         *string  `json:"key"`
	Subject     *string  `json:"subject"`
	Goal        *string  `json:"goal"`
	Constraints []string `json:"constraints"`
	Verify      *string  `json:"verify"`
	Severity    *string  `json:"blocking_severity"`
	CostCap     *float64 `json:"cost_cap"`
	DependsOn   []string `json:"depends_on"`
	Rationale   *string  `json:"rationale"`
	Evidence    []string `json:"evidence"`
}

// KeyOrEmpty and friends read a field that has passed validation.
func (p ProposedTask) KeyOrEmpty() string     { return deref(p.Key) }
func (p ProposedTask) SubjectOrEmpty() string { return deref(p.Subject) }
func (p ProposedTask) GoalOrEmpty() string    { return deref(p.Goal) }
func (p ProposedTask) VerifyOrEmpty() string  { return deref(p.Verify) }

// SeverityOrDefault returns the proposed threshold, falling back to the
// loosest one rather than to the empty string, which is not a valid setting.
func (p ProposedTask) SeverityOrDefault() string {
	if p.Severity == nil || *p.Severity == "" {
		return "any"
	}
	return *p.Severity
}

// CostCapOrZero returns the proposed cap, zero meaning "take the daemon's".
func (p ProposedTask) CostCapOrZero() float64 {
	if p.CostCap == nil {
		return 0
	}
	return *p.CostCap
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type proposalWire struct {
	Tasks *[]ProposedTask `json:"tasks"`
}

// validSeverities duplicates config.ValidSeverities deliberately: this package
// is the boundary between overseer and a CLI's output, and importing config
// here would make the agent package depend on the daemon's settings to decode
// a response.
var validSeverities = map[string]bool{
	"any": true, "minor": true, "major": true, "critical": true,
}

// ParseProposal decodes and validates an analysis response.
//
// Every deviation is an error rather than a lenient default. This is the same
// posture as ParseVerdict, and for the same reason: what comes back drives
// money being spent without a further human decision on each item, so a
// half-understood response must stop the wizard rather than quietly produce a
// shorter or subtly wrong task list.
//
// max bounds the number of tasks accepted. A model that ignores the budget and
// returns ninety tasks is not obeying the brief, and truncating silently would
// hide that.
func ParseProposal(b []byte, max int) ([]ProposedTask, error) {
	tasks, err := ParseActions(b, max)
	if err != nil {
		return nil, err
	}
	// An analysis that read a whole repository and found nothing worth doing
	// has not succeeded, it has failed to read it. Presenting that as an empty
	// review list would be indistinguishable from a clean repository.
	if len(tasks) == 0 {
		return nil, errors.New("parse proposal: no tasks proposed")
	}
	return tasks, nil
}

// ParseActions decodes a task list, accepting an empty one.
//
// This is the shared decode behind ParseProposal, and the whole of what the
// chat's pull needs: a conversation asked for actions before anything was
// agreed has correctly produced none, and refusing that would turn the normal
// early state of every chat into a failure. The callers that do require work
// to have been proposed say so themselves.
func ParseActions(b []byte, max int) ([]ProposedTask, error) {
	body := stripFence(strings.TrimSpace(string(b)))

	var w proposalWire
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("parse proposal: %w", err)
	}
	// Nothing may follow the object: a second concatenated response would
	// otherwise be silently dropped and the first taken as the whole answer.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse proposal: unexpected data after the JSON object")
	}

	if w.Tasks == nil {
		return nil, errors.New(`parse proposal: missing "tasks"`)
	}
	if err := ValidateProposedTasks(*w.Tasks, max); err != nil {
		return nil, err
	}
	return *w.Tasks, nil
}

// ValidateProposedTasks is the shared check behind every response that carries
// a task list, whether it came from reading a repository, from designing one,
// or from a conversation about one.
//
// Every deviation is an error rather than a lenient default, for the same
// reason ParseVerdict is strict: what comes back drives money being spent
// without a further human decision on each item.
//
// An empty list is not checked here. Whether proposing nothing is a failure
// depends entirely on what was asked — an analysis that found nothing has
// failed, a conversation that has not decided anything yet has not — so each
// caller states its own rule where a reader can see it.
func ValidateProposedTasks(tasks []ProposedTask, max int) error {
	if max > 0 && len(tasks) > max {
		return fmt.Errorf("parse proposal: %d tasks proposed, the brief allowed %d",
			len(tasks), max)
	}

	seen := make(map[string]bool, len(tasks))
	for i, t := range tasks {
		if t.Key == nil || strings.TrimSpace(*t.Key) == "" {
			return fmt.Errorf("parse proposal: task %d has no key", i)
		}
		key := strings.TrimSpace(*t.Key)
		// Duplicate keys would make depends_on ambiguous, and the wizard
		// resolves dependencies by key.
		if seen[key] {
			return fmt.Errorf("parse proposal: duplicate key %q", key)
		}
		if t.Goal == nil || strings.TrimSpace(*t.Goal) == "" {
			return fmt.Errorf("parse proposal: task %q has an empty goal", key)
		}
		if t.Severity != nil && !validSeverities[*t.Severity] {
			return fmt.Errorf("parse proposal: task %q has unknown blocking_severity %q",
				key, *t.Severity)
		}
		if cap := t.CostCapOrZero(); cap < 0 {
			return fmt.Errorf("parse proposal: task %q has a negative cost_cap", key)
		}
		// Dependencies may only point backwards. That rejects both a forward
		// reference and every cycle before any of this reaches the scheduler,
		// where a cycle would be a set of tasks that can only wait for each
		// other.
		for _, dep := range t.DependsOn {
			dep = strings.TrimSpace(dep)
			if dep == key {
				return fmt.Errorf("parse proposal: task %q depends on itself", key)
			}
			if !seen[dep] {
				return fmt.Errorf("parse proposal: task %q depends on %q, which is not an earlier task",
					key, dep)
			}
		}
		seen[key] = true
	}
	return nil
}

// designWire is the architect's final answer: the design the conversation
// arrived at, and the tasks that build it.
type designWire struct {
	Design *string         `json:"design"`
	Tasks  *[]ProposedTask `json:"tasks"`
}

// ParseDesign decodes the architect's accept response.
//
// Same posture as ParseProposal, and the same validation over the task list —
// this is the other door into the same room, so a task list that would be
// rejected coming from an analysis must be rejected coming from a design.
//
// The design document is required and must not be empty. An architect that
// proposes tasks without stating what it decided has produced something nobody
// can review, which is the one thing this whole conversation exists to avoid.
func ParseDesign(b []byte, max int) (string, []ProposedTask, error) {
	body := stripFence(strings.TrimSpace(string(b)))

	var w designWire
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return "", nil, fmt.Errorf("parse design: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", nil, errors.New("parse design: unexpected data after the JSON object")
	}

	if w.Design == nil || strings.TrimSpace(*w.Design) == "" {
		return "", nil, errors.New(`parse design: missing "design"`)
	}
	if w.Tasks == nil {
		return "", nil, errors.New(`parse design: missing "tasks"`)
	}
	// An architect that agreed a design and then proposed nothing to build it
	// has ended the conversation with nothing to show for it.
	if len(*w.Tasks) == 0 {
		return "", nil, errors.New("parse design: no tasks proposed")
	}
	if err := ValidateProposedTasks(*w.Tasks, max); err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(*w.Design), *w.Tasks, nil
}
