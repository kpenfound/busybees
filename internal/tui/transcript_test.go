package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/session"
)

// fixtureTranscript is a real session's transcript.jsonl, trimmed to the
// lines that carry each shape the renderer has a branch for and with the
// long strings shortened. It is a reviewer session from
// <state_dir>/sessions/, so the input shape is the one production writes
// rather than one invented for a test — the tool names, the nesting of a
// tool result's content and the bookkeeping lines in between are all
// claude's own.
const fixtureTranscript = "testdata/transcript.jsonl"

// A transcript reads the way Claude Code's own output does: what the
// session said, what it called and how each call answered, one line each.
// Everything the stream carries for the runner rather than for a reader —
// the init line, the thinking-token and task bookkeeping, the rate-limit
// events — is dropped, and a thought is a marker rather than the thought.
func TestTheTranscriptRendersWhatASessionSaidAndDid(t *testing.T) {
	lines, off, err := readTranscript("testdata", 0)
	if err != nil {
		t.Fatalf("reading the fixture transcript: %v", err)
	}
	st, err := os.Stat(fixtureTranscript)
	if err != nil {
		// The arm that fires if the dagger.toml includeExtraFiles entry is
		// ever dropped: the check container mounts only Go sources, so the
		// fixture is simply absent there. Naming it beats dereferencing the
		// nil FileInfo os.Stat returns with the error, which panics and
		// takes the whole package's tests with it.
		t.Fatalf("read %d bytes of the fixture transcript (%v)", off, err)
	}
	if off != st.Size() {
		t.Fatalf("read %d bytes of a %d-byte transcript", off, st.Size())
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		// what the session said
		"● I'll start by reading the diff",
		// a tool call, named by the argument that says what it is about
		`● Bash(git log --oneline -6 `,
		// how it answered: the first line, and how much more there was
		"  ⎿ e0be9c5 init: validate before writing anything (+5 lines)",
		// a thought is a marker, never the thought
		"✻ thinking",
		// an MCP tool has no named argument, so its own are listed
		"● mcp__bees__issue_view(number=88)",
		// a tool that failed says so
		"  ⎿ error: issue 88 is outside the factory's filter",
		// the final event of the stream
		"● session ended: ok, 24 turns, $1.28",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendered transcript does not contain %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		"thinking_tokens", "task_started", "rate_limit", "session_id", "toolu_",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the rendered transcript still carries %q:\n%s", unwanted, got)
		}
	}
	// Every rendered line is one line: a reader scrolls through lines, and a
	// cell that carries its own newline would make the count a lie.
	for _, l := range lines {
		if strings.Contains(l, "\n") {
			t.Errorf("a rendered transcript line carries a newline: %q", l)
		}
	}
}

// The transcript is read while the runner is writing it, so its last line is
// regularly half an object. Only whole lines are consumed: the half is left
// where it is and read once the newline that ends it arrives, rather than
// being parsed as far as it goes and then rendered twice.
func TestOnlyWholeTranscriptLinesAreRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, session.TranscriptFile)
	whole := `{"type":"assistant","message":{"content":[{"type":"text","text":"reading the diff"}]}}` + "\n"
	half := `{"type":"assistant","message":{"content":[{"type":"text","text":"and now the `
	if err := os.WriteFile(path, []byte(whole+half), 0o644); err != nil {
		t.Fatal(err)
	}

	lines, off, err := readTranscript(dir, 0)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "reading the diff") {
		t.Fatalf("the first read did not stop at the half line: %q", lines)
	}
	if off != int64(len(whole)) {
		t.Fatalf("the first read consumed %d bytes, want the %d of the whole line", off, len(whole))
	}

	// The runner finishes the line and writes another.
	rest := `tests"}]}}` + "\n" + `{"type":"result","subtype":"success","num_turns":3,"total_cost_usd":0.5}` + "\n"
	if err := os.WriteFile(path, []byte(whole+half+rest), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, off, err = readTranscript(dir, off)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(lines) != 2 || !strings.Contains(lines[0], "and now the tests") {
		t.Fatalf("the completed line was not read whole: %q", lines)
	}
	if strings.Contains(strings.Join(lines, "\n"), "reading the diff") {
		t.Error("the second read rendered a line the first one had already rendered")
	}
	if off != int64(len(whole+half+rest)) {
		t.Errorf("the second read stopped at %d, want the whole %d-byte file", off, len(whole+half+rest))
	}
}

// A session that has not written anything yet has no transcript at all: the
// directory is created before claude is started, so a missing file is the
// ordinary first moment of a session and never an error to report.
func TestAMissingTranscriptIsNotAnError(t *testing.T) {
	lines, off, err := readTranscript(t.TempDir(), 0)
	if err != nil || len(lines) != 0 || off != 0 {
		t.Errorf("readTranscript on a session with no transcript = (%q, %d, %v), want nothing at all", lines, off, err)
	}
	if lines, off, err := readTranscript("", 0); err != nil || len(lines) != 0 || off != 0 {
		t.Errorf("readTranscript on a session with no directory = (%q, %d, %v), want nothing at all", lines, off, err)
	}
}

// A line the view cannot make sense of is dropped rather than shown or
// returned as an error: the whole session's output is worth more than one
// unparseable line of it.
func TestALineTheViewCannotParseIsDropped(t *testing.T) {
	for _, line := range []string{
		"", "   ", "not json at all", `{"type":"assistant"}`,
		`{"type":"user","message":{"content":[]}}`,
		`{"type":"system","subtype":"init"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
	} {
		if got := renderTranscriptLine([]byte(line)); len(got) != 0 {
			t.Errorf("renderTranscriptLine(%q) = %q, want nothing", line, got)
		}
	}
}

// A session's own words run to several lines, and a reader wants all of
// them; a user turn can be a whole task prompt, and a reader wants to know
// it happened. So assistant text is kept whole and indented under its
// marker, and a user turn is cut short with a count of what was left.
func TestALongBlockIsIndentedUnderItsMarkerAndAUserTurnIsCutShort(t *testing.T) {
	say := renderTranscriptLine([]byte(
		`{"type":"assistant","message":{"content":[{"type":"text","text":"first\nsecond\nthird"}]}}`))
	if want := []string{"● first", "  second", "  third"}; strings.Join(say, "|") != strings.Join(want, "|") {
		t.Errorf("assistant text rendered as %q, want %q", say, want)
	}

	turn := renderTranscriptLine([]byte(
		`{"type":"user","message":{"content":[{"type":"text","text":"a\nb\nc\nd\ne\nf"}]}}`))
	if len(turn) != 4 || !strings.HasPrefix(turn[0], userMark+"a") {
		t.Fatalf("a user turn rendered as %q", turn)
	}
	if !strings.Contains(turn[3], "(+3 lines)") {
		t.Errorf("a user turn does not say how much of it was left behind: %q", turn)
	}
}

// A session that failed says so where a session that succeeded says "ok",
// and the subtype names what went wrong when claude gave one.
func TestAFailedSessionsResultLineSaysSo(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":0.011}`,
			"● session ended: ok, 1 turn, $0.01"},
		{`{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":40,"total_cost_usd":3}`,
			"● session ended: error_max_turns, 40 turns, $3.00"},
		{`{"type":"result","subtype":"error","is_error":true,"num_turns":2,"total_cost_usd":0.2}`,
			"● session ended: failed, 2 turns, $0.20"},
	} {
		if got := renderTranscriptLine([]byte(tc.line)); len(got) != 1 || got[0] != tc.want {
			t.Errorf("result line rendered as %q, want [%q]", got, tc.want)
		}
	}
}
