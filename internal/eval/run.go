package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/fakegh"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/skills"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/workspace"
)

// DefaultBranch is the fixture's default branch.
const DefaultBranch = "main"

// DefaultPassInterval is the pause between two scheduler passes, and the
// eval factory's poll_interval.
const DefaultPassInterval = "10s"

// Why a case's run stopped.
const (
	// StopDone: every seeded issue is closed or held for a person.
	StopDone = "done"
	// StopTimeout: the case's timeout ran out.
	StopTimeout = "timeout"
	// StopBudget: the sessions cost the case's max_cost.
	StopBudget = "budget"
	// StopInterrupted: the eval itself was stopped.
	StopInterrupted = "interrupted"
	// StopInvalid: the test passed on the fixture, so the run would prove
	// nothing; no session ran.
	StopInvalid = "invalid"
	// StopError: the case could not be set up or the factory failed.
	StopError = "error"
)

// Runner runs eval cases, one after another.
type Runner struct {
	// Bees is the bees executable: sessions run it as bees (mail, done,
	// the MCP server), and their gh runs its ShimCommand.
	Bees string
	// ClaudeBin, CodexBin and OpenCodeBin are the agent executables, as
	// for `bees run`.
	ClaudeBin, CodexBin, OpenCodeBin string
	// Skills prepares the skills a role names. Optional.
	Skills *skills.Manager
	// Console receives each case's session summaries, warnings and
	// errors; nil discards them. The whole log is the case's bees.log.
	Console io.Writer
	// PassInterval is the pause between two scheduler passes, a Go
	// duration; empty is DefaultPassInterval.
	PassInterval string
}

// Run runs every case with sel's profiles, each in a directory of its own
// under dir, and writes the report into dir. An eval that cannot even
// start a case records why in that case's result and goes on to the next.
func (r *Runner) Run(ctx context.Context, cases []Case, sel Selection, dir string) (*Report, error) {
	if _, err := time.ParseDuration(r.passInterval()); err != nil {
		return nil, fmt.Errorf("pass interval: %w", err)
	}
	rep := &Report{Started: time.Now().UTC(), Dir: dir, Profile: sel}
	for _, c := range cases {
		if ctx.Err() != nil {
			break
		}
		rep.Cases = append(rep.Cases, r.runCase(ctx, c, sel, filepath.Join(dir, c.Name)))
	}
	if err := rep.Write(); err != nil {
		return rep, err
	}
	return rep, nil
}

func (r *Runner) passInterval() string {
	return firstNonEmpty(r.PassInterval, DefaultPassInterval)
}

func (r *Runner) runCase(ctx context.Context, c Case, sel Selection, dir string) (res CaseResult) {
	start := time.Now()
	res = CaseResult{Case: c.Name, Profile: sel.String(), Dir: dir, Stop: StopError}
	defer func() {
		res.DurationSeconds = time.Since(start).Round(time.Millisecond).Seconds()
		res.Pass = res.Error == "" && res.Stop != StopInvalid && len(res.Checks) > 0
		for _, check := range res.Checks {
			res.Pass = res.Pass && check.Pass
		}
	}()
	fail := func(err error) CaseResult {
		res.Error = err.Error()
		return res
	}
	fx, err := buildFixture(ctx, c, dir)
	if err != nil {
		return fail(fmt.Errorf("fixture: %w", err))
	}
	passed, log, err := runTest(ctx, c, fx.origin, filepath.Join(dir, "before"))
	if err != nil {
		return fail(fmt.Errorf("test on the fixture: %w", err))
	}
	res.Checks = append(res.Checks, Check{
		Name:    "the test fails on the fixture",
		Failure: "the test passed on the fixture, where it has to fail: the case does not describe work to do",
		Pass:    !passed,
		Detail:  log,
	})
	if passed {
		res.Stop = StopInvalid
		return res
	}
	f, err := r.factory(ctx, c, sel, dir, fx)
	if err != nil {
		return fail(err)
	}
	defer f.close()
	res.Stop = f.loop(ctx, c)
	if res.Stop == StopError {
		res.Error = f.err.Error()
	}
	res.Checks = append(res.Checks, f.grade(ctx, c, filepath.Join(dir, "after"))...)
	res.CostUSD, res.CostUnknown, res.Turns, res.Sessions = f.spend()
	return res
}

// fixture is a case's repository: the bare origin the factory pushes to,
// the clone it works in, and the clone the runner merges in.
type fixture struct {
	origin, project, merger string
}

// gitIdentity is the author of the fixture's commit and of every merge.
var gitIdentity = []string{"-c", "user.name=bees eval", "-c", "user.email=eval@bees.invalid", "-c", "commit.gpgsign=false"}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return workspace.Git(ctx, dir, args...)
}

// buildFixture creates the case's origin with one commit on DefaultBranch,
// from its repo/ directory or what its setup.sh builds, and a clone of it.
func buildFixture(ctx context.Context, c Case, dir string) (fixture, error) {
	fx := fixture{origin: filepath.Join(dir, "origin.git"), project: filepath.Join(dir, "project"), merger: filepath.Join(dir, "merger")}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fx, err
	}
	steps := [][]string{
		{dir, "init", "-q", "--bare", "--initial-branch=" + DefaultBranch, fx.origin},
		{dir, "clone", "-q", fx.origin, fx.project},
		{fx.project, "checkout", "-q", "-B", DefaultBranch},
	}
	for _, s := range steps {
		if _, err := git(ctx, s[0], s[1:]...); err != nil {
			return fx, err
		}
	}
	if _, err := os.Stat(filepath.Join(c.Dir, RepoDir)); err == nil {
		if err := copyTree(filepath.Join(c.Dir, RepoDir), fx.project); err != nil {
			return fx, err
		}
	} else {
		if err := runLogged(ctx, fx.project, filepath.Join(dir, "setup.log"), "sh", filepath.Join(c.Dir, SetupScript)); err != nil {
			return fx, fmt.Errorf("%s: %w (see %s)", SetupScript, err, filepath.Join(dir, "setup.log"))
		}
	}
	// Like `bees init`: the configuration and the state stay out of git.
	if err := os.WriteFile(filepath.Join(fx.project, ".git", "info", "exclude"), []byte("/bees.toml\n/.bees/\n"), 0o644); err != nil {
		return fx, err
	}
	for _, args := range [][]string{
		{"add", "-A"},
		append(append([]string{}, gitIdentity...), "commit", "-q", "--allow-empty", "-m", "Fixture for eval case "+c.Name),
		{"push", "-q", "-u", "origin", DefaultBranch},
	} {
		if _, err := git(ctx, fx.project, args...); err != nil {
			return fx, err
		}
	}
	return fx, nil
}

// copyTree copies the regular files, directories and links under src into
// dst, keeping their modes.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, info.Mode().Perm())
		}
		return nil
	})
}

// TestTimeout bounds one run of a case's test command.
const TestTimeout = 10 * time.Minute

// runTest runs the case's test in a fresh checkout of origin's default
// branch at dest, with the case's grade/ files copied over it, and reports
// whether it passed. The output is kept beside dest; err is for a test
// that could not be run at all.
func runTest(ctx context.Context, c Case, origin, dest string) (passed bool, log string, err error) {
	if _, err := git(ctx, filepath.Dir(dest), "clone", "-q", "--branch", DefaultBranch, origin, dest); err != nil {
		return false, "", err
	}
	if _, err := os.Stat(filepath.Join(c.Dir, GradeDir)); err == nil {
		if err := copyTree(filepath.Join(c.Dir, GradeDir), dest); err != nil {
			return false, "", err
		}
	}
	log = dest + ".log"
	ctx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()
	err = runLogged(ctx, dest, log, "sh", "-c", c.Test)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, log, nil
	case errors.As(err, &exit) && ctx.Err() == nil:
		return false, log, nil
	}
	return false, log, err
}

// caseFactory is one case's factory: its fake GitHub and the scheduler
// working against it.
type caseFactory struct {
	cfg    *config.Config
	labels config.Labels
	gh     *fakegh.GitHub
	sched  *scheduler.Scheduler
	store  *state.Store
	fx     fixture
	srv    *server
	logger *logging.Logger
	log    *slog.Logger
	// interval is the pause between two passes.
	interval time.Duration
	// err is why the loop stopped with StopError.
	err error
}

// factory builds the case's factory: its bees.toml in the fixture's clone,
// the fake GitHub seeded with the case's issues, the mailbox with its mail,
// the gh every session reaches that GitHub through, and a scheduler.
func (r *Runner) factory(ctx context.Context, c Case, sel Selection, dir string, fx fixture) (*caseFactory, error) {
	repo := "bees-eval/" + c.Name
	stateDir := filepath.Join(dir, "state")
	text := sel.configText(settings{Repo: repo, StateDir: stateDir, Workspaces: filepath.Join(dir, "worktrees"), PassInterval: r.passInterval()})
	path := filepath.Join(fx.project, "bees.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	f := &caseFactory{cfg: cfg, labels: cfg.Labels(), fx: fx, interval: cfg.Scheduler.PollInterval.Duration}
	f.store = state.New(cfg.StateDir())
	if err := f.store.Init(); err != nil {
		return nil, err
	}
	console := r.Console
	if console == nil {
		console = io.Discard
	}
	f.logger = logging.New(logging.Options{Format: logging.FormatText, Quiet: true, Console: console})
	if err := f.logger.AttachFile(filepath.Join(f.store.Dir, "bees.log")); err != nil {
		return nil, err
	}
	f.log = f.logger.With("case", c.Name)

	f.gh = fakegh.New(repo)
	f.gh.Login = "bees-eval"
	f.gh.DiffFor = func(p github.PR) (string, error) {
		return git(context.Background(), fx.origin, "diff", p.BaseRefName+"..."+p.HeadRefName)
	}
	seed := fakegh.Seed{}
	for _, l := range f.labels.All() {
		seed.Labels = append(seed.Labels, l.Name)
	}
	now := time.Now()
	for _, i := range c.Issues {
		issue := github.Issue{Number: i.Number, Title: i.Title, Body: i.Body, Author: github.Author{Login: i.Author}, CreatedAt: now,
			Labels: []github.Label{{Name: cfg.Filter.Label}}}
		for _, l := range i.Labels {
			if !github.HasLabel(issue.Labels, l) {
				issue.Labels = append(issue.Labels, github.Label{Name: l})
			}
		}
		for _, cm := range i.Comments {
			issue.Comments = append(issue.Comments, github.Comment{Author: github.Author{Login: cm.Author}, Body: cm.Body, CreatedAt: now})
		}
		seed.Issues = append(seed.Issues, fakegh.SeedIssue{Issue: issue})
	}
	if err := f.gh.Load(seed); err != nil {
		f.close()
		return nil, err
	}
	box := mail.Open(f.store.MailDir(), f.store.Migrate)
	for _, m := range c.Mail {
		to, _ := config.CanonicalRole(m.To)
		if _, err := box.Send(mail.Message{From: m.From, To: to, Subject: m.Subject, Body: m.Body, Work: ghwork.New(m.Issue, 0)}); err != nil {
			f.close()
			return nil, err
		}
	}

	if f.srv, err = serve(f.gh); err != nil {
		f.close()
		return nil, err
	}
	bin := filepath.Join(dir, "bin")
	if err := writeShim(bin, r.Bees, f.srv.URL); err != nil {
		f.close()
		return nil, err
	}
	client := github.New(repo)
	client.Exec, client.ExecStdin = f.gh.Exec, f.gh.ExecStdin
	runner := &session.Runner{
		ClaudeBin:   r.ClaudeBin,
		CodexBin:    r.CodexBin,
		OpenCodeBin: r.OpenCodeBin,
		// The link in bin: the runner puts its directory first on every
		// session's PATH, which is how the shim's gh comes before any
		// other.
		BeesBin:     filepath.Join(bin, "bees"),
		SessionsDir: f.store.SessionsDir(),
		StateDir:    f.store.Dir,
		ConfigPath:  cfg.Path,
		Repo:        repo,
		Label:       cfg.Filter.Label,
		Notes:       cfg.Notes,
		Skills:      r.Skills,
		AddDirs:     []string{f.store.Dir},
		Logger:      f.log,
	}
	ws := workspace.NewManager(fx.project, cfg.Scheduler.WorkspaceRoot)
	ws.Remote = cfg.Project.Remote
	f.sched, err = scheduler.New(scheduler.Deps{Config: cfg, GitHub: client, Mail: box, Runner: runner, Workspaces: ws, Store: f.store, Logger: f.log})
	if err != nil {
		f.close()
		return nil, err
	}
	f.sched.Once = true
	return f, nil
}

func (f *caseFactory) close() {
	if f.srv != nil {
		_ = f.srv.Close()
	}
	if f.logger != nil {
		_ = f.logger.Close()
	}
}

// loop runs scheduler passes until the case stops, merging what the
// reviewer approved between them, and returns why it stopped. Each pass
// waits for the work it started, a developer's issue up to its approval.
// The timeout and the budget are also watched while a pass runs, and stop
// the sessions running when they end.
func (f *caseFactory) loop(ctx context.Context, c Case) string {
	runCtx, cancel := context.WithTimeout(ctx, c.Timeout.Duration)
	defer cancel()
	var mu sync.Mutex
	stop := ""
	setStop := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		if stop == "" {
			stop = s
		}
	}
	stopped := func() string {
		mu.Lock()
		defer mu.Unlock()
		return stop
	}
	ended := func() string {
		if ctx.Err() != nil {
			return StopInterrupted
		}
		return StopTimeout
	}
	watched := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-watched:
				return
			case <-runCtx.Done():
				setStop(ended())
				f.sched.HardStop()
				return
			case <-tick.C:
				if f.overBudget(c) {
					setStop(StopBudget)
					cancel()
				}
			}
		}
	}()
	defer func() {
		close(watched)
		wg.Wait()
	}()
	for {
		if err := f.sched.Run(runCtx); err != nil {
			f.err = err
			setStop(StopError)
		}
		if f.overBudget(c) {
			setStop(StopBudget)
		}
		if runCtx.Err() != nil {
			setStop(ended())
		}
		if s := stopped(); s != "" {
			return s
		}
		if err := f.mergeApproved(runCtx); err != nil {
			f.log.Warn("could not merge an approved pull request", "err", err)
		}
		if f.settled(c) {
			return StopDone
		}
		select {
		case <-runCtx.Done():
		case <-time.After(f.interval):
		}
	}
}

// spend sums the case's sessions from the ledger.
func (f *caseFactory) spend() (cost float64, unknown, turns, sessions int) {
	entries, err := f.store.ReadLedger(time.Time{})
	if err != nil {
		f.log.Warn("could not read the ledger", "err", err)
	}
	for _, e := range entries {
		sessions++
		turns += e.Turns
		cost += e.CostUSD
		if e.CostUnknown {
			unknown++
		}
	}
	return cost, unknown, turns, sessions
}

func (f *caseFactory) overBudget(c Case) bool {
	if c.MaxCost <= 0 {
		return false
	}
	cost, _, _, _ := f.spend()
	return cost >= c.MaxCost
}

// settled reports whether every seeded issue is closed or held for a
// person: there is nothing left for the factory to do with them.
func (f *caseFactory) settled(c Case) bool {
	snap := f.gh.Snapshot()
	for _, seeded := range c.Issues {
		i, ok := snap.Issue(seeded.Number)
		if ok && i.State == "OPEN" && !github.HasLabel(i.Labels, f.labels.NeedsHuman) {
			return false
		}
	}
	return true
}

// closingKeywords are the words that close an issue when the pull request
// whose body names it merges, as GitHub reads them.
var closingKeywords = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\s+#(\d+)\b`)

// closes lists the issues body closes on merge.
func closes(body string) []int {
	var out []int
	for _, m := range closingKeywords.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// mergeApproved merges every open pull request the reviewer approved, as
// the person the fake GitHub does not have would: into its base branch on
// the origin, closing the issues its body names with a closing keyword. A
// pull request whose head does not merge cleanly is left open and marked
// the way GitHub marks it: CONFLICTING, with the head it was tried at as its
// HeadSHA. That is what the scheduler's conflict check (checkPRs) needs to
// mail the developer once per head and send the approved issue back, so the
// next pass fixes the branch and the merge after the next approval tries
// the new head.
func (f *caseFactory) mergeApproved(ctx context.Context) error {
	var errs []error
	for _, p := range f.gh.Snapshot().PRs {
		if p.State != "OPEN" || !github.HasLabel(p.Labels, f.labels.Approved) {
			continue
		}
		head, err := f.merge(ctx, p)
		now := time.Now()
		f.gh.Lock()
		live := f.gh.PRs[p.Number]
		if err != nil {
			if head != "" {
				live.Mergeable, live.HeadSHA = github.MergeableConflicting, head
			}
			errs = append(errs, fmt.Errorf("pull request #%d: %w", p.Number, err))
		} else {
			live.State, live.MergedAt, live.Mergeable = "MERGED", &now, ""
			f.gh.Merged = append(f.gh.Merged, p.Number)
			for _, n := range closes(p.Body) {
				if i, ok := f.gh.Issues[n]; ok && i.State == "OPEN" {
					i.State, i.ClosedAt = "CLOSED", &now
				}
			}
		}
		f.gh.Unlock()
		if err == nil {
			f.log.Info("merged an approved pull request", "pr", p.Number, "closes", closes(p.Body))
		}
	}
	return errors.Join(errs...)
}

// merge merges p's head into its base on the origin, in the runner's own
// clone. conflict is the head commit when that merge is what failed: the
// head does not merge cleanly into the base. It is empty when the merge
// succeeded or something else failed.
func (f *caseFactory) merge(ctx context.Context, p github.PR) (conflict string, err error) {
	m := f.fx.merger
	if _, err := os.Stat(m); err != nil {
		if _, err := git(ctx, filepath.Dir(m), "clone", "-q", f.fx.origin, m); err != nil {
			return "", err
		}
	}
	if _, err := git(ctx, m, "fetch", "-q", "origin"); err != nil {
		return "", err
	}
	if _, err := git(ctx, m, "checkout", "-q", "-B", p.BaseRefName, "origin/"+p.BaseRefName); err != nil {
		return "", err
	}
	head, err := git(ctx, m, "rev-parse", "origin/"+p.HeadRefName)
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", p.Number, p.HeadRefName, p.Title)
	if _, err := git(ctx, m, append(append([]string{}, gitIdentity...), "merge", "-q", "--no-ff", "-m", msg, "origin/"+p.HeadRefName)...); err != nil {
		_, _ = git(ctx, m, "merge", "--abort")
		return strings.TrimSpace(head), err
	}
	_, err = git(ctx, m, "push", "-q", "origin", p.BaseRefName)
	return "", err
}

// grade checks the end state: each seeded issue closed, with a pull request
// of its own, and the case's test passing on the default branch.
func (f *caseFactory) grade(ctx context.Context, c Case, dest string) []Check {
	snap := f.gh.Snapshot()
	var checks []Check
	for _, seeded := range c.Issues {
		i, _ := snap.Issue(seeded.Number)
		closed := Check{
			Name:    fmt.Sprintf("#%d closed", seeded.Number),
			Failure: fmt.Sprintf("#%d was not closed", seeded.Number),
			Pass:    i.State == "CLOSED",
		}
		if !closed.Pass && github.HasLabel(i.Labels, f.labels.NeedsHuman) {
			closed.Detail = "held for a person: " + f.labels.NeedsHuman
		}
		checks = append(checks, closed)
		pr := Check{
			Name:    fmt.Sprintf("#%d has a pull request", seeded.Number),
			Failure: fmt.Sprintf("#%d got no pull request", seeded.Number),
		}
		branch := f.sched.BranchFor(seeded.Number)
		for _, p := range snap.PRs {
			if p.HeadRefName == branch || slices.Contains(closes(p.Body), seeded.Number) {
				pr.Pass, pr.Detail = true, fmt.Sprintf("#%d", p.Number)
				break
			}
		}
		checks = append(checks, pr)
	}
	passed, log, err := runTest(ctx, c, f.fx.origin, dest)
	test := Check{
		Name:    "the test passes",
		Failure: "the test still fails on the default branch",
		Pass:    passed,
		Detail:  log,
	}
	if err != nil {
		test.Detail = err.Error()
	}
	return append(checks, test)
}

// runLogged runs a command in dir with its output in the file log.
func runLogged(ctx context.Context, dir, log, name string, args ...string) error {
	out, err := os.Create(log)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, out, out
	return cmd.Run()
}
