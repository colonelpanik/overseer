package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The wordy goal an analysis actually produces: the instruction in the first
// sentence, four more sentences of qualification behind it.
const wordyGoal = `Add a cached projection of the rack inventory query. ` +
	`The view recomputes the whole join on every request, which is fine at ` +
	`the current row count and will not be at ten times it. Cache it in the ` +
	`store rather than in the handler, so the invalidation lives beside the ` +
	`writes that need it.`

func TestSubjectTakesTheFirstSentenceOfAWordyGoal(t *testing.T) {
	got := Subject(wordyGoal)
	want := "Add a cached projection of the rack inventory query"
	if got != want {
		t.Errorf("Subject = %q, want %q", got, want)
	}
}

func TestSubjectKeepsAShortGoalWholeWithoutItsFullStop(t *testing.T) {
	// A title does not end in a full stop, and the slug this feeds already
	// dropped it, so the two must not disagree.
	got := Subject("Add CSV export to the rack inventory view.")
	want := "Add CSV export to the rack inventory view"
	if got != want {
		t.Errorf("Subject = %q, want %q", got, want)
	}
}

func TestSubjectCollapsesEverySortOfWhitespace(t *testing.T) {
	got := Subject("  Enable WAL mode\n\ton the store\n  connection  ")
	want := "Enable WAL mode on the store connection"
	if got != want {
		t.Errorf("Subject = %q, want %q", got, want)
	}
}

func TestSubjectClampsAOneSentenceGoalOnAWordBoundary(t *testing.T) {
	// One sentence, no full stop to cut at, far past the budget: the only
	// option left is to elide — but not mid-word, which would invent a word
	// the repository does not contain.
	goal := "Replace the hand-rolled retry logic in the transport package " +
		"with the shared exponential backoff helper everything else uses"
	got := Subject(goal)
	if n := utf8.RuneCountInString(got); n > 72 {
		t.Errorf("Subject = %q (%d runes), want at most 72", got, n)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("Subject = %q, want an elision marker", got)
	}
	head := strings.TrimSuffix(got, "...")
	if !strings.HasPrefix(goal, head) {
		t.Errorf("Subject = %q, want a prefix of the goal", got)
	}
	if strings.HasSuffix(head, " ") {
		t.Errorf("Subject = %q, want no space before the elision", got)
	}
	// The word boundary: whatever survived must be whole words.
	if last := strings.LastIndexByte(head, ' '); last < 0 {
		t.Errorf("Subject = %q, want more than one word kept", got)
	} else if !strings.Contains(goal, head[last+1:]+" ") {
		t.Errorf("Subject = %q ends mid-word", got)
	}
}

func TestSubjectDoesNotSplitAMultiByteCharacter(t *testing.T) {
	// The clamp counts runes, not bytes. Counting bytes here would cut a
	// three-byte character in half and render as a replacement glyph.
	goal := strings.Repeat("日", 200)
	got := Subject(goal)
	if !utf8.ValidString(got) {
		t.Errorf("Subject = %q, want valid UTF-8", got)
	}
	if n := utf8.RuneCountInString(got); n > 72 {
		t.Errorf("Subject = %q (%d runes), want at most 72", got, n)
	}
}

func TestSubjectIgnoresAnAbbreviationsFullStop(t *testing.T) {
	// Both halves of the abbreviation problem, and each defeats one of the two
	// tests on its own — which is why both tests exist. The first is caught by
	// what follows the stop being lower case; the second is not, because a
	// proper noun is capitalised exactly like a new sentence, and is caught by
	// the word before the stop containing a stop of its own.
	//
	// Cutting at either would leave "Cache the expensive lookups, e.g", which
	// says less than no title at all. Note that a length guard cannot help: the
	// candidate is a perfectly plausible 33 characters.
	for _, goal := range []string{
		"Cache the expensive lookups, e.g. the rack inventory join",
		"Cache the expensive lookups, e.g. SQLite queries",
		"Bound the retry window, i.e. Go's context deadline, per call",
		"Ask J. Smith which of the two projections is canonical here",
	} {
		if got := Subject(goal); got != goal {
			t.Errorf("Subject(%q) = %q, want it left whole", goal, got)
		}
	}
}

func TestSubjectEndsASentenceOnAQuestionOrAnExclamation(t *testing.T) {
	// A full stop is not the only way a sentence ends, and neither of these
	// needs the abbreviation check: nothing is abbreviated with "!" or "?".
	// They also keep their punctuation, unlike a full stop — a title does not
	// end in a stop, but it can certainly ask something.
	cases := []struct{ goal, want string }{
		{"Does the cache invalidate? Yes, on every write to the table.",
			"Does the cache invalidate?"},
		{"Fix the crash! It happens on every request to the view.",
			"Fix the crash!"},
	}
	for _, c := range cases {
		if got := Subject(c.goal); got != c.want {
			t.Errorf("Subject(%q) = %q, want %q", c.goal, got, c.want)
		}
	}
}

func TestNormalizeSubjectLeavesAnAuthoredSubjectAlone(t *testing.T) {
	// The reason the two jobs are separate functions. Everything here is
	// already a title; the sentence heuristic has no business touching it, and
	// this is the test that says so.
	for _, subject := range []string{
		"Cache the expensive lookups, e.g. SQLite queries",
		"Cache the rack inventory query",
		"Does the cache invalidate?",
	} {
		if got := NormalizeSubject(subject); got != subject {
			t.Errorf("NormalizeSubject(%q) = %q, want it untouched", subject, got)
		}
	}

	// It still tidies: one line, no trailing full stop, inside the budget.
	if got := NormalizeSubject("Cache the query.\nAlso add a test."); got != "Cache the query. Also add a test" {
		t.Errorf("NormalizeSubject = %q, want it folded onto one line", got)
	}
	if got := NormalizeSubject(strings.Repeat("日", 90)); utf8.RuneCountInString(got) > 72 {
		t.Errorf("NormalizeSubject = %q, want at most 72 runes", got)
	}
}

func TestSubjectOrPrefersWhatSomebodyWrote(t *testing.T) {
	if got := SubjectOr("Cache the join", wordyGoal); got != "Cache the join" {
		t.Errorf("SubjectOr = %q, want the supplied subject", got)
	}
	if got := SubjectOr("   ", wordyGoal); got != Subject(wordyGoal) {
		t.Errorf("SubjectOr = %q, want one derived from the goal", got)
	}
	// A supplied subject goes through NormalizeSubject, not Subject, so an
	// abbreviation in it survives being stored.
	if got := SubjectOr("Cache the lookups, e.g. SQLite queries", wordyGoal); got != "Cache the lookups, e.g. SQLite queries" {
		t.Errorf("SubjectOr = %q, want the supplied subject intact", got)
	}
}

func TestSubjectSkipsAFragmentAndTakesTheSentenceAfterIt(t *testing.T) {
	// A flattened numbered list. The first full stop follows a single character,
	// which is a list marker rather than the end of a sentence — the same tell
	// that catches an initial in "Ask J. Smith". Rejecting a candidate must not
	// end the search, or the whole list becomes the subject.
	got := Subject("1. Cache the query. 2. Add a test.")
	want := "1. Cache the query"
	if got != want {
		t.Errorf("Subject = %q, want %q", got, want)
	}
}

func TestSubjectTreatsANonLetterAsANewSentence(t *testing.T) {
	// An identifier opening the second sentence is still a second sentence.
	got := Subject("Cache the query. `store.go` opens without a busy timeout.")
	want := "Cache the query"
	if got != want {
		t.Errorf("Subject = %q, want %q", got, want)
	}
}

func TestSubjectIsIdempotent(t *testing.T) {
	// A model-supplied subject is passed through this same function, and a
	// requeue passes an already-normalised one through again.
	once := Subject(wordyGoal)
	if twice := Subject(once); twice != once {
		t.Errorf("Subject(Subject(x)) = %q, want %q", twice, once)
	}
}

func TestSubjectOfNothingIsNothing(t *testing.T) {
	if got := Subject("   \n\t "); got != "" {
		t.Errorf("Subject = %q, want empty", got)
	}
}
