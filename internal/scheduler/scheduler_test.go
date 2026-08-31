package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/workspace"
)

// TestMain lets the test binary double as a fake `claude` when
// FAKE_CLAUDE is set: the runner executes it, it inspects its role and
// environment, performs a scripted action and prints a stream-json result.
//
// The flags that steer the fake (FAKE_CLAUDE, FAKE_DEV_HANG, FAKE_DEV_FAIL,
// FAKE_DEV_MAIL_TO, FAKE_REVIEW_ALWAYS_CHANGES, FAKE_COST)
// reach it through the ordinary environment, so they must NOT start with
// BEES_: the runner strips inherited BEES_* variables from every session.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_CLAUDE") == "1" {
		fakeClaude()
		os.Exit(0)
	}
	// The runner drops inherited BEES_* variables, but the tests run this
	// binary directly too (and read the environment themselves), so clear the
	// ones a bees session would have exported: `go test` run from inside a
	// session must behave like one run from a plain shell.
	for _, k := range []string{session.EnvRole, session.EnvSessionDir, session.EnvStateDir, session.EnvRepo,
		session.EnvLabel, session.EnvIssue, session.EnvPR, session.EnvBranch, session.EnvNotesFile,
		session.EnvConfig, session.EnvBin} {
		if err := os.Unsetenv(k); err != nil {
			fmt.Fprintln(os.Stderr, "unset:", err)
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}

const fakePR = 101

func fakeClaude() {
	role := os.Getenv(session.EnvRole)
	sessionDir := os.Getenv(session.EnvSessionDir)
	stateDir := os.Getenv(session.EnvStateDir)
	box := mail.Open(filepath.Join(stateDir, "mail"))
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "fake claude:", err)
		os.Exit(2)
	}
	// Record the command line so tests can assert on the flags the runner
	// built, the way internal/session's fake does.
	if err := os.WriteFile(filepath.Join(sessionDir, "args.txt"), []byte(strings.Join(os.Args, "\n")), 0o644); err != nil {
		fail(err)
	}
	git := func(args ...string) {
		if _, err := workspace.Git(context.Background(), ".", args...); err != nil {
			fail(err)
		}
	}
	// Record the order sessions ran in, so tests can assert the sequence
	// across roles (a session directory name carries a one-second timestamp,
	// so sorting the directories is not chronological).
	if f, err := os.OpenFile(filepath.Join(stateDir, "fake-order"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		_, _ = fmt.Fprintln(f, filepath.Base(sessionDir))
		_ = f.Close()
	}
	// FAKE_LIMIT makes a session hit the account-wide claude session limit,
	// whatever its role: it emits a blocking rate_limit_event. The value is
	// the resetsAt unix timestamp, or "none" for an event that carried
	// none. The session then dies without reporting an outcome, the way one
	// that could not start does — unless FAKE_LIMIT_WITH_OUTCOME is set, in
	// which case the role does its work and reports it, which is a session
	// that finished just as the account ran out of capacity.
	if v := os.Getenv("FAKE_LIMIT"); v != "" {
		resets := ""
		if v != "none" {
			resets = `,"resetsAt":` + v
		}
		fmt.Printf(`{"type":"rate_limit_event","rate_limit_info":{"status":"blocked","rateLimitType":"five_hour","overageStatus":"allowed"%s}}`+"\n", resets)
		if os.Getenv("FAKE_LIMIT_WITH_OUTCOME") == "" {
			fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"You've hit your session limit","session_id":"fake","num_turns":1,"total_cost_usd":0.01}`)
			return
		}
	}
	counter := func(name string) int {
		p := filepath.Join(stateDir, "fake-"+name)
		n := 0
		if b, err := os.ReadFile(p); err == nil {
			n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		n++
		_ = os.WriteFile(p, []byte(strconv.Itoa(n)), 0o644)
		return n
	}
	var outcome session.Outcome
	switch role {
	case config.RoleDeveloper:
		n := counter("dev")
		// Infrastructure failures, on the first attempt only: hang until the
		// role timeout kills the session, or report `failed` outright.
		if n == 1 && os.Getenv("FAKE_DEV_HANG") == "1" {
			time.Sleep(time.Minute)
		}
		if os.Getenv("FAKE_DEV_FAIL") == "1" {
			outcome = session.Outcome{Status: OutcomeFailed, Note: "cannot build"}
			break
		}
		// FAKE_DEV_MAIL_TO makes the session write to another role before it
		// finishes, the way a real one does with `bees mail send`: a
		// different process appending to the same mailbox on disk.
		if to := os.Getenv("FAKE_DEV_MAIL_TO"); to != "" {
			if _, err := box.Send(mail.Message{From: role, To: to, Subject: "a question about the queue", Body: "please look at this"}); err != nil {
				fail(err)
			}
		}
		if err := os.WriteFile(fmt.Sprintf("work-%d.txt", n), []byte("done"), 0o644); err != nil {
			fail(err)
		}
		git("add", ".")
		git("-c", "user.email=bee@example.com", "-c", "user.name=bee", "commit", "-q", "-m", fmt.Sprintf("work %d", n))
		git("push", "-q")
		if os.Getenv(session.EnvPR) == "" {
			// "Open" the PR: the fake gh treats the markers as the PR existing
			// (fakePR, and any hidden PR on this issue's branch).
			for _, marker := range []string{"fake-pr-created", "fake-pr-created-issue-" + os.Getenv(session.EnvIssue)} {
				if err := os.WriteFile(filepath.Join(stateDir, marker), nil, 0o644); err != nil {
					fail(err)
				}
			}
			outcome = session.Outcome{Status: OutcomePROpened, PR: fakePR}
		} else {
			outcome = session.Outcome{Status: OutcomePRUpdated, PR: fakePR}
		}
	case config.RoleReviewer:
		pr, _ := strconv.Atoi(os.Getenv(session.EnvPR))
		issue, _ := strconv.Atoi(os.Getenv(session.EnvIssue))
		if os.Getenv("BEES_REVIEW_MODE") == "checks" {
			counter("checks")
			prompt, _ := os.ReadFile(filepath.Join(sessionDir, "prompt.md"))
			if _, err := box.Send(mail.Message{From: role, To: config.RoleDeveloper, Subject: "Check failed: go / test", Body: "main error: TestX fails\n\n" + string(prompt), PR: pr, Issue: issue}); err != nil {
				fail(err)
			}
			outcome = session.Outcome{Status: OutcomeChangesRequested}
			break
		}
		// FAKE_REVIEW_ALWAYS_CHANGES never approves, which is the only way
		// to reach the "not approved after N review rounds" escalation.
		if os.Getenv("FAKE_REVIEW_ALWAYS_CHANGES") == "1" {
			round := counter("review")
			if _, err := box.Send(mail.Message{From: role, To: config.RoleDeveloper, Subject: fmt.Sprintf("Review round %d", round), Body: "still not right", PR: pr, Issue: issue}); err != nil {
				fail(err)
			}
			outcome = session.Outcome{Status: OutcomeChangesRequested}
			break
		}
		if counter("review") == 1 {
			if _, err := box.Send(mail.Message{From: role, To: config.RoleDeveloper, Subject: "Review round 1", Body: "please add tests", PR: pr, Issue: issue}); err != nil {
				fail(err)
			}
			outcome = session.Outcome{Status: OutcomeChangesRequested}
		} else {
			outcome = session.Outcome{Status: OutcomeApproved, Note: "lgtm"}
		}
	default:
		counter(role)
		outcome = session.Outcome{Status: OutcomeDone, Note: "ok"}
	}
	if err := session.WriteOutcome(sessionDir, outcome); err != nil {
		fail(err)
	}
	// FAKE_COST makes a session's cost controllable, which is what the cost
	// budget tests spend against.
	cost := 0.01
	if v := os.Getenv("FAKE_COST"); v != "" {
		c, err := strconv.ParseFloat(v, 64)
		if err != nil {
			fail(err)
		}
		cost = c
	}
	// FAKE_RESULT_TEXT is what the session says it did. A bee whose work is
	// the account's own limit writes the words "session limit" here.
	text := "ok"
	if v := os.Getenv("FAKE_RESULT_TEXT"); v != "" {
		text = v
	}
	fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":"fake","num_turns":2,"total_cost_usd":%v}`+"\n", text, cost)
}

// fakeGH is an in-memory GitHub backing the gh wrapper.
type fakeGH struct {
	mu       sync.Mutex
	issues   map[int]*github.Issue
	prs      map[int]*github.PR
	prMarker string
	// hidden lists PRs that do not exist until a developer session opened
	// one on their head branch (bees/issue-N), like fakePR.
	hidden   map[int]bool
	history  map[int][]string // label additions per number, in order
	comments map[int][]string
	merged   []int
	// activity is raw JSON served for api pulls/N/reviews, pulls/N/comments, issues/N/comments
	activity map[string]string
	// checks is a queue of responses for `pr checks --required`; the last one
	// repeats. checksAll is the same for the unrequired call, which the
	// scheduler only makes when the required list came back empty.
	checks    []checksResponse
	checksAll []checksResponse
	mergeArgs [][]string
	// calls logs every gh invocation, in order.
	calls [][]string
	// labels are the label names that exist in the repository.
	labels []string
	// milestones are the open milestones of the repository.
	milestones []github.Milestone
	// parents maps a work item to the feature it is a sub-issue of, for the
	// ParentIssue query. Empty means "use the hardcoded answer below": issue
	// 1 is a sub-issue of feature 5 while that feature exists.
	parents map[int]int
	// errFor makes a command fail: it is keyed by the command name, either
	// the first two arguments ("label list") or the first one ("label"), or
	// by "requested_reviewers" and "assignees" for the review-request and
	// assignee REST calls.
	errFor map[string]error
}

// callCount counts logged gh calls whose first two arguments are cmd
// ("issue list", "pr list", ...).
func (f *fakeGH) callCount(cmd string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) >= 2 && c[0]+" "+c[1] == cmd {
			n++
		}
	}
	return n
}

// total counts every logged gh call, whatever it was: what a test asserting
// that a code path costs no GitHub call measures.
func (f *fakeGH) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeClock is a settable clock for tests that drive the scheduler loop.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type checksResponse struct {
	json string
	err  error
}

func (f *fakeGH) exec(ctx context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) >= 2 {
		if err, ok := f.errFor[args[0]+" "+args[1]]; ok {
			return nil, err
		}
	}
	if err, ok := f.errFor[args[0]]; ok {
		return nil, err
	}
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	flags := func(name string) []string {
		var out []string
		for i, a := range args {
			if a == name && i+1 < len(args) {
				out = append(out, args[i+1])
			}
		}
		return out
	}
	num := func() int {
		n, _ := strconv.Atoi(args[2])
		return n
	}
	prVisible := func(p *github.PR) bool {
		if p.Number == fakePR {
			_, err := os.Stat(f.prMarker)
			return err == nil
		}
		if f.hidden[p.Number] {
			issue := strings.TrimPrefix(p.HeadRefName, "bees/issue-")
			_, err := os.Stat(f.prMarker + "-issue-" + issue)
			return err == nil
		}
		return true
	}
	setAssignee := func(n int, login string) {
		if i, ok := f.issues[n]; ok {
			i.Assignees = append(i.Assignees, github.Author{Login: login})
		} else if p, ok := f.prs[n]; ok {
			p.Assignees = append(p.Assignees, github.Author{Login: login})
		}
		f.history[n] = append(f.history[n], "assignee:"+login)
	}
	if args[0] == "api" {
		// Assignees and milestones go to the REST endpoints: `gh issue edit
		// --add-assignee` fails against GitHub with a Projects (classic)
		// GraphQL error when the number is a pull request.
		if i := slices.IndexFunc(args, func(a string) bool { return strings.HasSuffix(a, "/assignees") }); i >= 0 {
			if err, ok := f.errFor["assignees"]; ok {
				return nil, err
			}
			var n int
			if _, err := fmt.Sscanf(args[i], "repos/acme/widgets/issues/%d/assignees", &n); err != nil {
				return nil, fmt.Errorf("fake gh: bad assignees path %q", args[i])
			}
			for _, v := range flags("-f") {
				if login, ok := strings.CutPrefix(v, "assignees[]="); ok {
					setAssignee(n, login)
				}
			}
			return []byte("{}"), nil
		}
		if flag("--method") == "PATCH" {
			var n int
			if _, err := fmt.Sscanf(args[3], "repos/acme/widgets/issues/%d", &n); err != nil {
				return nil, fmt.Errorf("fake gh: bad issue path %q", args[3])
			}
			for _, v := range flags("-F") {
				number, ok := strings.CutPrefix(v, "milestone=")
				if !ok {
					continue
				}
				k, _ := strconv.Atoi(number)
				title := ""
				for _, m := range f.milestones {
					if m.Number == k {
						title = m.Title
					}
				}
				if title == "" {
					return nil, fmt.Errorf("fake gh: no milestone %s", number)
				}
				if i, ok := f.issues[n]; ok {
					i.Milestone = &github.MilestoneRef{Title: title}
				} else if p, ok := f.prs[n]; ok {
					p.Milestone = &github.MilestoneRef{Title: title}
				}
				f.history[n] = append(f.history[n], "milestone:"+title)
			}
			return []byte("{}"), nil
		}
		// Review requests go to the REST endpoint: `gh pr edit --add-reviewer`
		// fails against GitHub with a Projects (classic) GraphQL error.
		if slices.ContainsFunc(args, func(a string) bool { return strings.HasSuffix(a, "/requested_reviewers") }) {
			if err, ok := f.errFor["requested_reviewers"]; ok {
				return nil, err
			}
			return []byte("{}"), nil
		}
		if args[1] == "graphql" {
			n := 0
			for _, a := range args {
				if v, ok := strings.CutPrefix(a, "number="); ok {
					n, _ = strconv.Atoi(v)
				}
			}
			if p, ok := f.parents[n]; ok {
				title := "Feature"
				if i, ok := f.issues[p]; ok {
					title = i.Title
				}
				return fmt.Appendf(nil, `{"data":{"repository":{"issue":{"parent":{"number":%d,"title":%q}}}}}`, p, title), nil
			}
			// parent lookup: issue 1 has parent 5 when it exists
			if n == 1 {
				if _, ok := f.issues[5]; ok {
					return []byte(`{"data":{"repository":{"issue":{"parent":{"number":5,"title":"Exports"}}}}}`), nil
				}
			}
			return []byte(`{"data":{"repository":{"issue":{"parent":null}}}}`), nil
		}
		path := args[len(args)-1]
		if body, ok := f.activity[path]; ok {
			return []byte("[" + body + "]"), nil // --slurp wraps pages in an array
		}
		// REST issue details: repos/acme/widgets/issues/N
		var n int
		if _, err := fmt.Sscanf(path, "repos/acme/widgets/issues/%d", &n); err == nil && !strings.Contains(path, "/comments") {
			return []byte(fmt.Sprintf(`{"id": %d, "milestone": null, "sub_issues_summary": {"total": 3, "completed": 1, "percent_completed": 33}}`, 1000+n)), nil
		}
		if strings.Contains(path, "/pulls/") || strings.Contains(path, "/issues/") {
			return []byte("[[]]"), nil
		}
	}
	switch args[0] + " " + args[1] {
	case "issue list":
		var out []github.Issue
		label, state := flag("--label"), flag("--state")
		for _, i := range f.issues {
			if (state == "all" || i.State == "OPEN") && (label == "" || github.HasLabel(i.Labels, label)) {
				out = append(out, *i)
			}
		}
		sort.Slice(out, func(a, b int) bool { return out[a].Number < out[b].Number })
		return json.Marshal(out)
	case "issue view":
		i, ok := f.issues[num()]
		if !ok {
			return nil, fmt.Errorf("no issue %d", num())
		}
		return json.Marshal(i)
	case "issue edit":
		n := num()
		var labels *[]github.Label
		if i, ok := f.issues[n]; ok {
			labels = &i.Labels
		} else if p, ok := f.prs[n]; ok {
			labels = &p.Labels
		} else {
			return nil, fmt.Errorf("no item %d", n)
		}
		for _, l := range flags("--remove-label") {
			var kept []github.Label
			for _, have := range *labels {
				if have.Name != l {
					kept = append(kept, have)
				}
			}
			*labels = kept
		}
		for _, l := range flags("--add-label") {
			if !github.HasLabel(*labels, l) {
				*labels = append(*labels, github.Label{Name: l})
			}
			f.history[n] = append(f.history[n], l)
		}
		if a := flag("--add-assignee"); a != "" {
			// The factory must never build this: it fails against GitHub.
			return nil, fmt.Errorf("fake gh: issue edit --add-assignee is deprecated by GitHub, use the REST endpoint")
		}
		return nil, nil
	case "issue comment":
		f.comments[num()] = append(f.comments[num()], flag("--body"))
		return nil, nil
	case "pr list":
		var out []github.PR
		head, state := flag("--head"), flag("--state")
		for _, p := range f.prs {
			if !prVisible(p) {
				continue
			}
			if head != "" && p.HeadRefName != head {
				continue
			}
			if state == "open" && p.State != "OPEN" {
				continue
			}
			if state == "all" && p.State != "OPEN" && p.MergedAt == nil {
				continue
			}
			if state == "merged" && p.MergedAt == nil {
				continue
			}
			out = append(out, *p)
		}
		return json.Marshal(out)
	case "pr view":
		p, ok := f.prs[num()]
		if !ok || !prVisible(p) {
			return nil, fmt.Errorf("no pr %d", num())
		}
		return json.Marshal(p)
	case "pr merge":
		f.merged = append(f.merged, num())
		f.mergeArgs = append(f.mergeArgs, args)
		return nil, nil
	case "pr checks":
		queue := &f.checks
		if !slices.Contains(args, "--required") {
			queue = &f.checksAll
		}
		if len(*queue) == 0 {
			return nil, fmt.Errorf("no checks reported on the 'bees/issue-1' branch")
		}
		r := (*queue)[0]
		if len(*queue) > 1 {
			*queue = (*queue)[1:]
		}
		return []byte(r.json), r.err
	case "label list":
		out := make([]github.Label, 0, len(f.labels))
		for _, l := range f.labels {
			out = append(out, github.Label{Name: l})
		}
		return json.Marshal(out)
	case "label create":
		if !slices.Contains(f.labels, args[2]) {
			f.labels = append(f.labels, args[2])
		}
		return nil, nil
	case "api repos/acme/widgets/milestones?state=open&per_page=100":
		return json.Marshal(f.milestones)
	}
	return nil, fmt.Errorf("fake gh: unsupported %v", args)
}

type harness struct {
	t     *testing.T
	cfg   *config.Config
	gh    *fakeGH
	store *state.Store
	box   *mail.Box
	sched *Scheduler
	clone string
	logs  *syncBuffer
	clock *fakeClock // nil unless the harness was built with a fixed clock
}

// syncBuffer collects log output; workers log concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHarness(t *testing.T, toml string) *harness {
	t.Helper()
	return newHarnessAt(t, toml, time.Time{})
}

// newHarnessAt builds a harness whose scheduler reads the clock from
// harness.clock, starting at now. A zero now uses the real clock.
func newHarnessAt(t *testing.T, toml string, now time.Time) *harness {
	t.Helper()
	_, clone := testutil.SetupRepos(t)
	cfgPath := filepath.Join(clone, "bees.toml")
	if err := os.WriteFile(cfgPath, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	// Like `bees init`: keep the config and state dir out of git.
	if err := os.WriteFile(filepath.Join(clone, ".git", "info", "exclude"), []byte("/bees.toml\n/.bees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-review checks read runs before every first review, so without
	// this every test would sit through the 1m default wait. Tests that care
	// about the timings set their own and keep them.
	if rs := cfg.Roles[config.RoleReviewer]; rs.ChecksWait.Duration == 0 || rs.ChecksPollInterval.Duration == 0 {
		if rs.ChecksWait.Duration == 0 {
			rs.ChecksWait = config.Duration{Duration: time.Millisecond}
		}
		if rs.ChecksPollInterval.Duration == 0 {
			rs.ChecksPollInterval = config.Duration{Duration: 10 * time.Millisecond}
		}
		cfg.Roles[config.RoleReviewer] = rs
	}
	// default_branch is derived from the (local) origin remote's HEAD.
	if err := cfg.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.Project.DefaultBranch != "main" {
		t.Fatalf("default branch not derived: %q", cfg.Project.DefaultBranch)
	}
	store := state.New(cfg.StateDir())
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGH{
		issues:   map[int]*github.Issue{},
		prs:      map[int]*github.PR{},
		prMarker: filepath.Join(store.Dir, "fake-pr-created"),
		hidden:   map[int]bool{},
		history:  map[int][]string{},
		comments: map[int][]string{},
		activity: map[string]string{},
		errFor:   map[string]error{},
	}
	// Like a repository `bees init` has just set up: every label exists.
	for _, l := range cfg.Labels().All() {
		gh.labels = append(gh.labels, l.Name)
	}
	client := github.New(cfg.Project.Repo)
	client.Exec = gh.exec

	t.Setenv("FAKE_CLAUDE", "1")
	// Log through the real logging package so tests see the summary lines a
	// terminal would; dump it when the test fails.
	logs := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("scheduler log:\n" + logs.String())
		}
	})
	logger := logging.New(logging.Options{Format: logging.FormatText, Console: logs})
	runner := &session.Runner{
		ClaudeBin:   os.Args[0],
		SessionsDir: store.SessionsDir(),
		StateDir:    store.Dir,
		Repo:        cfg.Project.Repo,
		Label:       cfg.Filter.Label,
		Logger:      logger.Logger,
	}
	ws := workspace.NewManager(clone, filepath.Join(t.TempDir(), "ws"))
	box := mail.Open(store.MailDir())
	deps := Deps{Config: cfg, GitHub: client, Mail: box, Runner: runner, Workspaces: ws, Store: store, Logger: runner.Logger}
	var clock *fakeClock
	if !now.IsZero() {
		clock = &fakeClock{t: now}
		deps.Now = clock.now
	}
	sched, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	sched.Once = true
	return &harness{t: t, cfg: cfg, gh: gh, store: store, box: box, sched: sched, clone: clone, logs: logs, clock: clock}
}

func (h *harness) sessions(role string) []string {
	entries, _ := os.ReadDir(h.store.SessionsDir())
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "-"+role+"-") {
			out = append(out, filepath.Join(h.store.SessionsDir(), e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// sessionOrder lists the session directories in the order the sessions ran.
func (h *harness) sessionOrder() []string {
	b, err := os.ReadFile(filepath.Join(h.store.Dir, "fake-order"))
	if err != nil {
		return nil
	}
	return strings.Fields(strings.TrimSpace(string(b)))
}

// sessionNames is sessionOrder with the "<date>-<time>-" prefix and the random
// MkdirTemp suffix stripped, so "reviewer-pr-101-r2" is the whole name and not
// a prefix of "reviewer-pr-101-r2-checkfix1".
func (h *harness) sessionNames() []string {
	dirs := h.sessionOrder()
	names := make([]string, len(dirs))
	for i, d := range dirs {
		parts := strings.Split(d, "-")
		if len(parts) > 3 {
			parts = parts[2 : len(parts)-1]
		}
		names[i] = strings.Join(parts, "-")
	}
	return names
}

// wantOrder asserts the sessions ran in this order, by exact session name.
func (h *harness) wantOrder(want ...string) {
	h.t.Helper()
	got := h.sessionNames()
	if len(got) != len(want) {
		h.t.Fatalf("session order: %v\nwant     %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			h.t.Fatalf("session %d is %q, want %q\nfull order: %v", i, got[i], want[i], got)
		}
	}
}

// sessionFlag reads the value the runner passed for a flag in the newest
// session of a role, from the args.txt the fake claude wrote.
func (h *harness) sessionFlag(t *testing.T, role, flag string) string {
	t.Helper()
	dirs := h.sessions(role)
	if len(dirs) == 0 {
		t.Fatalf("no %s session ran", role)
	}
	b, err := os.ReadFile(filepath.Join(dirs[len(dirs)-1], "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(b), "\n")
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("%s session has no %s in %v", role, flag, args)
	return ""
}

const baseTOML = `
version = 1
[project]
repo = "acme/widgets"
[scheduler]
poll_interval = "1s"
max_developers = 2
max_review_rounds = 3
`

func TestFullDeveloperReviewLoop(t *testing.T) {
	h := newHarness(t, baseTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "Human filed this", Body: "hi", State: "OPEN", Labels: []github.Label{{Name: "bees"}}, CreatedAt: time.Now()}
	h.gh.issues[3] = &github.Issue{Number: 3, Title: "Spec me", Body: "vague", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// Issue 1 walked the whole loop: in-progress -> review -> in-progress -> review -> approved.
	want := []string{"bees:in-progress", "bees:review", "bees:in-progress", "bees:review", "bees:approved"}
	if got := h.gh.history[1]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("issue 1 label history: %v", got)
	}
	if !github.HasLabel(h.gh.prs[fakePR].Labels, "bees:approved") {
		t.Fatalf("PR labels: %v", h.gh.prs[fakePR].Labels)
	}
	if len(h.gh.merged) != 0 {
		t.Fatal("auto_merge is off; nothing should be merged")
	}
	if len(h.gh.comments[1]) != 0 {
		t.Fatalf("no escalation expected: %v", h.gh.comments[1])
	}
	// Issue 2 had no state label and neither bees:feature nor bees:feedback:
	// it is an idea a person handed the factory, so it goes to the product
	// manager as feedback.
	if got := h.gh.history[2]; strings.Join(got, ",") != "bees:feedback" {
		t.Fatalf("issue 2 label history: %v", got)
	}
	// Sessions: 2 developer, 2 reviewer, and each singleton once.
	for role, n := range map[string]int{config.RoleDeveloper: 2, config.RoleReviewer: 2, config.RoleProjectManager: 1, config.RoleProductManager: 1, config.RoleQA: 1} {
		if got := len(h.sessions(role)); got != n {
			t.Errorf("%s sessions: got %d want %d", role, got, n)
		}
	}
	// Report every role's count above, then stop: the assertions below index
	// these session slices, so a missing session would panic instead of failing.
	if t.Failed() {
		t.FailNow()
	}
	// The second developer session received the reviewer's mail.
	dev := h.sessions(config.RoleDeveloper)
	prompt, _ := os.ReadFile(filepath.Join(dev[1], "prompt.md"))
	if !strings.Contains(string(prompt), "please add tests") || !strings.Contains(string(prompt), "review round 2 of 3") {
		t.Fatalf("second developer prompt:\n%s", prompt)
	}
	// The console got one summary line per session, in the order they ended.
	wantSummaries := []string{
		"✓ developer issue #1 → PR #101 opened",
		"✗ reviewer PR #101 changes requested",
		"✓ developer issue #1 → PR #101 updated",
		"✓ reviewer PR #101 approved",
	}
	rest := h.logs.String()
	for _, line := range wantSummaries {
		_, after, found := strings.Cut(rest, line)
		if !found {
			t.Fatalf("missing summary %q (or out of order) in:\n%s", line, h.logs.String())
		}
		rest = after
	}

	// The project manager saw issue 3 in its triage list, and issue 2 — the
	// idea reconcile relabelled — reached the product manager instead.
	pjm := h.sessions(config.RoleProjectManager)
	prompt, _ = os.ReadFile(filepath.Join(pjm[0], "prompt.md"))
	if !strings.Contains(string(prompt), "#3: Spec me") {
		t.Fatalf("project manager prompt:\n%s", prompt)
	}
	pdm := h.sessions(config.RoleProductManager)
	prompt, _ = os.ReadFile(filepath.Join(pdm[0], "prompt.md"))
	if !strings.Contains(string(prompt), "#2: Human filed this") {
		t.Fatalf("product manager prompt:\n%s", prompt)
	}
	// Mail was delivered (marked read) and bookkeeping recorded round 2.
	unread, _ := h.box.List(mail.Filter{UnreadOnly: true})
	if len(unread) != 0 {
		t.Fatalf("unread mail left: %+v", unread)
	}
	if is, _ := h.store.Issue(1); is.Round != 2 || is.PR != fakePR {
		t.Fatalf("bookkeeping: %+v", is)
	}
	// Both commits reached origin on the developer branch and worktrees are gone.
	log, err := workspace.Git(ctx, h.clone, "log", "--oneline", "origin/bees/issue-1")
	if err != nil || strings.Count(log, "\n") != 2 {
		t.Fatalf("origin branch log: %q %v", log, err)
	}
	if out, _ := workspace.Git(ctx, h.clone, "worktree", "list"); strings.Count(out, "\n") != 0 {
		t.Fatalf("worktrees left behind:\n%s", out)
	}
	// Status file reflects an idle factory.
	st, _ := h.store.LoadStatus()
	if len(st.Workers) != 0 || st.Singletons[config.RoleQA] != "idle" {
		t.Fatalf("status: %+v", st)
	}
	for _, r := range []string{config.RoleProductManager, config.RoleProjectManager, config.RoleQA} {
		if rs, _ := h.store.Role(r); rs.LastRun.IsZero() {
			t.Errorf("%s last run not recorded", r)
		}
	}
	// Every session is in the ledger, with what it cost and what it did.
	ledger, err := h.store.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 7 {
		t.Fatalf("ledger has %d entries, want one per session (7):\n%+v", len(ledger), ledger)
	}
	byRole := map[string][]state.LedgerEntry{}
	for _, e := range ledger {
		if e.Turns != 2 || e.CostUSD != 0.01 || e.Session == "" || e.Time.IsZero() {
			t.Errorf("ledger entry not filled in: %+v", e)
		}
		// DurationMS is whole milliseconds, so a session that spawned and exited
		// in under 1ms records 0. That is a legitimate duration, not a missing
		// field: never assert it is positive.
		if e.DurationMS < 0 {
			t.Errorf("ledger entry has a negative duration: %+v", e)
		}
		byRole[e.Role] = append(byRole[e.Role], e)
	}
	for role, n := range map[string]int{config.RoleDeveloper: 2, config.RoleReviewer: 2, config.RoleProjectManager: 1, config.RoleProductManager: 1, config.RoleQA: 1} {
		if got := len(byRole[role]); got != n {
			t.Errorf("%s ledger entries: got %d want %d", role, got, n)
		}
	}
	// Report every role's count above, then stop: the assertions below index
	// these ledger buckets, so a missing entry would panic instead of failing.
	if t.Failed() {
		t.FailNow()
	}
	for i, want := range []string{OutcomePROpened, OutcomePRUpdated} {
		got := byRole[config.RoleDeveloper][i]
		if got.Outcome != want || got.Issue != 1 || got.PR != fakePR {
			t.Errorf("developer ledger entry %d: %+v", i, got)
		}
	}
	for i, want := range []string{OutcomeChangesRequested, OutcomeApproved} {
		got := byRole[config.RoleReviewer][i]
		if got.Outcome != want || got.PR != fakePR {
			t.Errorf("reviewer ledger entry %d: %+v", i, got)
		}
	}
	if e := byRole[config.RoleQA][0]; e.Issue != 0 || e.PR != 0 {
		t.Errorf("qa ledger entry should have no issue or PR: %+v", e)
	}
}

func TestQuestionBlocksAndAnswerUnblocks(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n")
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Vague", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:blocked"}}}
	// The developer asked earlier; now the project manager's answer is waiting.
	if _, err := h.box.Send(mail.Message{From: config.RoleProjectManager, To: config.RoleDeveloper, Subject: "Re: Vague", Body: "do X", Issue: 1}); err != nil {
		t.Fatal(err)
	}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true} // reviewer disabled: PR auto-approved
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// The size backstop runs after the unblock, so the issue is sized in the
	// same pass that made it ready.
	want := "bees:ready,bees:size/m,bees:in-progress,bees:approved"
	if got := strings.Join(h.gh.history[1], ","); got != want {
		t.Fatalf("history: %s want %s", got, want)
	}
	dev := h.sessions(config.RoleDeveloper)
	prompt, _ := os.ReadFile(filepath.Join(dev[0], "prompt.md"))
	if !strings.Contains(string(prompt), "do X") {
		t.Fatalf("answer not delivered:\n%s", prompt)
	}
}

func TestEscalationWhenNoPR(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "x", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}}
	// No PR 101 registered: the developer claims pr-opened but nothing exists.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:needs-human" {
		t.Fatalf("history: %s", got)
	}
	if len(h.gh.comments[1]) != 1 || !strings.Contains(h.gh.comments[1][0], "needs a human") {
		t.Fatalf("comments: %v", h.gh.comments[1])
	}
}

func TestHumanFeedbackReopensApprovedPR(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	created := time.Now().Add(-time.Hour)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Done already", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:approved"}, {Name: "bees:size/s"}}, CreatedAt: created}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:approved"}}, CreatedAt: created, UpdatedAt: time.Now(),
		Body: "Closes #1"}
	// The PR "exists" from the start for this test.
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Seed the branch on origin so the worker can reuse it.
	if err := os.WriteFile(filepath.Join(h.clone, "seed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"checkout", "-q", "-b", "bees/issue-1"}, {"add", "."}, {"commit", "-q", "-m", "seed"}, {"push", "-q", "-u", "origin", "bees/issue-1"}, {"checkout", "-q", "main"}} {
		if _, err := workspace.Git(context.Background(), h.clone, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	h.gh.activity["repos/acme/widgets/pulls/101/comments"] = fmt.Sprintf(`[
		{"id": 555, "user": {"login": "kyle"}, "body": "please rename this", "path": "seed.txt", "line": 1, "html_url": "https://x/555", "created_at": %q},
		{"id": 556, "user": {"login": "kyle"}, "body": "will do\n\n<!-- bees:developer -->", "path": "seed.txt", "line": 1, "html_url": "https://x/556", "created_at": %q},
		{"id": 557, "user": {"login": "kyle"}, "body": "Replying to the bot:\n> <!-- bees:developer -->\n\nActually, hold off on merging.", "path": "seed.txt", "line": 1, "html_url": "https://x/557", "created_at": %q}
	]`, now, now, now)
	h.gh.activity["repos/acme/widgets/pulls/101/reviews"] = fmt.Sprintf(`[
		{"id": 777, "user": {"login": "kyle"}, "body": "", "state": "APPROVED", "html_url": "https://x/777", "submitted_at": %q}
	]`, now)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// approved -> ready (human feedback) -> in-progress -> review -> ... -> approved
	hist := strings.Join(h.gh.history[1], ",")
	if !strings.HasPrefix(hist, "bees:ready,bees:in-progress,bees:review") || !strings.HasSuffix(hist, "bees:approved") {
		t.Fatalf("history: %s", hist)
	}
	dev := h.sessions(config.RoleDeveloper)
	if len(dev) == 0 {
		t.Fatal("no developer session ran")
	}
	prompt, _ := os.ReadFile(filepath.Join(dev[0], "prompt.md"))
	for _, want := range []string{"Feedback on PR #101 from kyle", "please rename this", "pulls/101/comments/555/replies", "seed.txt:1", "Actually, hold off on merging."} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("developer prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{"will do", "APPROVED"} {
		if strings.Contains(string(prompt), unwanted) {
			t.Errorf("developer prompt should not contain %q (bee comment / empty approval)", unwanted)
		}
	}
	if bk, _ := h.store.Issue(1); bk.HumanSeenAt.IsZero() {
		t.Fatal("HumanSeenAt not recorded")
	}
}

func TestLabelBackstop(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[filter]\nassignee = \"kyle\"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n")
	// A triage issue so the project manager runs; and an issue "created by a
	// bee" with a kind label but no base label and no assignee.
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "triage me", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, Assignees: []github.Author{{Login: "kyle"}}, CreatedAt: time.Now().Add(-time.Hour)}
	h.gh.issues[7] = &github.Issue{Number: 7, Title: "bug from a bee", State: "OPEN", Labels: []github.Label{{Name: "bees:bug"}, {Name: "bees:triage"}}, CreatedAt: time.Now()}
	h.gh.issues[8] = &github.Issue{Number: 8, Title: "unrelated", State: "OPEN", Labels: nil, CreatedAt: time.Now()}
	// A pull request opened outside a developer worker, with a factory label
	// but neither the base label nor the assignee: unassigned it would be
	// invisible to a factory filtering on one.
	h.gh.prs[9] = &github.PR{Number: 9, Title: "pr from a bee", State: "OPEN", HeadRefName: "bees/issue-7", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees:review"}}, CreatedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.history[7], ","); got != "bees,assignee:kyle" {
		t.Fatalf("issue 7 history: %s", got)
	}
	if len(h.gh.history[8]) != 0 {
		t.Fatalf("unrelated issue touched: %v", h.gh.history[8])
	}
	if got := strings.Join(h.gh.history[9], ","); got != "bees,assignee:kyle" {
		t.Fatalf("PR 9 history: %s", got)
	}
	// The project manager is told the size it must not exceed when it sizes
	// a work item, so it splits anything bigger instead.
	dirs := h.sessions(config.RoleProjectManager)
	if len(dirs) == 0 {
		t.Fatal("no project manager session")
	}
	prompt, _ := os.ReadFile(filepath.Join(dirs[0], "system-prompt.md"))
	if !strings.Contains(string(prompt), "anything larger than `l` is not dispatched") {
		t.Fatalf("project manager system prompt does not carry max_size:\n%s", prompt)
	}
}

func TestAutoMergeAfterChecks(t *testing.T) {
	h := newHarness(t, baseTOML+`
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
[roles.reviewer]
auto_merge = true
merge_method = "rebase"
checks_wait = "1ms"
checks_poll_interval = "10ms"
checks_timeout = "5s"
max_check_fix_rounds = 2
# This test pins the post-approval gate; prereview_test.go owns the other one.
pre_review_checks = false
`)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Ship it", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	pending := `[{"name":"go / test","bucket":"pending","state":"PENDING","link":"https://ci.example.com/run/1","workflow":"CI"}]`
	failing := `[{"name":"go / test","bucket":"fail","state":"FAILURE","link":"https://ci.example.com/run/1","description":"1 test failed","workflow":"CI"},{"name":"lint","bucket":"pass","state":"SUCCESS"}]`
	passing := `[{"name":"go / test","bucket":"pass","state":"SUCCESS"},{"name":"lint","bucket":"pass","state":"SUCCESS"}]`
	h.gh.checks = []checksResponse{
		{pending, fmt.Errorf("exit status 8")}, // still running: gh exits 8
		{failing, fmt.Errorf("exit status 1")}, // failed: gh exits 1
		{pending, fmt.Errorf("exit status 8")}, // after the fix push
		{passing, nil},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.gh.merged) != 1 || h.gh.merged[0] != fakePR {
		t.Fatalf("merged: %v", h.gh.merged)
	}
	if got := strings.Join(h.gh.mergeArgs[0], " "); !strings.Contains(got, "--rebase") || !strings.Contains(got, "--delete-branch") {
		t.Fatalf("merge args: %s", got)
	}
	want := "bees:in-progress,bees:review,bees:in-progress,bees:review,bees:approved,bees:in-progress"
	if got := strings.Join(h.gh.history[1], ","); got != want {
		t.Fatalf("history: %s\nwant    %s", got, want)
	}
	if len(h.gh.comments[1]) != 0 {
		t.Fatalf("unexpected escalation: %v", h.gh.comments[1])
	}
	// developer: initial, review fix, checks fix = 3; reviewer: 2 reviews + 1 checks diagnosis
	if n := len(h.sessions(config.RoleDeveloper)); n != 3 {
		t.Fatalf("developer sessions: %d", n)
	}
	rev := h.sessions(config.RoleReviewer)
	if len(rev) != 3 {
		t.Fatalf("reviewer sessions: %d", len(rev))
	}
	var checksPrompt string
	for _, dir := range rev {
		if strings.Contains(dir, "checks") {
			b, _ := os.ReadFile(filepath.Join(dir, "prompt.md"))
			checksPrompt = string(b)
		}
	}
	for _, want := range []string{"checks failed on pull request #101", "**go / test** (CI) — fail: 1 test failed", "https://ci.example.com/run/1", "do not assume GitHub"} {
		if !strings.Contains(checksPrompt, want) {
			t.Errorf("checks prompt missing %q", want)
		}
	}
	if strings.Contains(checksPrompt, "lint") {
		t.Error("passing checks should not be listed as failing")
	}
	dev := h.sessions(config.RoleDeveloper)
	fix, _ := os.ReadFile(filepath.Join(dev[2], "prompt.md"))
	if !strings.Contains(string(fix), "main error: TestX fails") {
		t.Fatalf("developer fix prompt missing the diagnosis:\n%s", fix)
	}
	if bk, _ := h.store.Issue(1); bk.CheckFixRounds != 1 {
		t.Fatalf("bookkeeping: %+v", bk)
	}
}

func TestChecksTimeoutEscalates(t *testing.T) {
	h := newHarness(t, baseTOML+`
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
[roles.reviewer]
auto_merge = true
checks_wait = "1ms"
checks_poll_interval = "10ms"
checks_timeout = "1ms"
pre_review_checks = false
`)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Slow CI", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	h.gh.checks = []checksResponse{{`[{"name":"slow","bucket":"pending","state":"PENDING"}]`, fmt.Errorf("exit status 8")}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.gh.merged) != 0 {
		t.Fatal("must not merge with pending checks")
	}
	if got := h.gh.history[1]; got[len(got)-1] != "bees:needs-human" {
		t.Fatalf("history: %v", got)
	}
	if len(h.gh.comments[1]) != 1 || !strings.Contains(h.gh.comments[1][0], "still pending") {
		t.Fatalf("comments: %v", h.gh.comments[1])
	}
}

func TestFeedbackGoesToProductManager(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	now := time.Now()
	h.gh.issues[3] = &github.Issue{Number: 3, Title: "Dark mode please", Body: "idea", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feedback"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	h.gh.issues[4] = &github.Issue{Number: 4, Title: "Already answered", Body: "old idea", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feedback"}}, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now,
		Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "filed #10 for this\n\n<!-- bees:product_manager -->", CreatedAt: now.Add(-time.Hour)}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Feedback issues never enter the workflow state machine.
	if len(h.gh.history[3]) != 0 || len(h.gh.history[4]) != 0 {
		t.Fatalf("feedback issues were relabelled: %v %v", h.gh.history[3], h.gh.history[4])
	}
	pm := h.sessions(config.RoleProductManager)
	if len(pm) != 1 {
		t.Fatalf("product manager sessions: %d", len(pm))
	}
	prompt, _ := os.ReadFile(filepath.Join(pm[0], "prompt.md"))
	if !strings.Contains(string(prompt), "#3: Dark mode please") {
		t.Fatalf("fresh feedback missing from prompt:\n%s", prompt)
	}
	if strings.Contains(string(prompt), "#4: Already answered") {
		t.Fatal("feedback already answered by the product manager should not be re-presented")
	}
	// After the run, nothing is fresh until a human comments again.
	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.sched.productManagerHasWork(ctx, snap) {
		t.Fatal("product manager should be idle: no fresh feedback, interval not elapsed")
	}
	h.gh.issues[3].Comments = []github.Comment{
		{Author: github.Author{Login: "kyle"}, Body: "created #11\n\n<!-- bees:product_manager -->", CreatedAt: now.Add(time.Second)},
		{Author: github.Author{Login: "kyle"}, Body: "also on mobile please", CreatedAt: now.Add(2 * time.Second)},
	}
	h.gh.issues[3].UpdatedAt = now.Add(2 * time.Second)
	snap, _ = h.sched.poll(ctx)
	if !h.sched.productManagerHasWork(ctx, snap) {
		t.Fatal("a new human comment on feedback should wake the product manager")
	}
}

func TestFeatureIssuesBelongToProductManager(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	now := time.Now()
	// A feature filed by a person, never touched by the PM: fresh.
	h.gh.issues[5] = &github.Issue{Number: 5, Title: "Exports", Body: "csv please", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feature"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	// A feature where the PM asked a question and the person just answered.
	h.gh.issues[6] = &github.Issue{Number: 6, Title: "Search", Body: "find things", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feature"}, {Name: "bees:question"}}, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now,
		Comments: []github.Comment{
			{Author: github.Author{Login: "kyle"}, Body: "Fuzzy or exact?\n\n<!-- bees:product_manager -->", CreatedAt: now.Add(-2 * time.Hour)},
			{Author: github.Author{Login: "kyle"}, Body: "fuzzy", CreatedAt: now.Add(-time.Minute)},
		}}
	// A feature already broken down: the PM commented last.
	h.gh.issues[7] = &github.Issue{Number: 7, Title: "Done planning", Body: "x", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feature"}}, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now,
		Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "work items: #8 #9\n\n<!-- bees:product_manager -->", CreatedAt: now.Add(-time.Hour)}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{5, 6, 7} {
		for _, l := range h.gh.history[n] {
			if strings.HasPrefix(l, "bees:") && l != "bees:question" {
				t.Fatalf("feature issue #%d was put in the state machine: %v", n, h.gh.history[n])
			}
		}
	}
	if github.HasLabel(h.gh.issues[6].Labels, "bees:question") {
		t.Fatal("answered question should have lost the question label")
	}
	pm := h.sessions(config.RoleProductManager)
	if len(pm) != 1 {
		t.Fatalf("product manager sessions: %d", len(pm))
	}
	prompt, _ := os.ReadFile(filepath.Join(pm[0], "prompt.md"))
	for _, want := range []string{"#5: Exports", "#6: Search", "fuzzy", "| 7 | - | 1/3 done | - | - | Done planning |"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(string(prompt), "#7: Done planning") {
		t.Error("already broken-down feature should only appear in the summary table")
	}
	if strings.Contains(string(prompt), "| 5 | - |") && strings.Contains(string(prompt), "## Open work items (3)") {
		t.Error("feature issues must not be listed as work items")
	}
}

// A session killed by its timeout is an infrastructure failure: the worker
// runs it again instead of escalating.
func TestInfrastructureFailureIsRetried(t *testing.T) {
	t.Setenv("FAKE_DEV_HANG", "1")
	h := newHarness(t, baseTOML+`
retries = 1
retry_delay = "0s"
[roles.developer]
timeout = "5s"
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
`)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true} // reviewer disabled: PR auto-approved

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:approved" {
		t.Fatalf("history: %s", got)
	}
	if len(h.gh.comments[1]) != 0 {
		t.Fatalf("no escalation expected: %v", h.gh.comments[1])
	}
	dev := h.sessions(config.RoleDeveloper)
	if len(dev) != 2 {
		t.Fatalf("developer sessions: %v", dev)
	}
	// The retry keeps its own transcript directory.
	if !strings.Contains(filepath.Base(dev[1]), "-retry1-") {
		t.Errorf("retry session directory not named as a retry: %s", dev[1])
	}
	first, _ := os.ReadFile(filepath.Join(dev[0], "prompt.md"))
	if strings.Contains(string(first), "previous attempt was interrupted") {
		t.Errorf("first attempt got the retry preamble:\n%s", first)
	}
	retry, _ := os.ReadFile(filepath.Join(dev[1], "prompt.md"))
	if !strings.Contains(string(retry), "previous attempt was interrupted") {
		t.Errorf("retry is missing the interrupted-work preamble:\n%s", retry)
	}
}

// A session that ran and reported `failed` made a decision: it escalates at
// once, however many retries are configured.
func TestReportedFailureEscalatesWithoutRetry(t *testing.T) {
	t.Setenv("FAKE_DEV_FAIL", "1")
	h := newHarness(t, baseTOML+`
retries = 2
retry_delay = "0s"
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
`)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:needs-human" {
		t.Fatalf("history: %s", got)
	}
	if got := len(h.sessions(config.RoleDeveloper)); got != 1 {
		t.Fatalf("developer sessions: got %d want 1", got)
	}
	if len(h.gh.comments[1]) != 1 || !strings.Contains(h.gh.comments[1][0], "ended with `failed`") {
		t.Fatalf("comments: %v", h.gh.comments[1])
	}
}

// workHoursTOML is baseTOML with a mon-fri 09:00-18:00 window in UTC and
// every role disabled, so only the polling loop itself is exercised.
const workHoursTOML = baseTOML + `
off_hours_poll_interval = "1h"
work_hours = "09:00-18:00"
work_days = ["mon", "tue", "wed", "thu", "fri"]
timezone = "UTC"
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

func TestOffHoursPollingIsThrottled(t *testing.T) {
	// 2026-08-29 12:00 UTC is a Saturday: outside the window.
	h := newHarnessAt(t, workHoursTOML, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Later", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}}
	ctx := context.Background()

	full, err := h.sched.tick(ctx)
	if err != nil || !full {
		t.Fatalf("first tick: full=%v err=%v", full, err)
	}
	if h.gh.callCount("issue list") != 1 || h.gh.callCount("pr list") != 1 {
		t.Fatalf("first tick should poll once: %v", h.gh.calls)
	}
	// The loop keeps ticking every poll_interval, but off hours the next
	// GitHub poll is an hour out, so the second pass is local.
	h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	full, err = h.sched.tick(ctx)
	if err != nil || full {
		t.Fatalf("second tick: full=%v err=%v", full, err)
	}
	if h.gh.callCount("issue list") != 1 || h.gh.callCount("pr list") != 1 {
		t.Fatalf("a local pass must not poll GitHub: %v", h.gh.calls)
	}

	h.sched.writeStatus()
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.InWorkHours == nil || *st.InWorkHours {
		t.Fatalf("status should report off hours: %+v", st.InWorkHours)
	}
	if want := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC); !st.NextPoll.Equal(want) {
		t.Fatalf("next poll %s, want %s", st.NextPoll, want)
	}

	// Once the off-hours interval has elapsed, polling resumes.
	h.clock.advance(time.Hour)
	if full, err := h.sched.tick(ctx); err != nil || !full {
		t.Fatalf("tick after the off-hours interval: full=%v err=%v", full, err)
	}
	if h.gh.callCount("issue list") != 2 {
		t.Fatalf("expected a second poll: %v", h.gh.calls)
	}
}

func TestInWorkHoursPollsEveryTick(t *testing.T) {
	// 2026-08-31 12:00 UTC is a Monday inside the window.
	h := newHarnessAt(t, workHoursTOML, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if full, err := h.sched.tick(ctx); err != nil || !full {
			t.Fatalf("tick %d: full=%v err=%v", i, full, err)
		}
		h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	}
	if h.gh.callCount("issue list") != 3 || h.gh.callCount("pr list") != 3 {
		t.Fatalf("every tick should poll in work hours: %v", h.gh.calls)
	}
	h.sched.writeStatus()
	if st, _ := h.store.LoadStatus(); st.InWorkHours == nil || !*st.InWorkHours {
		t.Fatalf("status should report work hours: %+v", st.InWorkHours)
	}
}

func TestLocalPassUnblocksIssueOffHours(t *testing.T) {
	h := newHarnessAt(t, workHoursTOML, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Vague", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:blocked"}, {Name: "bees:size/s"}}}
	ctx := context.Background()
	if _, err := h.sched.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.gh.history[1]) != 0 {
		t.Fatalf("nothing has answered the question yet: %v", h.gh.history[1])
	}
	// The project manager answers by mail; the next local pass picks it up
	// without polling GitHub.
	if _, err := h.box.Send(mail.Message{From: config.RoleProjectManager, To: config.RoleDeveloper, Subject: "Re: Vague", Body: "do X", Issue: 1}); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	if full, err := h.sched.tick(ctx); err != nil || full {
		t.Fatalf("second tick: full=%v err=%v", full, err)
	}
	if got := strings.Join(h.gh.history[1], ","); got != "bees:ready" {
		t.Fatalf("history: %v", h.gh.history[1])
	}
	if h.gh.callCount("issue list") != 1 || h.gh.callCount("pr list") != 1 {
		t.Fatalf("the local pass polled GitHub: %v", h.gh.calls)
	}
	// The new state label reached the cached poll, so the local passes until
	// the next one classify the issue as ready and do not move it again.
	cached := h.sched.classify(h.sched.lastIssues, h.sched.lastPRs)
	if len(cached.byState["blocked"]) != 0 || len(cached.byState["ready"]) != 1 {
		t.Fatalf("cached poll still says blocked: %v", h.sched.lastIssues)
	}
	h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	if full, err := h.sched.tick(ctx); err != nil || full {
		t.Fatalf("third tick: full=%v err=%v", full, err)
	}
	if got := strings.Join(h.gh.history[1], ","); got != "bees:ready" {
		t.Fatalf("a local pass moved the label again: %v", h.gh.history[1])
	}
}

// workHoursDevTOML is workHoursTOML with the developer and reviewer enabled,
// so a whole develop -> review -> approve cycle runs off hours.
const workHoursDevTOML = baseTOML + `
off_hours_poll_interval = "1h"
work_hours = "09:00-18:00"
work_days = ["mon", "tue", "wed", "thu", "fri"]
timezone = "UTC"
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

func TestLocalPassDoesNotRedispatchFinishedIssues(t *testing.T) {
	// Saturday: off hours, so the tick after the first one is local and its
	// snapshot still carries the issue's pre-work labels.
	h := newHarnessAt(t, workHoursDevTOML, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if full, err := h.sched.tick(ctx); err != nil || !full {
		t.Fatalf("first tick: full=%v err=%v", full, err)
	}
	h.sched.wg.Wait()
	want := "bees:in-progress,bees:review,bees:in-progress,bees:review,bees:approved"
	before := strings.Join(h.gh.history[1], ",")
	if before != want {
		t.Fatalf("first pass history: %s, want %s", before, want)
	}

	lists, views := h.gh.callCount("issue list")+h.gh.callCount("pr list"), h.gh.callCount("issue view")

	h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	if full, err := h.sched.tick(ctx); err != nil || full {
		t.Fatalf("second tick: full=%v err=%v", full, err)
	}
	h.sched.wg.Wait()
	if got := strings.Join(h.gh.history[1], ","); got != before {
		t.Fatalf("local pass restarted a finished issue: %s", got)
	}
	if n := len(h.sessions(config.RoleDeveloper)); n != 2 {
		t.Fatalf("developer sessions: %d, want 2", n)
	}
	// The candidate cost one live issue view, not a poll.
	if n := h.gh.callCount("issue list") + h.gh.callCount("pr list"); n != lists {
		t.Fatalf("a local pass must not poll GitHub: %v", h.gh.calls)
	}
	if n := h.gh.callCount("issue view"); n != views+1 {
		t.Fatalf("live checks: %d, want 1", n-views)
	}
	// The refreshed issue replaces the stale cached one, so the next local
	// pass classifies it as approved and does not check it again.
	views = h.gh.callCount("issue view")
	h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	if full, err := h.sched.tick(ctx); err != nil || full {
		t.Fatalf("third tick: full=%v err=%v", full, err)
	}
	h.sched.wg.Wait()
	if h.gh.callCount("issue view") != views {
		t.Fatalf("an issue the cache already knows is approved was checked again: %v", h.gh.calls)
	}
	if got := strings.Join(h.gh.history[1], ","); got != before {
		t.Fatalf("local pass restarted a finished issue: %s", got)
	}
}

// An issue a human filed without a state label is labelled bees:feedback
// once: the cached poll a local pass classifies from is updated with the new
// label, so the passes in between two GitHub polls do not repeat the edit.
func TestLocalPassDoesNotRepeatTheFeedbackLabel(t *testing.T) {
	// Saturday: off hours, so only the first tick polls GitHub.
	h := newHarnessAt(t, workHoursTOML, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Filed from the GitHub UI", State: "OPEN", Labels: []github.Label{{Name: "bees"}}}
	ctx := context.Background()

	if full, err := h.sched.tick(ctx); err != nil || !full {
		t.Fatalf("first tick: full=%v err=%v", full, err)
	}
	lists := h.gh.callCount("issue list")
	for i := 0; i < 2; i++ {
		h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
		if full, err := h.sched.tick(ctx); err != nil || full {
			t.Fatalf("local tick %d: full=%v err=%v", i, full, err)
		}
	}
	if got := strings.Join(h.gh.history[1], ","); got != "bees:feedback" {
		t.Fatalf("label history: %q, want bees:feedback once", got)
	}
	if n := h.gh.callCount("issue list"); n != lists {
		t.Fatalf("a local pass polled GitHub: %v", h.gh.calls)
	}
}

// noRolesTOML disables every role, so a pass does nothing but reconcile.
const noRolesTOML = baseTOML + `
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

func TestReadyIssueWithoutASizeGetsTheDefault(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	// 1 was fast-tracked to ready by a human and has no size; 2 was sized
	// by the project manager.
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Fast-tracked", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}}, CreatedAt: time.Now()}
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "Sized", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/xs"}}, CreatedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.history[1], ","); got != "bees:size/m" {
		t.Fatalf("issue 1 label history: %q, want bees:size/m", got)
	}
	if got := h.gh.history[2]; len(got) != 0 {
		t.Fatalf("issue 2 was already sized and must be left alone: %v", got)
	}
	if !github.HasLabel(h.gh.issues[1].Labels, "bees:ready") {
		t.Fatalf("issue 1 lost its state label: %v", h.gh.issues[1].Labels)
	}
	if !strings.Contains(h.logs.String(), "ready issue without a size gets the default") {
		t.Fatalf("no log line about the default size:\n%s", h.logs.String())
	}
	// reconcile sized the issue and recounts before the status is written,
	// so the first pass already reports it as "m" rather than unsized.
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.ReadySizes["m"] != 1 || st.ReadySizes["xs"] != 1 || st.ReadySizes[""] != 0 {
		t.Fatalf("ready sizes after the first pass: %v", st.ReadySizes)
	}

	// Second pass: the label is there, so nothing is added again and the
	// breakdown reports it.
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.history[1], ","); got != "bees:size/m" {
		t.Fatalf("size added twice: %q", got)
	}
	st, err = h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.ReadySizes["m"] != 1 || st.ReadySizes["xs"] != 1 || st.ReadySizes[""] != 0 {
		t.Fatalf("ready sizes after the second pass: %v", st.ReadySizes)
	}
}

// An issue reconcile unblocks joins the ready queue after the blocked loop
// has run, so the size backstop has to come last to size it in the same pass.
func TestUnblockedIssueIsSizedInTheSamePass(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Vague", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:blocked"}}}
	// The answer the developer asked for is waiting in the mailbox.
	if _, err := h.box.Send(mail.Message{From: config.RoleProjectManager, To: config.RoleDeveloper, Subject: "Re: Vague", Body: "do X", Issue: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.history[1], ","); got != "bees:ready,bees:size/m" {
		t.Fatalf("label history: %q, want bees:ready,bees:size/m", got)
	}
	// Both edits reached the cached poll, so the local passes until the next
	// poll classify the issue as a sized ready one and add neither again.
	cached := h.sched.classify(h.sched.lastIssues, h.sched.lastPRs).byState["ready"]
	if len(cached) != 1 || !github.HasLabel(cached[0].Labels, "bees:ready") || !github.HasLabel(cached[0].Labels, "bees:size/m") {
		t.Fatalf("cached issue: %v", h.sched.lastIssues)
	}
	h.sched.localPass(ctx)
	if got := strings.Join(h.gh.history[1], ","); got != "bees:ready,bees:size/m" {
		t.Fatalf("a local pass repeated the edits: %q", got)
	}
}

func TestSizeSurvivesTheStateMachine(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/xs"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// ready -> in-progress -> review -> ... -> approved, and the size label
	// is still there: state transitions only touch the state labels.
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:review,bees:in-progress,bees:review,bees:approved" {
		t.Fatalf("label history: %s", got)
	}
	if !github.HasLabel(h.gh.issues[1].Labels, "bees:size/xs") {
		t.Fatalf("size label lost: %v", h.gh.issues[1].Labels)
	}
	// The reviewer was told the size.
	dirs := h.sessions(config.RoleReviewer)
	if len(dirs) == 0 {
		t.Fatal("no reviewer session")
	}
	prompt, _ := os.ReadFile(filepath.Join(dirs[0], "system-prompt.md"))
	if !strings.Contains(string(prompt), "this is an `xs` change") {
		t.Fatalf("reviewer system prompt does not mention the size:\n%s", prompt)
	}
}

// An issue a human filed without a state label is counted under "no_state",
// never under the empty string, and the count is refreshed after reconcile has
// moved it out — not left stale until the next poll. It leaves the state
// machine entirely: bees:feedback is a kind, so it lands in no queue at all.
func TestNoStateQueueIsNamedAndRecountedAfterReconcile(t *testing.T) {
	h := newHarness(t, baseTOML)
	h.sched.OnlyRoles = map[string]bool{}
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Filed from the GitHub UI", State: "OPEN", Labels: []github.Label{{Name: "bees"}}}
	// A blocked issue whose question reconcile is about to answer: it must
	// leave the blocked bucket, not be counted in both.
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "Vague", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:blocked"}}}
	if _, err := h.box.Send(mail.Message{From: config.RoleProjectManager, To: config.RoleDeveloper, Subject: "Re: Vague", Body: "do X", Issue: 2}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.sched.writeStatus()
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Queues[""]; ok {
		t.Errorf("unnamed queue key in status: %+v", st.Queues)
	}
	if st.Queues["no_state"] != 1 {
		t.Errorf("after poll: no_state = %d, want 1 (%+v)", st.Queues["no_state"], st.Queues)
	}
	if st.Queues["blocked"] != 1 {
		t.Errorf("after poll: blocked = %d, want 1 (%+v)", st.Queues["blocked"], st.Queues)
	}

	if err := h.sched.reconcile(ctx, snap); err != nil {
		t.Fatal(err)
	}
	h.sched.writeStatus()
	if st, err = h.store.LoadStatus(); err != nil {
		t.Fatal(err)
	}
	if st.Queues["no_state"] != 0 {
		t.Errorf("after reconcile: no_state = %d, want 0 (%+v)", st.Queues["no_state"], st.Queues)
	}
	// Issue 1 became feedback, which is no queue: it must not have been
	// dropped into triage on the way out.
	if st.Queues["triage"] != 0 {
		t.Errorf("after reconcile: triage = %d, want 0 (%+v)", st.Queues["triage"], st.Queues)
	}
	if !github.HasLabel(h.gh.issues[1].Labels, "bees:feedback") {
		t.Errorf("issue 1 labels %v, want bees:feedback", h.gh.issues[1].Labels)
	}
	// The unblocked issue moved to ready; counting it in both buckets would
	// make `bees status` report more issues than exist.
	if st.Queues["blocked"] != 0 {
		t.Errorf("after reconcile: blocked = %d, want 0 (%+v)", st.Queues["blocked"], st.Queues)
	}
	if st.Queues["ready"] != 1 {
		t.Errorf("after reconcile: ready = %d, want 1 (%+v)", st.Queues["ready"], st.Queues)
	}
}

// Attaching loose work items to their feature is once-a-pass product manager
// work, so its task must say which items are already attached: runProductManager
// looks each open work item's parent up and the prompt renders it.
func TestProductManagerSeesEachWorkItemsParent(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	now := time.Now()
	// The fake answers the parent query for issue 1 with feature #5.
	h.gh.issues[5] = &github.Issue{Number: 5, Title: "Exports", Body: "csv please", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feature"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "work items: #1\n\n<!-- bees:product_manager -->", CreatedAt: now}}}
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Export to CSV", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	// A bug QA filed: attached to nothing, which is what the column is for.
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "Header is wrong", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}, {Name: "bees:bug"}, {Name: "bees:size/s"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	pm := h.sessions(config.RoleProductManager)
	if len(pm) != 1 {
		t.Fatalf("product manager sessions: %d", len(pm))
	}
	prompt, _ := os.ReadFile(filepath.Join(pm[0], "prompt.md"))
	if !strings.Contains(string(prompt), "| 1 | ready | - | #5 Exports | - | Export to CSV |") {
		t.Errorf("work item #1 should name its parent feature:\n%s", prompt)
	}
	if !strings.Contains(string(prompt), "| 2 | triage | bug | - | - | Header is wrong |") {
		t.Errorf("loose work item #2 should show - for its parent:\n%s", prompt)
	}
}

// TestQAReceivesHumanMail: `bees mail send --from human --to qa` is a
// documented channel, and until #199 runQA built its session with no Inbox at
// all, so a message sat unread forever. The message reaches the next QA
// session — this test's own next run is its interval plus a new merge, not
// the mail — and is marked read there, which is what keeps the following
// session's mail section empty.
func TestQAReceivesHumanMail(t *testing.T) {
	h := newHarnessAt(t, baseTOML+"\n[roles.developer]\nenabled = false\n[roles.project_manager]\nenabled = false\n[roles.product_manager]\nenabled = false\n", time.Now())
	merged := h.clock.now().Add(-time.Minute)
	h.gh.prs[300] = &github.PR{Number: 300, Title: "Merged", State: "MERGED", HeadRefName: "bees/issue-9",
		Labels: []github.Label{{Name: "bees"}}, MergedAt: &merged}
	if _, err := h.box.Send(mail.Message{From: HumanSender, To: config.RoleQA,
		Subject: "Focus", Body: "test the mail commands by hand this time"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	qa := h.sessions(config.RoleQA)
	if len(qa) != 1 {
		t.Fatalf("qa sessions: %d", len(qa))
	}
	first, _ := os.ReadFile(filepath.Join(qa[0], "prompt.md"))
	for _, want := range []string{"## Mail for you (1)", "test the mail commands by hand this time"} {
		if !strings.Contains(string(first), want) {
			t.Errorf("the qa session is missing %q:\n%s", want, first)
		}
	}
	if unread, _ := h.box.List(mail.Filter{To: config.RoleQA, UnreadOnly: true}); len(unread) != 0 {
		t.Errorf("qa mail left unread: %+v", unread)
	}
	// The next run, once the interval has passed and something else merged,
	// is not told the same thing again.
	next := h.clock.now().Add(time.Hour)
	h.gh.prs[300].MergedAt = &next
	h.clock.advance(2 * time.Hour)
	forcePoll(h)
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if qa = h.sessions(config.RoleQA); len(qa) != 2 {
		t.Fatalf("qa sessions after the second pass: %d", len(qa))
	}
	second, _ := os.ReadFile(filepath.Join(qa[1], "prompt.md"))
	if strings.Contains(string(second), "test the mail commands by hand this time") {
		t.Errorf("the message was delivered twice:\n%s", second)
	}
	if !strings.Contains(string(second), "## Mail for you (0)") || !strings.Contains(string(second), "_No new mail._") {
		t.Errorf("the second qa session has no empty mail section:\n%s", second)
	}
}
