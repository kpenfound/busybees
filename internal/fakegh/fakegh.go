// Package fakegh is an in-memory GitHub behind the gh wrapper: a GitHub whose
// Exec is assigned to github.Client.Exec answers the gh argument lists the
// client builds from the state it holds, and records what they changed. Tests
// and evals seed it (Load, or the fields directly), drive the factory against
// it and read the state back (Snapshot, or the fields directly) without
// touching real GitHub.
//
// A session runs in its own process and cannot reach the fake; it asks for
// its writes through a directory instead (RequestEdit), which Exec applies
// before it answers the next call, or reaches it through a gh of its own
// that forwards every call to Exec or ExecStdin (internal/eval).
package fakegh

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/busybees/internal/github"
)

// GitHub is an in-memory GitHub for one repository. Its exported fields are
// the state; hold Lock while touching them when Exec may be running.
type GitHub struct {
	mu sync.Mutex
	// Repo is the owner/name the REST paths are read against.
	Repo string
	// Login is who pull requests created through Exec are authored by and
	// who the reviews submitted through an edit are served as.
	Login  string
	Issues map[int]*github.Issue
	PRs    map[int]*github.PR
	// History lists the label additions per number, in order, with
	// "assignee:<login>" and "milestone:<title>" entries for those writes.
	History map[int][]string
	// Comments are the bodies commented on each issue or pull request
	// through Exec, in order.
	Comments map[int][]string
	// Merged lists the pull requests merged through Exec and MergeArgs the
	// argument lists that merged them.
	Merged    []int
	MergeArgs [][]string
	// Activity is raw JSON served for an api path (pulls/N/reviews,
	// pulls/N/comments, issues/N/comments): one page, which --slurp wraps.
	Activity map[string]string
	// Reviews are the reviews sessions submitted, per pull request, served
	// from the reviews endpoint after any Activity fixture for it.
	Reviews map[int][]Review
	// Checks is a queue of responses for `pr checks --required`; the last
	// one repeats. ChecksAll is the same for the call without --required.
	Checks    []ChecksResponse
	ChecksAll []ChecksResponse
	// Calls logs every gh invocation, in order.
	Calls [][]string
	// Labels are the label names that exist in the repository.
	Labels []string
	// Milestones are the open milestones of the repository.
	Milestones []github.Milestone
	// Tags maps tag names to the commit they point at. Releases records tags
	// published with generated notes.
	Tags     map[string]string
	Releases map[string]bool
	// Branches are remote branch heads served through the git ref API.
	Branches map[string]string
	// Parents maps a work item to the feature it is a sub-issue of.
	Parents map[int]int
	// ImplicitParent answers the parent of an issue Parents has no entry
	// for, with the parent's number and title; zero means none. Called with
	// the lock held.
	ImplicitParent func(n int) (int, string)
	// SubIssues overrides the sub-issue summary the REST issue-details call
	// answers for one issue; DefaultSubIssues answers for the rest.
	SubIssues        map[int]github.SubIssueSummary
	DefaultSubIssues github.SubIssueSummary
	// ParentErr makes the parent query fail for one issue.
	ParentErr map[int]error
	// ChildResponse overrides the whole paginated sub-issues response of a
	// parent, and ChildErr makes it fail.
	ChildResponse map[int]string
	ChildErr      map[int]error
	// ErrFor makes a command fail: it is keyed by the command name, either
	// the first two arguments ("label list") or the first one ("label"), or
	// by "requested_reviewers" and "assignees" for the review-request and
	// assignee REST calls.
	ErrFor map[string]error
	// Visible, when set, hides a pull request it returns false for from
	// `pr list` and `pr view`. Called with the lock held.
	Visible func(p *github.PR) bool
	// Diff is what `pr diff` answers for any pull request that exists.
	Diff string
	// DiffFor, when set, answers `pr diff` instead of Diff, from the pull
	// request. Called with the lock held.
	DiffFor func(p github.PR) (string, error)
	// EditsDir is where session processes leave the edits they ask for
	// (RequestEdit); empty means none are read.
	EditsDir string
	// Now is the clock stamping closes, creations and reviews; nil is
	// time.Now.
	Now func() time.Time
	// lastID is the last id Load gave a seeded comment or review.
	lastID int64
}

// ChecksResponse is one answer to `pr checks`: its output and its error.
type ChecksResponse struct {
	JSON string
	Err  error
}

// Review is one review the fake holds on a pull request: its state and when
// it was submitted.
type Review struct {
	State string
	At    time.Time
}

// ReviewStates maps a gh review event to the state GitHub records for it.
var ReviewStates = map[string]string{"approve": "APPROVED", "request-changes": "CHANGES_REQUESTED", "comment": "COMMENTED"}

// New returns an empty GitHub for repo.
func New(repo string) *GitHub {
	return &GitHub{
		Repo:          repo,
		Issues:        map[int]*github.Issue{},
		PRs:           map[int]*github.PR{},
		History:       map[int][]string{},
		Comments:      map[int][]string{},
		Activity:      map[string]string{},
		Reviews:       map[int][]Review{},
		Parents:       map[int]int{},
		SubIssues:     map[int]github.SubIssueSummary{},
		ParentErr:     map[int]error{},
		ChildResponse: map[int]string{},
		ChildErr:      map[int]error{},
		ErrFor:        map[string]error{},
		Tags:          map[string]string{},
		Releases:      map[string]bool{},
		Branches:      map[string]string{"main": "main-head"},
	}
}

// Lock holds the state against Exec, for a caller touching the fields.
func (f *GitHub) Lock() { f.mu.Lock() }

// Unlock releases Lock.
func (f *GitHub) Unlock() { f.mu.Unlock() }

func (f *GitHub) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// CallCount counts logged gh calls whose first two arguments are cmd
// ("issue list", "pr list", ...).
func (f *GitHub) CallCount(cmd string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if len(c) >= 2 && c[0]+" "+c[1] == cmd {
			n++
		}
	}
	return n
}

// Total counts every logged gh call, whatever it was.
func (f *GitHub) Total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}

// Edit is a write a session process asks the fake to make.
type Edit struct {
	Number int      `json:"number"`
	Create bool     `json:"create,omitempty"`
	Title  string   `json:"title,omitempty"`
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
	// Review is the state of a review the session submitted on the pull
	// request — APPROVED, CHANGES_REQUESTED or COMMENTED — which the fake
	// serves from the reviews endpoint. A review edit changes no labels.
	Review string `json:"review,omitempty"`
}

// RequestEdit records one edit in dir for the fake whose EditsDir it is to
// apply. The file name carries the time so edits are applied in the order
// they were asked for.
func RequestEdit(dir string, e Edit) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, fmt.Sprintf("%020d-*.json", time.Now().UnixNano()))
	if err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(e); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// applyEdits applies every edit a session process has asked for and forgets
// it, so the next call sees what the session did. Called with f.mu held.
func (f *GitHub) applyEdits() {
	if f.EditsDir == "" {
		return
	}
	entries, err := os.ReadDir(f.EditsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(f.EditsDir, entry.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		_ = os.Remove(path)
		var e Edit
		if err := json.Unmarshal(b, &e); err != nil {
			continue
		}
		if e.Review != "" {
			// Stamped as it is applied, which is after the session submitted
			// it and before the caller reads it back.
			f.Reviews[e.Number] = append(f.Reviews[e.Number], Review{State: e.Review, At: f.now()})
			continue
		}
		i, ok := f.Issues[e.Number]
		if !ok {
			if !e.Create {
				continue
			}
			i = &github.Issue{Number: e.Number, Title: e.Title, Body: "please", State: "OPEN"}
			f.Issues[e.Number] = i
		}
		i.Labels = removeLabels(i.Labels, e.Remove)
		for _, l := range e.Add {
			if !github.HasLabel(i.Labels, l) {
				i.Labels = append(i.Labels, github.Label{Name: l})
			}
			f.History[e.Number] = append(f.History[e.Number], l)
		}
	}
}

func removeLabels(labels []github.Label, remove []string) []github.Label {
	for _, l := range remove {
		var kept []github.Label
		for _, have := range labels {
			if have.Name != l {
				kept = append(kept, have)
			}
		}
		labels = kept
	}
	return labels
}

// Exec answers one gh invocation; assign it to github.Client.Exec.
func (f *GitHub) Exec(ctx context.Context, args ...string) ([]byte, error) {
	return f.exec(args, nil)
}

// ExecStdin answers one gh invocation with stdin on its standard input, read
// where the arguments name "-" as a file: --body-file and --input, and an
// "@-" field value. Assign it to github.Client.ExecStdin.
func (f *GitHub) ExecStdin(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	return f.exec(args, &stdin)
}

// readArg is the content of a file argument: stdin for "-", else the file.
func readArg(file string, stdin *string) (string, error) {
	if file == "-" {
		if stdin == nil {
			return "", fmt.Errorf("fake gh: %q reads standard input, and the call has none", file)
		}
		return *stdin, nil
	}
	b, err := os.ReadFile(file)
	return string(b), err
}

// bodyOf is the text --body or --body-file gives; ok is false when the call
// has neither.
func bodyOf(args []string, stdin *string) (body string, ok bool, err error) {
	if i := slices.Index(args, "--body"); i >= 0 && i+1 < len(args) {
		return args[i+1], true, nil
	}
	file := flagValue(args, "--body-file")
	if file == "" {
		return "", false, nil
	}
	body, err = readArg(file, stdin)
	return body, err == nil, err
}

// fields are the -f and -F values of an api call, "@file" values read.
func fields(args []string, stdin *string) (map[string]string, error) {
	out := map[string]string{}
	for i, a := range args {
		if (a != "-f" && a != "-F" && a != "--field" && a != "--raw-field") || i+1 >= len(args) {
			continue
		}
		k, v, _ := strings.Cut(args[i+1], "=")
		if file, ok := strings.CutPrefix(v, "@"); ok && a != "-f" && a != "--raw-field" {
			content, err := readArg(file, stdin)
			if err != nil {
				return nil, err
			}
			v = content
		}
		out[k] = v
	}
	return out, nil
}

func (f *GitHub) exec(args []string, stdin *string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyEdits()
	f.Calls = append(f.Calls, append([]string(nil), args...))
	if len(args) == 0 {
		return nil, fmt.Errorf("fake gh: no arguments")
	}
	if len(args) >= 2 {
		if err, ok := f.ErrFor[args[0]+" "+args[1]]; ok {
			return nil, err
		}
	}
	if err, ok := f.ErrFor[args[0]]; ok {
		return nil, err
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("fake gh: unsupported %v", args)
	}
	if args[0] == "api" {
		if out, err, ok := f.api(args, stdin); ok {
			return out, err
		}
	}
	flag := func(name string) string { return flagValue(args, name) }
	flags := func(name string) []string { return flagValues(args, name) }
	num := func() int {
		if len(args) < 3 {
			return 0
		}
		n, _ := strconv.Atoi(args[2])
		return n
	}
	switch args[0] + " " + args[1] {
	case "issue list":
		var out []github.Issue
		label, state, author := flag("--label"), flag("--state"), flag("--author")
		for _, i := range f.Issues {
			if author != "" && !strings.EqualFold(i.Author.Login, author) {
				continue
			}
			if (state == "all" || i.State == "OPEN") && (label == "" || github.HasLabel(i.Labels, label)) {
				out = append(out, *i)
			}
		}
		sort.Slice(out, func(a, b int) bool { return out[a].Number < out[b].Number })
		return json.Marshal(out)
	case "issue view":
		i, ok := f.Issues[num()]
		if !ok {
			return nil, fmt.Errorf("no issue %d", num())
		}
		return json.Marshal(i)
	case "issue create":
		return f.createIssue(args, stdin)
	case "issue edit", "pr edit":
		n := num()
		var labels *[]github.Label
		var title, body *string
		if i, ok := f.Issues[n]; ok && args[0] == "issue" {
			labels, title, body = &i.Labels, &i.Title, &i.Body
		} else if p, ok := f.PRs[n]; ok {
			labels, title, body = &p.Labels, &p.Title, &p.Body
		} else {
			return nil, fmt.Errorf("no item %d", n)
		}
		text, ok, err := bodyOf(args, stdin)
		if err != nil {
			return nil, err
		}
		if ok {
			*body = text
		}
		if t := flag("--title"); t != "" {
			*title = t
		}
		*labels = removeLabels(*labels, flags("--remove-label"))
		for _, l := range flags("--add-label") {
			if !github.HasLabel(*labels, l) {
				*labels = append(*labels, github.Label{Name: l})
			}
			f.History[n] = append(f.History[n], l)
		}
		if a := flag("--add-assignee"); a != "" {
			// The factory must never build this: it fails against GitHub.
			return nil, fmt.Errorf("fake gh: issue edit --add-assignee is deprecated by GitHub, use the REST endpoint")
		}
		return nil, nil
	case "issue comment", "pr comment":
		text, _, err := bodyOf(args, stdin)
		if err != nil {
			return nil, err
		}
		f.Comments[num()] = append(f.Comments[num()], text)
		return nil, nil
	case "issue close":
		i, ok := f.Issues[num()]
		if !ok {
			return nil, fmt.Errorf("no issue %d", num())
		}
		if c := flag("--comment"); c != "" {
			f.Comments[i.Number] = append(f.Comments[i.Number], c)
		}
		at := f.now()
		i.State, i.ClosedAt = "CLOSED", &at
		return nil, nil
	case "pr create":
		return f.createPR(args, stdin)
	case "pr list":
		var out []github.PR
		head, state, author := flag("--head"), flag("--state"), flag("--author")
		for _, p := range f.PRs {
			if !f.visible(p) {
				continue
			}
			if author != "" && !strings.EqualFold(p.Author.Login, author) {
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
		p, ok := f.PRs[num()]
		if !ok || !f.visible(p) {
			return nil, fmt.Errorf("no pr %d", num())
		}
		return json.Marshal(p)
	case "pr diff":
		p, ok := f.PRs[num()]
		if !ok {
			return nil, fmt.Errorf("no pr %d", num())
		}
		if f.DiffFor != nil {
			diff, err := f.DiffFor(*p)
			return []byte(diff), err
		}
		return []byte(f.Diff), nil
	case "pr review":
		if _, ok := f.PRs[num()]; !ok {
			return nil, fmt.Errorf("no pr %d", num())
		}
		if _, _, err := bodyOf(args, stdin); err != nil {
			return nil, err
		}
		for _, event := range slices.Sorted(maps.Keys(ReviewStates)) {
			if slices.Contains(args, "--"+event) {
				f.Reviews[num()] = append(f.Reviews[num()], Review{State: ReviewStates[event], At: f.now()})
				return nil, nil
			}
		}
		return nil, fmt.Errorf("fake gh: pr review needs --approve, --request-changes or --comment")
	case "pr merge":
		f.Merged = append(f.Merged, num())
		f.MergeArgs = append(f.MergeArgs, args)
		return nil, nil
	case "pr checks":
		queue := &f.Checks
		if !slices.Contains(args, "--required") {
			queue = &f.ChecksAll
		}
		if len(*queue) == 0 {
			branch := "bees/issue-1"
			if p, ok := f.PRs[num()]; ok && p.HeadRefName != "" {
				branch = p.HeadRefName
			}
			return nil, fmt.Errorf("no checks reported on the '%s' branch", branch)
		}
		r := (*queue)[0]
		if len(*queue) > 1 {
			*queue = (*queue)[1:]
		}
		return []byte(r.JSON), r.Err
	case "label list":
		out := make([]github.Label, 0, len(f.Labels))
		for _, l := range f.Labels {
			out = append(out, github.Label{Name: l})
		}
		return json.Marshal(out)
	case "label create":
		if len(args) > 2 && !slices.Contains(f.Labels, args[2]) {
			f.Labels = append(f.Labels, args[2])
		}
		return nil, nil
	case "release create":
		if len(args) < 3 || !slices.Contains(args, "--generate-notes") || flag("-R") != f.Repo {
			return nil, fmt.Errorf("fake gh: invalid generated release: %v", args)
		}
		if _, ok := f.Tags[args[2]]; !ok {
			return nil, fmt.Errorf("fake gh: tag %q does not exist", args[2])
		}
		if f.Releases[args[2]] {
			return nil, fmt.Errorf("fake gh: release %q already exists", args[2])
		}
		f.Releases[args[2]] = true
		return nil, nil
	case "api repos/" + f.Repo + "/milestones?state=open&per_page=100":
		var open []github.Milestone
		for _, m := range f.Milestones {
			if m.State != "closed" {
				open = append(open, m)
			}
		}
		return json.Marshal(open)
	}
	return nil, fmt.Errorf("fake gh: unsupported %v", args)
}

func (f *GitHub) visible(p *github.PR) bool {
	return f.Visible == nil || f.Visible(p)
}

// createIssue opens an issue numbered after every issue and pull request
// there is, authored by Login, and answers its URL as gh does.
func (f *GitHub) createIssue(args []string, stdin *string) ([]byte, error) {
	title := flagValue(args, "--title")
	if title == "" {
		return nil, fmt.Errorf("fake gh: issue create needs --title")
	}
	body, _, err := bodyOf(args, stdin)
	if err != nil {
		return nil, err
	}
	n := f.nextNumber()
	i := &github.Issue{Number: n, Title: title, Body: body, State: "OPEN",
		Author: github.Author{Login: f.Login}, CreatedAt: f.now()}
	for _, l := range flagValues(args, "--label") {
		i.Labels = append(i.Labels, github.Label{Name: l})
	}
	for _, a := range flagValues(args, "--assignee") {
		i.Assignees = append(i.Assignees, github.Author{Login: a})
	}
	if m := flagValue(args, "--milestone"); m != "" {
		i.Milestone = &github.MilestoneRef{Title: m}
	}
	f.Issues[n] = i
	return fmt.Appendf(nil, "https://github.com/%s/issues/%d\n", f.Repo, n), nil
}

// createPR opens a pull request numbered after every issue and pull request
// there is, and answers its URL as gh does.
func (f *GitHub) createPR(args []string, stdin *string) ([]byte, error) {
	head, base := flagValue(args, "--head"), flagValue(args, "--base")
	if head == "" || base == "" {
		return nil, fmt.Errorf("fake gh: pr create needs --head and --base")
	}
	for _, p := range f.PRs {
		if p.HeadRefName == head && p.State == "OPEN" {
			return nil, fmt.Errorf("a pull request for branch %q into branch %q already exists", head, base)
		}
	}
	body, _, err := bodyOf(args, stdin)
	if err != nil {
		return nil, err
	}
	n := f.nextNumber()
	url := fmt.Sprintf("https://github.com/%s/pull/%d", f.Repo, n)
	p := &github.PR{Number: n, Title: flagValue(args, "--title"), Body: body, State: "OPEN", URL: url,
		HeadRefName: head, BaseRefName: base, IsDraft: slices.Contains(args, "--draft"),
		Author: github.Author{Login: f.Login}, CreatedAt: f.now(), UpdatedAt: f.now()}
	for _, l := range flagValues(args, "--label") {
		p.Labels = append(p.Labels, github.Label{Name: l})
	}
	f.PRs[n] = p
	return []byte(url + "\n"), nil
}

func (f *GitHub) nextNumber() int {
	n := 0
	for k := range f.Issues {
		n = max(n, k)
	}
	for k := range f.PRs {
		n = max(n, k)
	}
	return n + 1
}

// api answers a REST or GraphQL call it recognises; ok is false for one it
// leaves to the command switch.
func (f *GitHub) api(args []string, stdin *string) (out []byte, err error, ok bool) {
	repo := "repos/" + f.Repo + "/"
	method := flagValue(args, "--method")
	if method == "" {
		method = flagValue(args, "-X")
	}
	var target string
	if i := slices.IndexFunc(args, func(a string) bool { return strings.HasPrefix(a, repo) }); i >= 0 {
		target = args[i]
	}
	var n int
	switch {
	case method == "" && strings.HasPrefix(target, repo+"git/ref/heads/"):
		branch := strings.TrimPrefix(target, repo+"git/ref/heads/")
		sha := f.Branches[branch]
		if sha == "" {
			return nil, fmt.Errorf("fake gh: no branch %q", branch), true
		}
		out, err := json.Marshal(map[string]any{"object": map[string]string{"sha": sha}})
		return out, err, true
	case method == "" && strings.HasPrefix(target, repo+"git/matching-refs/tags/"):
		prefix := strings.TrimPrefix(target, repo+"git/matching-refs/tags/")
		var refs []struct {
			Ref string `json:"ref"`
		}
		for tag := range f.Tags {
			if strings.HasPrefix(tag, prefix) {
				refs = append(refs, struct {
					Ref string `json:"ref"`
				}{Ref: "refs/tags/" + tag})
			}
		}
		slices.SortFunc(refs, func(a, b struct {
			Ref string `json:"ref"`
		}) int {
			return strings.Compare(a.Ref, b.Ref)
		})
		out, err := json.Marshal(refs)
		return out, err, true
	case method == "POST" && target == repo+"git/refs":
		fs, err := fields(args, stdin)
		if err != nil {
			return nil, err, true
		}
		tag, valid := strings.CutPrefix(fs["ref"], "refs/tags/")
		if !valid || tag == "" || fs["sha"] == "" {
			return nil, fmt.Errorf("fake gh: invalid tag ref or sha"), true
		}
		if _, exists := f.Tags[tag]; exists {
			return nil, fmt.Errorf("fake gh: tag %q already exists", tag), true
		}
		f.Tags[tag] = fs["sha"]
		return []byte("{}"), nil, true
	case method == "PATCH" && sscanfAll(target, repo+"milestones/%d", &n):
		fs, err := fields(args, stdin)
		if err != nil {
			return nil, err, true
		}
		if fs["state"] != "closed" {
			return nil, fmt.Errorf("fake gh: milestone state %q", fs["state"]), true
		}
		for i := range f.Milestones {
			if f.Milestones[i].Number == n {
				f.Milestones[i].State = "closed"
				return []byte("{}"), nil, true
			}
		}
		return nil, fmt.Errorf("fake gh: no milestone %d", n), true
	case method == "POST" && sscanfAll(target, repo+"pulls/%d/reviews", &n):
		// A review with its comments, the way github.Client.PostReview
		// submits one: the JSON request on --input.
		raw, err := readArg(flagValue(args, "--input"), stdin)
		if err != nil {
			return nil, err, true
		}
		var r struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("fake gh: review request: %w", err), true
		}
		state, known := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}[r.Event]
		if !known {
			return nil, fmt.Errorf("fake gh: unknown review event %q", r.Event), true
		}
		if _, ok := f.PRs[n]; !ok {
			return nil, fmt.Errorf("no pr %d", n), true
		}
		f.Reviews[n] = append(f.Reviews[n], Review{State: state, At: f.now()})
		return []byte("{}"), nil, true
	case method == "POST" && sscanfAll(target, repo+"issues/%d/sub_issues", &n):
		// The child is named by the id the issue-details call answers,
		// 1000 more than its number.
		fs, err := fields(args, stdin)
		if err != nil {
			return nil, err, true
		}
		id, err := strconv.Atoi(fs["sub_issue_id"])
		if err != nil {
			return nil, fmt.Errorf("fake gh: sub_issue_id: %w", err), true
		}
		f.Parents[id-1000] = n
		return []byte("{}"), nil, true
	case method == "PATCH" && sscanfAll(target, repo+"pulls/%d", &n):
		p, ok := f.PRs[n]
		if !ok {
			return nil, fmt.Errorf("no pr %d", n), true
		}
		fs, err := fields(args, stdin)
		if err != nil {
			return nil, err, true
		}
		if v, ok := fs["body"]; ok {
			p.Body = v
		}
		if v, ok := fs["title"]; ok {
			p.Title = v
		}
		return []byte("{}"), nil, true
	}
	// Assignees and milestones go to the REST endpoints: `gh issue edit
	// --add-assignee` fails against GitHub with a Projects (classic)
	// GraphQL error when the number is a pull request.
	if i := slices.IndexFunc(args, func(a string) bool { return strings.HasSuffix(a, "/assignees") }); i >= 0 {
		if err, ok := f.ErrFor["assignees"]; ok {
			return nil, err, true
		}
		var n int
		if _, err := fmt.Sscanf(args[i], repo+"issues/%d/assignees", &n); err != nil {
			return nil, fmt.Errorf("fake gh: bad assignees path %q", args[i]), true
		}
		for _, v := range flagValues(args, "-f") {
			if login, ok := strings.CutPrefix(v, "assignees[]="); ok {
				f.setAssignee(n, login)
			}
		}
		return []byte("{}"), nil, true
	}
	if method == "PATCH" && len(args) > 3 {
		if _, err := fmt.Sscanf(args[3], repo+"issues/%d", &n); err != nil {
			return nil, fmt.Errorf("fake gh: bad issue path %q", args[3]), true
		}
		for _, v := range flagValues(args, "-F") {
			number, ok := strings.CutPrefix(v, "milestone=")
			if !ok {
				continue
			}
			k, _ := strconv.Atoi(number)
			title := ""
			for _, m := range f.Milestones {
				if m.Number == k {
					title = m.Title
				}
			}
			if title == "" {
				return nil, fmt.Errorf("fake gh: no milestone %s", number), true
			}
			if i, ok := f.Issues[n]; ok {
				i.Milestone = &github.MilestoneRef{Title: title}
			} else if p, ok := f.PRs[n]; ok {
				p.Milestone = &github.MilestoneRef{Title: title}
			}
			f.History[n] = append(f.History[n], "milestone:"+title)
		}
		return []byte("{}"), nil, true
	}
	// Review requests go to the REST endpoint: `gh pr edit --add-reviewer`
	// fails against GitHub with a Projects (classic) GraphQL error.
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasSuffix(a, "/requested_reviewers") }) {
		if err, ok := f.ErrFor["requested_reviewers"]; ok {
			return nil, err, true
		}
		return []byte("{}"), nil, true
	}
	path := args[len(args)-1]
	if strings.Contains(path, "/sub_issues?per_page=") {
		var parent int
		if _, err := fmt.Sscanf(path, repo+"issues/%d/sub_issues?per_page=100", &parent); err != nil {
			return nil, err, true
		}
		if err := f.ChildErr[parent]; err != nil {
			return nil, err, true
		}
		if raw, ok := f.ChildResponse[parent]; ok {
			return []byte(raw), nil, true
		}
		children := []map[string]any{}
		for n, child := range f.Issues {
			if p, _ := f.parentOf(n); p == parent {
				children = append(children, map[string]any{"repository_url": "https://api.github.com/repos/" + f.Repo, "number": n, "state": strings.ToLower(child.State), "user": child.Author, "labels": child.Labels, "assignees": child.Assignees, "milestone": child.Milestone})
			}
		}
		out, err := json.Marshal([]any{children})
		return out, err, true
	}
	if args[1] == "graphql" {
		n := 0
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "number="); ok {
				n, _ = strconv.Atoi(v)
			}
		}
		if err, ok := f.ParentErr[n]; ok {
			return nil, err, true
		}
		if p, title := f.parentOf(n); p != 0 {
			return fmt.Appendf(nil, `{"data":{"repository":{"issue":{"parent":{"number":%d,"title":%q}}}}}`, p, title), nil, true
		}
		return []byte(`{"data":{"repository":{"issue":{"parent":null}}}}`), nil, true
	}
	if n, ok := f.reviewsPath(path); ok && len(f.Reviews[n]) > 0 {
		// One page per source, which --slurp flattens: the fixture, if there
		// is one, and the reviews sessions have submitted.
		pages := []string{}
		if body, ok := f.Activity[path]; ok {
			pages = append(pages, body)
		}
		var out []string
		for i, r := range f.Reviews[n] {
			out = append(out, fmt.Sprintf(`{"id":%d,"user":{"login":%q},"body":"reviewed\n\n<!-- bees:reviewer -->","state":%q,"submitted_at":%q}`,
				9000+i, f.Login, r.State, r.At.Format(time.RFC3339)))
		}
		pages = append(pages, "["+strings.Join(out, ",")+"]")
		return []byte("[" + strings.Join(pages, ",") + "]"), nil, true
	}
	if body, ok := f.Activity[path]; ok {
		return []byte("[" + body + "]"), nil, true // --slurp wraps pages in an array
	}
	// REST issue details: repos/<repo>/issues/N
	if _, err := fmt.Sscanf(path, repo+"issues/%d", &n); err == nil && !strings.Contains(path, "/comments") {
		sum := f.DefaultSubIssues
		if s, ok := f.SubIssues[n]; ok {
			sum = s
		}
		return fmt.Appendf(nil, `{"id": %d, "milestone": null, "sub_issues_summary": {"total": %d, "completed": %d}}`, 1000+n, sum.Total, sum.Completed), nil, true
	}
	if strings.Contains(path, "/pulls/") || strings.Contains(path, "/issues/") {
		return []byte("[[]]"), nil, true
	}
	return nil, nil, false
}

// parentOf is the parent of issue n and its title, from Parents first and
// ImplicitParent after; zero means none.
func (f *GitHub) parentOf(n int) (int, string) {
	if p, ok := f.Parents[n]; ok {
		title := "Feature"
		if i, ok := f.Issues[p]; ok {
			title = i.Title
		}
		return p, title
	}
	if f.ImplicitParent != nil {
		return f.ImplicitParent(n)
	}
	return 0, ""
}

func (f *GitHub) setAssignee(n int, login string) {
	if i, ok := f.Issues[n]; ok {
		i.Assignees = append(i.Assignees, github.Author{Login: login})
	} else if p, ok := f.PRs[n]; ok {
		p.Assignees = append(p.Assignees, github.Author{Login: login})
	}
	f.History[n] = append(f.History[n], "assignee:"+login)
}

// reviewsPath matches the reviews endpoint of a pull request and returns its
// number.
func (f *GitHub) reviewsPath(path string) (int, bool) {
	var n int
	if _, err := fmt.Sscanf(path, "repos/"+f.Repo+"/pulls/%d/reviews", &n); err != nil {
		return 0, false
	}
	return n, true
}

// sscanfAll reports whether path is exactly format with its one number
// filled in, which it stores in n.
func sscanfAll(path, format string, n *int) bool {
	if _, err := fmt.Sscanf(path, format, n); err != nil {
		return false
	}
	return path == fmt.Sprintf(format, *n)
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func flagValues(args []string, name string) []string {
	var out []string
	for i, a := range args {
		if a == name && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}
