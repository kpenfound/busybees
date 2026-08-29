package mail

import (
	"strings"
	"testing"
)

func TestSendListMark(t *testing.T) {
	box := Open(t.TempDir())
	m1, err := box.Send(Message{From: "developer", To: "project_manager", Subject: "q", Body: "how?", Issue: 3})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := box.Send(Message{From: "reviewer", To: "developer", Subject: "review", Body: "fix", PR: 9, Issue: 3})
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
	byPR, _ := box.List(Filter{PR: 9})
	if len(byPR) != 1 || byPR[0].ID != m2.ID {
		t.Fatalf("filter pr: %+v", byPR)
	}
	byIssue, _ := box.List(Filter{Issue: 3})
	if len(byIssue) != 2 {
		t.Fatalf("filter issue: %+v", byIssue)
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
	text := Format(got)
	if !strings.Contains(text, "### review") || !strings.Contains(text, "- pr: #9") || !strings.Contains(text, "fix") {
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
