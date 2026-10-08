package mail

import (
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/work"
)

// sendFixture sends the two messages the rest of this file's scenarios
// filter, mark and format: m1 is older, addressed to project_manager and
// tagged only "task"; m2 is newer, addressed to developer and tagged both
// "task" and "change".
func sendFixture(t *testing.T, box *Box) (m1, m2 Message) {
	t.Helper()
	m1, err := box.Send(Message{From: "developer", To: "project_manager", Subject: "q", Body: "how?", Work: work.Ref{Key: "task-A", Tags: map[string]string{"task": "A"}}})
	if err != nil {
		t.Fatal(err)
	}
	m2, err = box.Send(Message{From: "reviewer", To: "developer", Subject: "review", Body: "fix", Work: work.Ref{Key: "task-A", Tags: map[string]string{"task": "A", "change": "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	return m1, m2
}

// Send rejects a message addressed to no role, before anything is written.
func TestSendRejectsAMessageWithNoRecipient(t *testing.T) {
	box := Open(t.TempDir())
	if _, err := box.Send(Message{From: "x", To: "", Body: "y"}); err == nil {
		t.Fatal("expected error for missing recipient")
	}
	all, err := box.List(Filter{})
	if err != nil || len(all) != 0 {
		t.Fatalf("a rejected message was written: %+v %v", all, err)
	}
}

// List with no filter returns every message, oldest first — the order the
// mailbox tools render a role's mail in.
func TestListReturnsEveryMessageOldestFirst(t *testing.T) {
	box := Open(t.TempDir())
	m1, m2 := sendFixture(t, box)
	all, err := box.List(Filter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %d %v", len(all), err)
	}
	if all[0].ID != m1.ID || all[1].ID != m2.ID {
		t.Fatalf("expected %s then %s, got %s then %s", m1.ID, m2.ID, all[0].ID, all[1].ID)
	}
}

// List filters by recipient role and by unread status, independently of
// each other.
func TestListFiltersByRecipientAndUnreadStatus(t *testing.T) {
	box := Open(t.TempDir())
	m1, _ := sendFixture(t, box)
	forPM, err := box.List(Filter{To: "project_manager", UnreadOnly: true})
	if err != nil || len(forPM) != 1 || forPM[0].Body != "how?" {
		t.Fatalf("filter to: %+v %v", forPM, err)
	}
	if err := box.MarkRead(m1); err != nil {
		t.Fatal(err)
	}
	forPM, err = box.List(Filter{To: "project_manager", UnreadOnly: true})
	if err != nil || len(forPM) != 0 {
		t.Fatalf("filter to+unread after marking read: %+v %v", forPM, err)
	}
}

// List filters on the opaque work tags a message carries, matching whatever
// subset of tags the caller names.
func TestListFiltersByWorkTags(t *testing.T) {
	box := Open(t.TempDir())
	_, m2 := sendFixture(t, box)
	byChange, err := box.List(Filter{Tags: map[string]string{"change": "B"}})
	if err != nil || len(byChange) != 1 || byChange[0].ID != m2.ID {
		t.Fatalf("filter change: %+v %v", byChange, err)
	}
	byTask, err := box.List(Filter{Tags: map[string]string{"task": "A"}})
	if err != nil || len(byTask) != 2 {
		t.Fatalf("filter task: %+v %v", byTask, err)
	}
}

// MarkRead delivers a message exactly once: a later List(UnreadOnly) no
// longer sees it, and the unaffected message still does.
func TestMarkReadRemovesAMessageFromUnread(t *testing.T) {
	box := Open(t.TempDir())
	m1, m2 := sendFixture(t, box)
	if err := box.MarkRead(m1); err != nil {
		t.Fatal(err)
	}
	unread, err := box.List(Filter{UnreadOnly: true})
	if err != nil || len(unread) != 1 || unread[0].ID != m2.ID {
		t.Fatalf("unread after mark: %+v %v", unread, err)
	}
}

// Counts reports unread mail per recipient role, the number the scheduler's
// mailbox indicator shows.
func TestCountsCountsUnreadMessagesPerRecipient(t *testing.T) {
	box := Open(t.TempDir())
	m1, _ := sendFixture(t, box)
	if err := box.MarkRead(m1); err != nil {
		t.Fatal(err)
	}
	counts, err := box.Counts()
	if err != nil || counts["developer"] != 1 || counts["project_manager"] != 0 {
		t.Fatalf("counts: %v %v", counts, err)
	}
}

// Get finds a message by ID regardless of which role's directory holds it.
func TestGetFindsAMessageByID(t *testing.T) {
	box := Open(t.TempDir())
	_, m2 := sendFixture(t, box)
	got, err := box.Get(m2.ID)
	if err != nil || got.Subject != "review" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := box.Get("missing"); err == nil {
		t.Error("Get found a message that was never sent")
	}
}

// Format renders the subject heading, the caller-supplied fields and the
// body, the prompt text a role's mailbox tool hands back.
func TestFormatRendersSubjectFieldsAndBody(t *testing.T) {
	box := Open(t.TempDir())
	_, m2 := sendFixture(t, box)
	got, err := box.Get(m2.ID)
	if err != nil {
		t.Fatal(err)
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
