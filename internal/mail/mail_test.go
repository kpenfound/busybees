package mail

import (
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/work"
)

func TestSendListMark(t *testing.T) {
	box := Open(t.TempDir())
	m1, err := box.Send(Message{From: "developer", To: "project_manager", Subject: "q", Body: "how?", Work: work.Ref{Key: "task-A", Tags: map[string]string{"task": "A"}}})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := box.Send(Message{From: "reviewer", To: "developer", Subject: "review", Body: "fix", Work: work.Ref{Key: "task-A", Tags: map[string]string{"task": "A", "change": "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Send(Message{From: "x", To: "", Body: "y"}); err == nil {
		t.Fatal("expected error for missing recipient")
	}

	all, err := box.List(Filter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %d %v", len(all), err)
	}
	if all[0].ID != m1.ID {
		t.Fatalf("expected oldest first, got %s", all[0].ID)
	}
	forPM, _ := box.List(Filter{To: "project_manager", UnreadOnly: true})
	if len(forPM) != 1 || forPM[0].Body != "how?" {
		t.Fatalf("filter to: %+v", forPM)
	}
	byChange, _ := box.List(Filter{Tags: map[string]string{"change": "B"}})
	if len(byChange) != 1 || byChange[0].ID != m2.ID {
		t.Fatalf("filter change: %+v", byChange)
	}
	byTask, _ := box.List(Filter{Tags: map[string]string{"task": "A"}})
	if len(byTask) != 2 {
		t.Fatalf("filter task: %+v", byTask)
	}
	if err := box.MarkRead(m1); err != nil {
		t.Fatal(err)
	}
	unread, _ := box.List(Filter{UnreadOnly: true})
	if len(unread) != 1 || unread[0].ID != m2.ID {
		t.Fatalf("unread after mark: %+v", unread)
	}
	counts, _ := box.Counts()
	if counts["developer"] != 1 || counts["project_manager"] != 0 {
		t.Fatalf("counts: %v", counts)
	}
	got, err := box.Get(m2.ID)
	if err != nil || got.Subject != "review" {
		t.Fatalf("get: %+v %v", got, err)
	}
	text := Format(got, Field{Name: "change", Value: "B"})
	if !strings.Contains(text, "### review") || !strings.Contains(text, "- change: B") || !strings.Contains(text, "fix") {
		t.Fatalf("format: %s", text)
	}
}

func TestEmptyBox(t *testing.T) {
	box := Open(t.TempDir() + "/missing")
	msgs, err := box.List(Filter{To: "qa"})
	if err != nil || len(msgs) != 0 {
		t.Fatalf("empty: %v %v", msgs, err)
	}
}

func TestOpaqueAddressingAndRoleMail(t *testing.T) {
	box := Open(t.TempDir())
	refs := []work.Ref{{Key: "custom/path", Tags: map[string]string{"lane": "A", "empty": ""}}, {Key: "other", Tags: map[string]string{"lane": "A"}}, {Tags: map[string]string{"annotation": "role only"}}}
	for _, ref := range refs {
		if _, err := box.Send(Message{From: "caller", To: "role", Subject: "message", Work: ref}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		filter Filter
		want   int
	}{{Filter{Key: "custom/path"}, 1}, {Filter{Tags: map[string]string{"lane": "A"}}, 2}, {Filter{Tags: map[string]string{"empty": ""}}, 1}, {Filter{Key: "custom/path", Tags: map[string]string{"lane": "wrong"}}, 0}, {Filter{Unaddressed: true}, 1}} {
		got, err := box.List(tc.filter)
		if err != nil || len(got) != tc.want {
			t.Fatalf("filter %+v: %d %v", tc.filter, len(got), err)
		}
	}
}
