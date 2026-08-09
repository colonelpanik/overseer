package engine

import (
	"strings"

	"overseer/internal/store"
)

// spoken is one turn of either conversation, reduced to what replaying it
// needs. The design conversation and the repository chat store their turns in
// different tables, but a transcript handed back to an agent reads the same
// way, and the rules for building one — who is labelled how, what is left out —
// must not diverge between them.
type spoken struct {
	mine   bool
	body   string
	failed bool
}

func chatSpoken(turns []store.ChatTurn) []spoken {
	out := make([]spoken, 0, len(turns))
	for _, t := range turns {
		out = append(out, spoken{
			mine:   t.Speaker == store.SpeakerOperator,
			body:   t.Body,
			failed: t.ErrMsg != "",
		})
	}
	return out
}

func architectSpoken(turns []store.ArchitectTurn) []spoken {
	out := make([]spoken, 0, len(turns))
	for _, t := range turns {
		out = append(out, spoken{
			mine:   t.Speaker == store.SpeakerOperator,
			body:   t.Body,
			failed: t.ErrMsg != "",
		})
	}
	return out
}

// renderConversation writes turns out as a transcript an agent can read.
//
// Deliberately plain: two labels and the text. Anything more decorative would
// be indistinguishable, to the model, from something the operator wrote.
//
// Failed turns are left out. What went wrong is a fact about the daemon rather
// than anything either of them said, and replaying "I could not reply" would
// teach the model that this is a thing it says.
func renderConversation(turns []spoken) string {
	var b strings.Builder
	for _, t := range turns {
		if t.failed {
			continue
		}
		who := "them"
		if t.mine {
			who = "developer"
		}
		b.WriteString(who)
		b.WriteString(": ")
		b.WriteString(strings.TrimSpace(t.body))
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}
