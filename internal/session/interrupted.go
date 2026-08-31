package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kpenfound/busybees/internal/procs"
)

// ResultFile is the file Run writes when a session ends, whatever it ended
// with. Its absence is what says a session never finished.
const ResultFile = "result.json"

// TranscriptFile is the session's stream-json output, written line by line
// while the session runs. An interrupted session leaves an unfinished one.
const TranscriptFile = "transcript.jsonl"

// InterruptedFile marks a session directory whose session was stopped on
// purpose — `bees kill` and the live view's kill key write it — so the next
// session can be told it was stopped rather than left to guess that the
// machine crashed. It holds one
// line of prose, which is what MarkInterrupted was given.
const InterruptedFile = "interrupted"

// Interrupted describes a session that started and never finished: no
// result file was written and no process is running for it any more.
type Interrupted struct {
	// Role is who ran the session, and Name the session directory's name.
	Role string
	Name string
	Dir  string
	// Transcript is the path of the unfinished transcript, empty when the
	// session was killed before it wrote one.
	Transcript string
	// Turns is how many assistant messages the transcript holds. It is an
	// approximation of the turn count a finished session reports: that
	// number comes from claude's final result event, which an interrupted
	// session never emitted.
	Turns int
	// Killed is true when the session was stopped on purpose (`bees kill`)
	// rather than lost with the process that ran it, and Note is what the
	// marker said.
	Killed bool
	Note   string
}

// MarkInterrupted records in a session directory that the session was
// stopped on purpose. A directory that is already gone is not an error:
// marking is bookkeeping for the next session, never a reason to fail a
// kill.
func MarkInterrupted(sessionDir, note string) error {
	if sessionDir == "" {
		return nil
	}
	if _, err := os.Stat(sessionDir); err != nil {
		return nil
	}
	return os.WriteFile(filepath.Join(sessionDir, InterruptedFile), []byte(note+"\n"), 0o644)
}

// CheckInterrupted reports what a session directory says about the session
// that ran in it. role names who ran it — the directory cannot say — and
// alive answers whether a pid is still running (nil means procs.Alive; it
// is a seam so tests need no real process). It returns:
//
//   - nil, true while the session is still running: its pid file names a
//     live process. A session that has not written its result file yet is
//     the ordinary state of a running session and must never be reported as
//     interrupted;
//   - nil, false when the session finished — the result file exists — or
//     when the directory is gone;
//   - an Interrupted, false when it started and never finished.
func CheckInterrupted(role, dir string, alive func(int) bool) (*Interrupted, bool) {
	if dir == "" {
		return nil, false
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, false
	}
	if _, err := os.Stat(filepath.Join(dir, ResultFile)); err == nil {
		return nil, false
	}
	if alive == nil {
		alive = procs.Alive
	}
	if b, err := os.ReadFile(filepath.Join(dir, procs.PIDFile)); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && alive(pid) {
			return nil, true
		}
	}
	in := &Interrupted{Role: role, Name: filepath.Base(dir), Dir: dir}
	if b, err := os.ReadFile(filepath.Join(dir, InterruptedFile)); err == nil {
		in.Killed, in.Note = true, strings.TrimSpace(string(b))
	}
	transcript := filepath.Join(dir, TranscriptFile)
	if _, err := os.Stat(transcript); err == nil {
		in.Transcript, in.Turns = transcript, countTurns(transcript)
	}
	return in, false
}

// countTurns counts the assistant messages of an unfinished transcript.
// A finished session takes its turn count from the result event of claude's
// stream, and an interrupted one never emitted that event, so the messages
// are counted instead: close enough to say how far the session had got, and
// not the same number claude would have reported.
func countTurns(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	// The same limits consume reads the live stream with: a single assistant
	// message can be very large, and a line the scanner refuses to read would
	// silently end the count.
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	n := 0
	for sc.Scan() {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(sc.Bytes(), &probe); err != nil {
			continue
		}
		if probe.Type == "assistant" {
			n++
		}
	}
	return n
}
