package statemigrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/busybees/internal/ghwork"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"mail/developer/issue.json":                                    `{"id":"issue","from":"human","to":"developer","subject":"question","body":"body","issue":12,"created_at":"2026-09-14T12:00:00Z","in_reply_to":"earlier","extension":{"keep":true}}`,
		"mail/reviewer/pr.json":                                        `{"id":"pr","from":"human","to":"reviewer","subject":"review","body":"body","pr":34,"created_at":"2026-09-14T12:01:00Z"}`,
		"mail/developer/both.json":                                     `{"id":"both","from":"reviewer","to":"developer","subject":"fix","body":"body","issue":12,"pr":34,"created_at":"2026-09-14T12:02:00Z","read_at":"2026-09-14T13:00:00Z"}`,
		"mail/qa/role.json":                                            `{"id":"role","from":"human","to":"qa","subject":"hello","body":"body","created_at":"2026-09-14T12:03:00Z"}`,
		"issues/12.json":                                               `{"number":12,"round":3,"pr":34,"branch":"bees/issue-12","check_fix_rounds":2,"worker_stage":"checks","after_develop":"checks","pre_review_done":true,"review_artifact":"reviews/34/first","reviewed_head":"abcd","reviewed_sha":"recorded-head","session":{"role":"developer","name":"interrupted","dir":"/legacy/sessions/interrupted","started_at":"2026-09-14T14:00:00Z"},"human_seen_at":"2026-09-14T13:00:00Z","issue_human_seen_at":"2026-09-14T13:01:00Z","conflict_notified_sha":"cdef","cost":7.25,"sessions":5,"proposal":true,"proposal_approved_at":"2026-09-14T12:00:00Z","open_children":[55,56],"complete_reported_at":"2026-09-14T14:00:00Z","escalation":"reason","escalated_at":"2026-09-14T14:01:00Z","updated_at":"2026-09-14T14:02:00Z","extension":"kept"}`,
		"issues/77.json":                                               `{"number":77,"reviewed_sha":"review-head","updated_at":"2026-09-14T14:02:00Z"}`,
		"sessions/interrupted/issue":                                   "12\n",
		"sessions/interrupted/interrupted":                             "stopped by bees kill\n",
		"sessions/interrupted/transcript.jsonl":                        "{\"type\":\"system\"}\n",
		"sessions/finished/issue":                                      "12\n",
		"sessions/finished/outcome.json":                               `{"status":"pr-updated","issue":12,"pr":34,"note":"kept"}`,
		"sessions/finished/result.json":                                `{"name":"finished","has_outcome":true,"cost_usd":2.5,"outcome":{"status":"pr-updated","issue":12,"pr":34,"note":"kept"}}`,
		"sessions/20260914-140000-reviewer-pr-34-123/transcript.jsonl": "{}\n",
		"status.json":                                                  `{"pid":123,"workers":[{"name":"dev","issue":12,"stage":"checks","round":3,"resumed":true},{"name":"review-77","issue":77,"stage":"requested review"}],"priority":[12],"waiting_on_deps":{"12":[55,56]},"needs_human":[{"issue":12,"reason":"kept"}],"approved":[{"issue":12,"pr":34,"title":"kept"}]}`,
		"ledger.jsonl":                                                 "{\"time\":\"2026-09-14T12:00:00Z\",\"issue\":12,\"pr\":34,\"role\":\"developer\",\"session\":\"one\",\"cost_usd\":2.5,\"turns\":8}\n{\"time\":\"2026-09-14T13:00:00Z\",\"issue\":12,\"role\":\"reviewer\",\"session\":\"two\",\"cost_usd\":4.75}\n{\"time\":\"2026-09-14T14:00:00Z\",\"pr\":77,\"role\":\"reviewer\",\"session\":\"three\",\"cost_usd\":1}\n{truncated",
	}
	for name, body := range files {
		put(t, filepath.Join(dir, name), body)
	}
	return dir
}
func put(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || strings.HasSuffix(path, ".lock") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestMigrationPreservesLegacyState(t *testing.T) {
	dir := fixture(t)
	before := snapshot(t, dir)
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, dir)
	if _, ok := after["issues/12.json"]; ok {
		t.Fatal("legacy bookkeeping survived")
	}
	if _, ok := after["sessions/interrupted/issue"]; ok {
		t.Fatal("legacy marker survived")
	}
	for _, name := range []string{"sessions/interrupted/transcript.jsonl", "sessions/interrupted/interrupted"} {
		if after[name] != before[name] {
			t.Fatalf("changed %s", name)
		}
	}
	book := after[filepath.Join("issues", ghwork.IssueKey(12).Filename())]
	for _, part := range []string{`"key": "issue-12"`, `"github.issue": "12"`, `"github.pr": "34"`, `"cost": 7.25`, `"sessions": 5`, `"extension": "kept"`, `"open_children": [`, `"issue-55"`, `"worker_stage": "checks"`, `"name": "interrupted"`} {
		if !strings.Contains(book, part) {
			t.Errorf("bookkeeping missing %s: %s", part, book)
		}
	}
	// Every unrelated field, including every interruption/review clock, survives.
	var old, current object
	if err := json.Unmarshal([]byte(before["issues/12.json"]), &old); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(book), &current); err != nil {
		t.Fatal(err)
	}
	for key, want := range old {
		if key == "number" || key == "pr" || key == "open_children" {
			continue
		}
		var a, b any
		_ = json.Unmarshal(want, &a)
		_ = json.Unmarshal(current[key], &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("lost field %s", key)
		}
	}
	if !strings.Contains(after[filepath.Join("issues", ghwork.PRKey(77).Filename())], `"key": "pr-77"`) {
		t.Fatal("requested review lost its PR identity")
	}
	ledger := after["ledger.jsonl"]
	if !strings.HasSuffix(ledger, "\n{truncated") || strings.Count(ledger, "cost_usd") != 3 {
		t.Fatalf("ledger damaged: %s", ledger)
	}
	for _, name := range []string{"mail/developer/issue.json", "mail/reviewer/pr.json", "mail/developer/both.json", "mail/qa/role.json", "sessions/finished/outcome.json", "sessions/finished/result.json", "status.json"} {
		if strings.Contains(after[name], `"issue":`) || strings.Contains(after[name], `"pr":`) {
			t.Errorf("legacy identity in %s", name)
		}
	}
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, snapshot(t, dir)) {
		t.Fatal("completed rerun changed files")
	}
}

func TestMigrationRestartAfterEveryStep(t *testing.T) {
	baseline := fixture(t)
	steps := 0
	if err := migrate(baseline, func(string) error { steps++; return nil }); err != nil {
		t.Fatal(err)
	}
	want := snapshot(t, baseline)
	if steps < 12 {
		t.Fatalf("fixture exercised only %d steps", steps)
	}
	for stop := 1; stop <= steps; stop++ {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			dir := fixture(t)
			count := 0
			injected := errors.New("interrupted")
			err := migrate(dir, func(string) error {
				count++
				if count == stop {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("want injected failure, got %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, Marker)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed migration published marker")
			}
			// A crash may also leave an incomplete, uniquely named staging file.
			put(t, filepath.Join(dir, "issues", ".migration-abandoned.tmp"), "half a file")
			if err := Ensure(dir); err != nil {
				t.Fatal(err)
			}
			got := snapshot(t, dir)
			delete(got, "issues/.migration-abandoned.tmp")
			if !reflect.DeepEqual(want, got) {
				t.Fatal("restart changed, lost or duplicated records")
			}
		})
	}
}
func TestMigrationConflictKeepsRecoverableSource(t *testing.T) {
	dir := fixture(t)
	source := filepath.Join(dir, "issues", "12.json")
	before, _ := os.ReadFile(source)
	dest := filepath.Join(dir, "issues", ghwork.IssueKey(12).Filename())
	put(t, dest, `{"work":{"key":"issue-12"},"cost":999}`)
	if err := Ensure(dir); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("want collision failure: %v", err)
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(before, after) {
		t.Fatal("conflict discarded source")
	}
	if _, err := os.Stat(filepath.Join(dir, Marker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("conflict marked complete")
	}
}
func TestMigrationCorruptionAndIOFailure(t *testing.T) {
	for _, name := range []string{"mail/developer/issue.json", "issues/12.json", "sessions/finished/outcome.json", "sessions/interrupted/issue", "sessions/new/work.json", "status.json", "ledger.jsonl"} {
		t.Run(name, func(t *testing.T) {
			dir := fixture(t)
			path := filepath.Join(dir, name)
			if name == "ledger.jsonl" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				put(t, path, "{broken")
			}
			if err := Ensure(dir); err == nil {
				t.Fatal("corrupt state accepted")
			}
			if _, err := os.Stat(filepath.Join(dir, Marker)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed rewrite marked complete")
			}
		})
	}
}
func TestConcurrentMigration(t *testing.T) {
	dir := fixture(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := Ensure(dir); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	want := fixture(t)
	if err := Ensure(want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot(t, dir), snapshot(t, want)) {
		t.Fatal("concurrent migration duplicated records")
	}
}
