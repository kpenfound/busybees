package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/workspace"
)

// Two forty-character revisions, so the check's display abbreviation is
// exercised the way a real commit exercises it.
const (
	runningRev = "1111111111111111111111111111111111111111"
	headRev    = "2222222222222222222222222222222222222222"
)

// writeStatus writes a status.json by hand. state.SaveStatus stamps the
// current time and the test binary's own pid over whatever it is given,
// which is exactly the two fields these cases vary.
func writeStatus(t *testing.T, f *fixture, fields string) {
	t.Helper()
	dir := f.Config.StateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(fields), 0o644); err != nil {
		t.Fatal(err)
	}
}

func statusJSON(pid int, version, revision string) string {
	return fmt.Sprintf(`{"updated_at":"2026-08-31T10:00:00Z","pid":%d,"version":%q,"revision":%q}`, pid, version, revision)
}

// gitAnswers stands in for the repository the check asks about. Each field is
// one git command's answer; for `cat-file -e` and `merge-base --is-ancestor`
// git prints nothing and answers with its exit status, so those two are
// booleans that decide whether Deps.Git returns an error.
type gitAnswers struct {
	head       string
	headErr    error
	knowsRev   bool
	isAncestor bool
	count      string
	countErr   error
}

// install replaces Deps.Git and asserts the argv of every command the check
// runs, so a check that asks git a different question fails here rather than
// quietly reading a fake answer meant for another one.
func (g gitAnswers) install(t *testing.T, f *fixture) {
	t.Helper()
	f.Git = func(_ context.Context, dir string, args ...string) (string, error) {
		if dir != f.Config.Dir() {
			t.Errorf("git run in %s, want the clone %s", dir, f.Config.Dir())
		}
		switch strings.Join(args, " ") {
		case "rev-parse HEAD":
			return g.head, g.headErr
		case "cat-file -e " + runningRev + "^{commit}":
			if !g.knowsRev {
				return "", errors.New("fatal: Not a valid object name")
			}
			return "", nil
		case "merge-base --is-ancestor " + runningRev + " HEAD":
			if !g.isAncestor {
				return "", errors.New("exit status 1")
			}
			return "", nil
		case "rev-list --count " + runningRev + "..HEAD":
			return g.count, g.countErr
		}
		t.Errorf("unexpected git call: git %s", strings.Join(args, " "))
		return "", errors.New("unexpected git call")
	}
}

// TestCheckSchedulerBuild walks every row of #297's verdict table. The check
// warns and never fails: a scheduler behind HEAD is a real factory doing real
// work, and `bees doctor` exits non-zero on a failure.
func TestCheckSchedulerBuild(t *testing.T) {
	cases := []struct {
		name   string
		status string // "" writes no status.json at all
		alive  bool
		git    gitAnswers
		want   Status
		detail []string
	}{
		{name: "no status.json at all", alive: true, want: Pass, detail: []string{"has not run"}},
		{
			name:   "a status.json the scheduler never stamped",
			status: `{"pid":4242,"version":"dev","revision":"` + runningRev + `"}`,
			alive:  true,
			want:   Pass, detail: []string{"has not run"},
		},
		{
			name:   "the recorded pid is not running",
			status: statusJSON(4242, "dev", runningRev),
			alive:  false,
			want:   Pass, detail: []string{"no scheduler is running"},
		},
		{
			name:   "a release build, which records no revision",
			status: statusJSON(4242, "v0.2.0", ""),
			alive:  true,
			want:   Pass, detail: []string{"v0.2.0", "no revision"},
		},
		{
			name:   "a status.json written before bees recorded the build",
			status: statusJSON(4242, "", ""),
			alive:  true,
			want:   Pass, detail: []string{"recorded no build"},
		},
		{
			name:   "a revision this repository has never heard of",
			status: statusJSON(4242, "dev", runningRev),
			alive:  true,
			git:    gitAnswers{head: headRev},
			want:   Warn, detail: []string{"111111111111", "not a commit in this repository", "rebuild and restart `bees run`"},
		},
		{
			name:   "an ancestor of HEAD, and HEAD is ahead",
			status: statusJSON(4242, "dev", runningRev),
			alive:  true,
			git:    gitAnswers{head: headRev, knowsRev: true, isAncestor: true, count: "3\n"},
			want:   Warn, detail: []string{"111111111111", "3 commits behind HEAD", "rebuild and restart `bees run`"},
		},
		{
			name:   "the commit HEAD is on",
			status: statusJSON(4242, "dev", runningRev),
			alive:  true,
			git:    gitAnswers{head: runningRev},
			want:   Pass, detail: []string{"111111111111", "HEAD"},
		},
		{
			name:   "a commit that is not an ancestor of HEAD",
			status: statusJSON(4242, "dev", runningRev),
			alive:  true,
			git:    gitAnswers{head: headRev, knowsRev: true},
			want:   Warn, detail: []string{"111111111111", "not an ancestor of HEAD", "rebuild and restart `bees run`"},
		},
		{
			name:   "git cannot say what HEAD is",
			status: statusJSON(4242, "dev", runningRev),
			alive:  true,
			git:    gitAnswers{headErr: errors.New("fatal: not a git repository")},
			want:   Warn, detail: []string{"111111111111", "not a git repository"},
		},
		{
			name:   "a status.json that cannot be read",
			status: `{"pid": not json`,
			alive:  true,
			want:   Warn, detail: []string{"cannot read the scheduler status", "status.json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, "", nil)
			if tc.status != "" {
				writeStatus(t, f, tc.status)
			}
			f.Alive = func(int) bool { return tc.alive }
			tc.git.install(t, f)
			r := f.run(t, f.checkSchedulerBuild)
			wantResult(t, r, tc.want, tc.detail...)
			if r.Group != GroupConfig {
				t.Errorf("group %q, want %q", r.Group, GroupConfig)
			}
			// A revision is abbreviated for display and compared whole:
			// a detail naming all forty characters is the tell that the
			// two were confused.
			for _, whole := range []string{runningRev, headRev} {
				if strings.Contains(r.Detail, whole) {
					t.Errorf("the detail names the whole revision, which should be abbreviated: %q", r.Detail)
				}
			}
			// The exit code of `bees doctor` is doctor.Failures over the
			// results, so "never fails" and "never changes the exit code"
			// are the same assertion.
			if n := Failures([]Result{r}); n != 0 {
				t.Errorf("the check reported %d failures: %+v", n, r)
			}
		})
	}
}

// TestCheckSchedulerBuildNeverFails covers the answers the table above does
// not reach: whatever git says, and however it says it, the check is a
// warning at worst, so `bees doctor`'s exit code is unchanged by it.
func TestCheckSchedulerBuildNeverFails(t *testing.T) {
	gits := map[string]func(context.Context, string, ...string) (string, error){
		"every command errors": func(context.Context, string, ...string) (string, error) {
			return "", errors.New("git: command not found")
		},
		"every command answers nothing": func(context.Context, string, ...string) (string, error) {
			return "", nil
		},
		"every command answers nonsense": func(context.Context, string, ...string) (string, error) {
			return "not a revision at all", nil
		},
	}
	for name, git := range gits {
		t.Run(name, func(t *testing.T) {
			f := setup(t, "", nil)
			writeStatus(t, f, statusJSON(4242, "dev", runningRev))
			f.Alive = func(int) bool { return true }
			f.Git = git
			r := f.run(t, f.checkSchedulerBuild)
			if r.Status == Fail {
				t.Errorf("status %s, want pass or warn: %+v", r.Status, r)
			}
			if n := Failures([]Result{r}); n != 0 {
				t.Errorf("the check reported %d failures: %+v", n, r)
			}
		})
	}
}

// TestCheckSchedulerBuildAgainstARealRepository asks real git the three
// questions the check asks. `git merge-base --is-ancestor` and `git cat-file
// -e` print nothing and answer with their exit status, so a fake that agreed
// with the wrong argv would leave that handling unproved.
func TestCheckSchedulerBuildAgainstARealRepository(t *testing.T) {
	f := setup(t, "", nil)
	f.Alive = func(int) bool { return true }
	ctx := context.Background()
	git := func(args ...string) string {
		t.Helper()
		out, err := workspace.Git(ctx, f.clone, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	commit := func(msg string) string {
		t.Helper()
		git("commit", "-q", "--allow-empty", "-m", msg)
		return git("rev-parse", "HEAD")
	}

	first := git("rev-parse", "HEAD")
	writeStatus(t, f, statusJSON(4242, "dev", first))
	wantResult(t, f.run(t, f.checkSchedulerBuild), Pass, first[:12], "HEAD")

	second := commit("second")
	commit("third")
	writeStatus(t, f, statusJSON(4242, "dev", second))
	behind := f.run(t, f.checkSchedulerBuild)
	wantResult(t, behind, Warn, second[:12], "1 commit behind HEAD", "rebuild and restart `bees run`")
	if strings.Contains(behind.Detail, second) {
		t.Errorf("the detail names the whole revision, which should be abbreviated: %q", behind.Detail)
	}

	// A commit on another branch: known to the repository, not an ancestor.
	git("checkout", "-q", "-b", "side")
	side := commit("side")
	git("checkout", "-q", "main")
	writeStatus(t, f, statusJSON(4242, "dev", side))
	wantResult(t, f.run(t, f.checkSchedulerBuild), Warn, side[:12], "not an ancestor of HEAD")

	// A well-formed revision this repository has never seen.
	writeStatus(t, f, statusJSON(4242, "dev", runningRev))
	wantResult(t, f.run(t, f.checkSchedulerBuild), Warn, "111111111111", "not a commit in this repository")
}
