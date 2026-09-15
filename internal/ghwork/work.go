// Package ghwork is busybees' GitHub adapter for opaque runtime identity.
package ghwork

import (
	"strconv"
	"strings"

	"github.com/kpenfound/busybees/core/work"
)

const IssueTag = "github.issue"
const PRTag = "github.pr"

func IssueKey(n int) work.Key {
	if n <= 0 {
		return ""
	}
	return work.Key("issue-" + strconv.Itoa(n))
}
func PRKey(n int) work.Key {
	if n <= 0 {
		return ""
	}
	return work.Key("pr-" + strconv.Itoa(n))
}

// New uses the issue as the primary work item when both subjects are known.
// Tags preserve both routing addresses independently of the primary key.
func New(issue, pr int) work.Ref {
	r := work.Ref{}
	r = WithPR(r, pr)
	return WithIssue(r, issue)
}
func Issue(r work.Ref) int { n, _ := strconv.Atoi(r.Tags[IssueTag]); return n }
func PR(r work.Ref) int    { n, _ := strconv.Atoi(r.Tags[PRTag]); return n }
func WithIssue(r work.Ref, n int) work.Ref {
	old := Issue(r)
	primary := r.Key == "" || r.Key == IssueKey(old) || r.Key == PRKey(PR(r))
	r = withTag(r, IssueTag, n)
	if primary {
		if n > 0 {
			r.Key = IssueKey(n)
		} else {
			r.Key = PRKey(PR(r))
		}
	}
	return r
}
func WithPR(r work.Ref, n int) work.Ref {
	primary := r.Key == "" || r.Key == PRKey(PR(r))
	r = withTag(r, PRTag, n)
	if primary && Issue(r) == 0 {
		r.Key = PRKey(n)
	}
	return r
}
func withTag(r work.Ref, key string, n int) work.Ref {
	r = r.Clone()
	if r.Tags == nil {
		r.Tags = map[string]string{}
	}
	if n > 0 {
		r.Tags[key] = strconv.Itoa(n)
	} else {
		delete(r.Tags, key)
	}
	return r
}

// Number parses a busybees key at the GitHub boundary.
func Number(key work.Key) int {
	s := string(key)
	for _, prefix := range []string{"issue-", "pr-"} {
		if strings.HasPrefix(s, prefix) {
			n, _ := strconv.Atoi(strings.TrimPrefix(s, prefix))
			return n
		}
	}
	return 0
}
func Keys(numbers []int) []work.Key {
	if numbers == nil {
		return nil
	}
	out := make([]work.Key, len(numbers))
	for i, n := range numbers {
		out[i] = IssueKey(n)
	}
	return out
}
func Numbers(keys []work.Key) []int {
	if keys == nil {
		return nil
	}
	out := make([]int, len(keys))
	for i, k := range keys {
		out[i] = Number(k)
	}
	return out
}
func Dependencies(deps map[int][]int) map[work.Key][]work.Key {
	if deps == nil {
		return nil
	}
	out := map[work.Key][]work.Key{}
	for k, v := range deps {
		out[IssueKey(k)] = Keys(v)
	}
	return out
}
