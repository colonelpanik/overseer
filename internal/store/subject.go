package store

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxSubjectRunes is how long a subject may be.
//
// The same budget worktree.PRTitle uses, deliberately: a subject becomes the
// pull request title, and two different opinions about what fits on one line
// would mean a task whose title reads one way on the board and another on
// GitHub. Counted in runes rather than bytes so a clamp cannot cut a character
// in half.
const maxSubjectRunes = 72

// minSentenceRunes is the shortest first sentence worth cutting at. Belt and
// braces beside endsAWord, which is the real guard: a headline of two or three
// characters is not worth having whatever produced it.
const minSentenceRunes = 12

// sentenceEnders are the characters that can end a sentence. Each has to be
// followed by a space to be a candidate at all, and only the full stop needs
// the abbreviation check below — nothing is abbreviated with "!" or "?".
const sentenceEnders = ".!?"

// ellipsis marks a subject that had to be cut short. Three dots rather than the
// single character, matching worktree.PRTitle, which counts the same budget in
// the same runes.
const ellipsis = "..."

// NormalizeSubject tidies a subject somebody wrote: an analysis, the architect,
// or an operator typing into the review form.
//
// Whitespace and length only. It deliberately does NOT look for sentences —
// this text is already a title, and the sentence heuristic in Subject below
// exists to guess at where one ends. Applied here it could only ever be wrong:
// "Cache the expensive lookups, e.g. SQLite queries" is one perfectly good
// subject that any full-stop rule is liable to cut down to "Cache the expensive
// lookups, e.g". Keeping the two jobs apart is what makes that impossible
// rather than merely unlikely.
//
// Idempotent, which requeueing a proposal row and re-rendering a stored subject
// both rely on.
func NormalizeSubject(s string) string {
	// Fields splits on every kind of whitespace, so this folds a subject that
	// arrived with a newline in it into one line and collapses runs of spaces
	// in the same pass.
	out := strings.Join(strings.Fields(s), " ")
	if out == "" {
		return ""
	}
	if r := []rune(out); len(r) > maxSubjectRunes {
		out = clampRunes(r)
	}
	// A title does not end in a full stop. An elision does, so it is left
	// alone — "...".
	if strings.HasSuffix(out, ".") && !strings.HasSuffix(out, "..") {
		out = strings.TrimSuffix(out, ".")
	}
	return out
}

// Subject derives a subject from a goal, for when nobody wrote one.
//
// This is the fallback, not the intended path. An analysis and the architect are
// each asked for a subject of their own, because a model that has just written
// five sentences can say what they were about far better than any rule over the
// text can. This exists for everything the model never saw: a goal typed into a
// task file or the dashboard, an item promoted off the backlog, and every row
// written before the column existed. Without it those surfaces would render a
// paragraph where a title goes, which is the whole complaint.
func Subject(goal string) string {
	s := strings.Join(strings.Fields(goal), " ")
	if s == "" {
		return ""
	}
	if first := firstSentence(s); first != "" {
		s = first
	}
	return NormalizeSubject(s)
}

// SubjectOr is the policy every caller wants: the subject somebody supplied,
// tidied, or one derived from the goal when there is none.
//
// It exists so that no call site has to remember which of the two functions
// above applies to which input — the mistake it prevents is exactly the one
// this file is shaped to prevent, applied one layer up.
func SubjectOr(supplied, goal string) string {
	if s := NormalizeSubject(supplied); s != "" {
		return s
	}
	return Subject(goal)
}

// firstSentence returns s up to and including the end of its first sentence, or
// "" when there is no sensible boundary to cut at.
//
// An instruction's first sentence is nearly always the instruction; what
// follows is the qualification. Finding where it ends is the hard part, and the
// hard part is abbreviations: "Cache the expensive lookups, e.g. the rack
// inventory join" and "Cache the expensive lookups, e.g. SQLite queries" both
// contain a full stop followed by a space, and cutting at either produces
// "Cache the expensive lookups, e.g" — a title that says less than no title.
// Two independent tests have to agree before a boundary is believed, because
// each one alone lets a different half of that pair through: endsAWord looks at
// the word before the stop, startsNewSentence at what follows it.
//
// Candidates are walked in order and a rejected one is skipped rather than
// ending the search, which is what makes an abbreviation mid-sentence
// survivable. A first sentence longer than the budget is no use to anyone, and
// every later candidate is longer still, so that ends the search: the clamp in
// NormalizeSubject deals with the whole string instead.
//
// It errs towards finding no boundary. A false negative costs an elided
// headline, which is honest; a false positive silently rewrites the operator's
// text into something that means something else.
func firstSentence(s string) string {
	// Byte indexing is safe for the four ASCII characters this looks at: no
	// byte of a multi-byte UTF-8 sequence is ever below 0x80.
	for i := 0; i+1 < len(s); i++ {
		if !strings.ContainsRune(sentenceEnders, rune(s[i])) || s[i+1] != ' ' {
			continue
		}
		candidate := s[:i+1]
		n := len([]rune(candidate))
		if n > maxSubjectRunes {
			return ""
		}
		if n < minSentenceRunes {
			continue
		}
		if s[i] == '.' && !endsAWord(candidate) {
			continue
		}
		if !startsNewSentence(s[i+2:]) {
			continue
		}
		return candidate
	}
	return ""
}

// endsAWord reports whether candidate's final full stop follows an ordinary
// word rather than an abbreviation or an initial.
//
// Two tells, and neither needs a list of abbreviations to keep up to date: a
// word with a full stop inside it is an abbreviation — "e.g.", "i.e.", "a.m.",
// "Ph.D." — and a single character before a full stop is an initial ("J.
// Smith") or a list marker ("1. Cache the query"). Both are followed by more of
// the same sentence, in whatever case that sentence happens to continue, which
// is why startsNewSentence cannot be the only test.
//
// The cost is a sentence that genuinely ends in a dotted word: "Update main.go.
// It still opens the store without a timeout." finds no boundary and gets an
// elided headline instead of a clean one. That is the direction to be wrong in.
func endsAWord(candidate string) bool {
	word := candidate[:len(candidate)-1] // drop the full stop
	if i := strings.LastIndexByte(word, ' '); i >= 0 {
		word = word[i+1:]
	}
	return len([]rune(word)) > 1 && !strings.Contains(word, ".")
}

// startsNewSentence reports whether rest begins something new rather than more
// of the sentence before it.
//
// A new sentence does not start with a lower-case letter, and an abbreviation
// mid-clause is followed by one: "query. The view recomputes" is two sentences,
// "e.g. the rack inventory join" is one. Anything that is not a lower-case
// letter counts — a capital, a digit ("Cache it. 3 callers do this."), a
// backtick ("Cache the query. `store.go` opens without a timeout.") — because
// none of those continue a clause. An abbreviation followed by a proper noun
// defeats this test on its own, which is what endsAWord is for.
func startsNewSentence(rest string) bool {
	r, _ := utf8.DecodeRuneInString(rest)
	return r != utf8.RuneError && !unicode.IsLower(r)
}

// clampRunes cuts r to the budget, on a word boundary where there is one, and
// marks the elision.
func clampRunes(r []rune) string {
	head := r[:maxSubjectRunes-len(ellipsis)]
	// Back up to the last space, so the subject ends on a word the repository
	// actually contains rather than half of one. A single word longer than the
	// whole budget has no boundary to find, and is cut where it lands.
	for i := len(head) - 1; i >= 0; i-- {
		if head[i] == ' ' {
			head = head[:i]
			break
		}
	}
	// Trailing punctuation left dangling by the cut reads as a typo next to
	// the dots.
	return strings.TrimRight(string(head), " ,;:-") + ellipsis
}

// Headline is the one line a task is listed under: the subject whatever
// proposed it wrote, or one derived from the goal when there is none.
//
// Every display surface reads through this rather than through the column, so
// a row written before the column existed and a row written by an analysis
// that ignored the field both still render a title rather than a paragraph.
func (t Task) Headline() string {
	if s := NormalizeSubject(t.Subject); s != "" {
		return s
	}
	return Subject(t.Goal)
}

// Headline is the proposed task's, on the same terms.
func (t ProposalTask) Headline() string {
	if s := NormalizeSubject(t.Subject); s != "" {
		return s
	}
	return Subject(t.Goal)
}

// Headline is the backlog item's, on the same terms as a task's. The fallback
// derives from Title, which for a review finding or a hand-written item is
// already one line and comes back unchanged.
func (b BacklogItem) Headline() string {
	if s := NormalizeSubject(b.Subject); s != "" {
		return s
	}
	return Subject(b.Title)
}
