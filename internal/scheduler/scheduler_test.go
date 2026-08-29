package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/workspace"
)

// TestMain lets the test binary double as a fake `claude` when
// BEES_FAKE_CLAUDE is set: the runner executes it, it inspects its role and
// environment, performs a scripted action and prints a stream-json result.
func TestMain(m *testing.M) {
	if os.Getenv("BEES_FAKE_CLAUDE") == "1" {
		fakeClaude()
		os.Exit(0)
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
	git := func(args ...string) {
		if _, err := workspace.Git(context.Background(), ".", args...); err != nil {
			fail(err)
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
		if err := os.WriteFile(fmt.Sprintf("work-%d.txt", n), []byte("done"), 0o644); err != nil {
			fail(err)
		}
		git("add", ".")
		git("-c", "user.email=bee@example.com", "-c", "user.name=bee", "commit", "-q", "-m", fmt.Sprintf("work %d", n))
		git("push", "-q")
		if os.Getenv(session.EnvPR) == "" {
			// "Open" the PR: the fake gh treats the marker as the PR existing.
			if err := os.WriteFile(filepath.Join(stateDir, "fake-pr-created"), nil, 0o644); err != nil {
				fail(err)
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
			if _, err := box.Send(mail.Message{From: role, To: config.RoleDeveloper, Subject: "Required check failed: go / test", Body: "main error: TestX fails\n\n" + string(prompt), PR: pr, Issue: issue}); err != nil {
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
	fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"fake","num_turns":2,"total_cost_usd":0.01}`)
}

// fakeGH is an in-memory GitHub backing the gh wrapper.
type fakeGH struct {
	mu       sync.Mutex
	issues   map[int]*github.Issue
	prs      map[int]*github.PR
	prMarker string
	history  map[int][]string // label additions per number, in order
	comments map[int][]string
	merged   []int
	// activity is raw JSON served for api pulls/N/reviews, pulls/N/comments, issues/N/comments
	activity map[string]string
	// checks is a queue of responses for `pr checks`; the last one repeats.
	checks    []checksResponse
	mergeArgs [][]string
}

type checksResponse struct {
	json string
	err  error
}

func (f *fakeGH) exec(ctx context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
		return true
	}
	if args[0] == "api" {
		if args[1] == "graphql" {
			// parent lookup: issue 1 has parent 5 when it exists
			if strings.Contains(strings.Join(args, " "), "number=1") {
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
			if i, ok := f.issues[n]; ok {
				i.Assignees = append(i.Assignees, github.Author{Login: a})
			} else if p, ok := f.prs[n]; ok {
				p.Assignees = append(p.Assignees, github.Author{Login: a})
			}
			f.history[n] = append(f.history[n], "assignee:"+a)
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
		if len(f.checks) == 0 {
			return nil, fmt.Errorf("no checks reported on the 'bees/issue-1' branch")
		}
		r := f.checks[0]
		if len(f.checks) > 1 {
			f.checks = f.checks[1:]
		}
		return []byte(r.json), r.err
	case "api repos/acme/widgets/milestones?state=open&per_page=100":
		return []byte("[]"), nil
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
}

func newHarness(t *testing.T, toml string) *harness {
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
		history:  map[int][]string{},
		comments: map[int][]string{},
		activity: map[string]string{},
	}
	client := github.New(cfg.Project.Repo)
	client.Exec = gh.exec

	t.Setenv("BEES_FAKE_CLAUDE", "1")
	runner := &session.Runner{
		ClaudeBin:   os.Args[0],
		SessionsDir: store.SessionsDir(),
		StateDir:    store.Dir,
		Repo:        cfg.Project.Repo,
		Label:       cfg.Filter.Label,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	ws := workspace.NewManager(clone, filepath.Join(t.TempDir(), "ws"))
	box := mail.Open(store.MailDir())
	sched, err := New(Deps{Config: cfg, GitHub: client, Mail: box, Runner: runner, Workspaces: ws, Store: store, Logger: runner.Logger})
	if err != nil {
		t.Fatal(err)
	}
	sched.Once = true
	return &harness{t: t, cfg: cfg, gh: gh, store: store, box: box, sched: sched, clone: clone}
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
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}}, CreatedAt: time.Now()}
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "Human filed this", Body: "hi", State: "OPEN", Labels: []github.Label{{Name: "bees"}}, CreatedAt: time.Now()}
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
	// Issue 2 had no state label and entered triage.
	if got := h.gh.history[2]; strings.Join(got, ",") != "bees:triage" {
		t.Fatalf("issue 2 label history: %v", got)
	}
	// Sessions: 2 developer, 2 reviewer, and each singleton once.
	for role, n := range map[string]int{config.RoleDeveloper: 2, config.RoleReviewer: 2, config.RoleProjectManager: 1, config.RoleProductManager: 1, config.RoleQA: 1} {
		if got := len(h.sessions(role)); got != n {
			t.Errorf("%s sessions: got %d want %d", role, got, n)
		}
	}
	// The second developer session received the reviewer's mail.
	dev := h.sessions(config.RoleDeveloper)
	prompt, _ := os.ReadFile(filepath.Join(dev[1], "prompt.md"))
	if !strings.Contains(string(prompt), "please add tests") || !strings.Contains(string(prompt), "review round 2 of 3") {
		t.Fatalf("second developer prompt:\n%s", prompt)
	}
	// The project manager saw issue 2 in its triage list.
	pjm := h.sessions(config.RoleProjectManager)
	prompt, _ = os.ReadFile(filepath.Join(pjm[0], "prompt.md"))
	if !strings.Contains(string(prompt), "#2: Human filed this") {
		t.Fatalf("project manager prompt:\n%s", prompt)
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
	want := "bees:ready,bees:in-progress,bees:approved"
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
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "x", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}}}
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
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Done already", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:approved"}}, CreatedAt: created}
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
		{"id": 556, "user": {"login": "kyle"}, "body": "will do <!-- bees:developer -->", "path": "seed.txt", "line": 1, "html_url": "https://x/556", "created_at": %q}
	]`, now, now)
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
	for _, want := range []string{"Feedback on PR #101 from kyle", "please rename this", "pulls/101/comments/555/replies", "seed.txt:1"} {
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
`)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Ship it", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}}, CreatedAt: time.Now()}
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
	for _, want := range []string{"required checks failed on pull request #101", "**go / test** (CI) — fail: 1 test failed", "https://ci.example.com/run/1", "do not assume GitHub"} {
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
`)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Slow CI", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}}, CreatedAt: time.Now()}
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
		Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "filed #10 for this <!-- bees:product_manager -->", CreatedAt: now.Add(-time.Hour)}}}
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
		{Author: github.Author{Login: "kyle"}, Body: "created #11 <!-- bees:product_manager -->", CreatedAt: now.Add(time.Second)},
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
			{Author: github.Author{Login: "kyle"}, Body: "Fuzzy or exact? <!-- bees:product_manager -->", CreatedAt: now.Add(-2 * time.Hour)},
			{Author: github.Author{Login: "kyle"}, Body: "fuzzy", CreatedAt: now.Add(-time.Minute)},
		}}
	// A feature already broken down: the PM commented last.
	h.gh.issues[7] = &github.Issue{Number: 7, Title: "Done planning", Body: "x", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feature"}}, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now,
		Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "work items: #8 #9 <!-- bees:product_manager -->", CreatedAt: now.Add(-time.Hour)}}}
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
	for _, want := range []string{"#5: Exports", "#6: Search", "fuzzy", "| 7 | - | 1/3 done | - | Done planning |"} {
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
