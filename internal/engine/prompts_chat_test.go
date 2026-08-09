package engine

import (
	"strings"
	"testing"

	"overseer/internal/agent"
)

func TestChatPromptDemandsCitationsAndRefusesToProduceATaskList(t *testing.T) {
	// Both clauses are the whole difference between this and the architect.
	// An answer that cannot be checked against the tree is worth nothing here,
	// and a reply that turns into a task list has stopped being a conversation
	// — there is a button for that, and it is a separate paid turn.
	got := ChatPrompt("Go · go test ./... · 412 tracked files", "")

	for _, want := range []string{
		"READ-ONLY",
		"file:line",
		"guess",
		"412 tracked files",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("chat prompt is missing %q", want)
		}
	}
	if !strings.Contains(got, "Do not produce a task list") {
		t.Error("the chat must be told not to produce a task list")
	}
	// The pull is a separate turn against a separate session; a chat that
	// emitted JSON would put an unreadable blob in the transcript.
	if strings.Contains(got, string(agent.ProposalSchema)) {
		t.Error("the conversation prompt must not carry the task schema")
	}
}

func TestChatPromptCarriesTheConversationOnlyWhenReseeding(t *testing.T) {
	// The opening turn has nothing to restate. A re-seed after a lost session
	// has everything to restate, because the agent's own memory of it is gone.
	opening := ChatPrompt("Go", "")
	if strings.Contains(opening, "SO FAR") {
		t.Error("the opening turn should not claim there is a conversation already")
	}

	reseeded := ChatPrompt("Go", "you: why?\nassistant: because")
	if !strings.Contains(reseeded, "you: why?") {
		t.Error("a re-seeded prompt must carry what was already said")
	}
}

func TestPullPromptAsksForDecisionsAndAcceptsFindingNone(t *testing.T) {
	// The two failure modes this prompt exists to prevent: turning everything
	// that was merely discussed into work, and inventing something to say when
	// the honest answer is that nothing has been agreed yet.
	got := PullActionsPrompt("you: what about X?\nassistant: because Y", "Go",
		[]string{"add the in-flight column"}, 12)

	for _, want := range []string{
		"DECIDED",
		"you: what about X?",
		"add the in-flight column",
		"12",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pull prompt is missing %q", want)
		}
	}
	// Stated however it is worded, this is the clause that stops a model
	// inventing work for a conversation that has not agreed anything yet.
	if !strings.Contains(strings.ToLower(got), "empty") {
		t.Error("the pull prompt must say that proposing nothing is acceptable")
	}
	// The parser on the other side is the actual guarantee, but the schema is
	// what gives the model a chance of satisfying it.
	if !strings.Contains(got, strings.TrimSpace(string(agent.ProposalSchema))) {
		t.Error("the pull prompt must embed the task schema verbatim")
	}
}

func TestPullPromptSaysNothingAboutAlreadyFiledWorkWhenThereIsNone(t *testing.T) {
	// A first pull that was told "do not propose these again" followed by an
	// empty list reads as an instruction to propose nothing.
	got := PullActionsPrompt("you: hello", "Go", nil, 12)
	if strings.Contains(got, "ALREADY PULLED") {
		t.Error("the first pull should not mention work that does not exist")
	}
}
