package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/procs"
)

// sessionDir builds a session directory holding the named files, and returns
// its path. A file's content is the value; an empty value writes an empty
// file.
func sessionDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "20260831-081500-developer-issue-4-r1-ab")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// transcript is what claude's stream-json output looks like on disk: a line
// per event, of which the assistant messages are the turns.
const transcript = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"role":"assistant"}}
{"type":"user","message":{"role":"user"}}
{"type":"assistant","message":{"role":"assistant"}}
not json at all
{"type":"assistant","message":{"role":"assistant"}}
`

// TestCheckInterruptedTellsARunningSessionFromAnInterruptedOne is the whole
// point of the pid check. A session that has not written its result file yet
// is the ordinary state of a session that is still running, so the absence of
// the file cannot be the signal on its own: the pid file decides. alive is a
// seam precisely so this test needs no real process to kill (#250).
func TestCheckInterruptedTellsARunningSessionFromAnInterruptedOne(t *testing.T) {
	dead := func(int) bool { return false }
	live := func(int) bool { return true }

	cases := []struct {
		name    string
		files   map[string]string
		alive   func(int) bool
		want    *Interrupted // nil: nothing to report
		running bool
	}{{
		name:  "killed mid-session: a pid file whose process is gone",
		files: map[string]string{procs.PIDFile: "4242\n", TranscriptFile: transcript},
		alive: dead,
		want:  &Interrupted{Turns: 3},
	}, {
		name:  "still running: the pid is alive, the result is simply not written yet",
		files: map[string]string{procs.PIDFile: "4242\n", TranscriptFile: transcript},
		alive: live,
		want:  nil, running: true,
	}, {
		name:  "finished: the result file is there, whatever the pid file says",
		files: map[string]string{procs.PIDFile: "4242\n", TranscriptFile: transcript, ResultFile: "{}"},
		alive: live,
		want:  nil,
	}, {
		name: "stopped by bees kill: the pid file is gone and the marker is there",
		files: map[string]string{TranscriptFile: transcript,
			InterruptedFile: "stopped by bees kill\n"},
		alive: dead,
		want:  &Interrupted{Turns: 3, Killed: true, Note: "stopped by bees kill"},
	}, {
		name:  "no pid file and no result: the pid file was cleaned up, the session never finished",
		files: map[string]string{TranscriptFile: transcript},
		alive: live, // nothing to ask about: there is no pid to check
		want:  &Interrupted{Turns: 3},
	}, {
		name:  "killed before it wrote anything",
		files: map[string]string{procs.PIDFile: "4242\n"},
		alive: dead,
		want:  &Interrupted{},
	}, {
		name:  "an unreadable pid file is no evidence that anything is running",
		files: map[string]string{procs.PIDFile: "not a pid", TranscriptFile: transcript},
		alive: live,
		want:  &Interrupted{Turns: 3},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := sessionDir(t, tc.files)
			got, running := CheckInterrupted("developer", dir, tc.alive)
			if running != tc.running {
				t.Fatalf("running = %v, want %v", running, tc.running)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("reported %+v, want nothing", got)
				}
				return
			}
			if got == nil {
				t.Fatal("nothing reported, want an interrupted session")
			}
			if got.Role != "developer" || got.Name != filepath.Base(dir) || got.Dir != dir {
				t.Errorf("session identity: %+v", got)
			}
			if got.Turns != tc.want.Turns {
				t.Errorf("turns = %d, want %d", got.Turns, tc.want.Turns)
			}
			if got.Killed != tc.want.Killed || got.Note != tc.want.Note {
				t.Errorf("killed = %v %q, want %v %q", got.Killed, got.Note, tc.want.Killed, tc.want.Note)
			}
			wantTranscript := ""
			if tc.files[TranscriptFile] != "" {
				wantTranscript = filepath.Join(dir, TranscriptFile)
			}
			if got.Transcript != wantTranscript {
				t.Errorf("transcript = %q, want %q", got.Transcript, wantTranscript)
			}
		})
	}
}

// A session directory that is gone — deleted to reclaim space, or never
// created because the scheduler died before it started the session — says
// nothing about anything.
func TestCheckInterruptedIgnoresAMissingDirectory(t *testing.T) {
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "not-there")} {
		if got, running := CheckInterrupted("developer", dir, func(int) bool { return false }); got != nil || running {
			t.Errorf("%q reported %+v (running %v)", dir, got, running)
		}
	}
}

// The marker `bees kill` leaves is what tells a session that was stopped on
// purpose from one lost with the machine. Writing it into a directory that
// has since been removed must not fail a kill.
func TestMarkInterrupted(t *testing.T) {
	dir := sessionDir(t, map[string]string{procs.PIDFile: "4242\n"})
	if err := MarkInterrupted(dir, "stopped by bees kill"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, InterruptedFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "stopped by bees kill" {
		t.Fatalf("marker holds %q", b)
	}
	in, _ := CheckInterrupted("developer", dir, func(int) bool { return false })
	if in == nil || !in.Killed {
		t.Fatalf("the marker was not read back: %+v", in)
	}
	if err := MarkInterrupted(filepath.Join(t.TempDir(), "gone"), "x"); err != nil {
		t.Fatalf("marking a directory that is gone: %v", err)
	}
	if err := MarkInterrupted("", "x"); err != nil {
		t.Fatalf("marking no directory at all: %v", err)
	}
}
