package agent

import (
	"reflect"
	"testing"

	"github.com/kpenfound/busybees/core/work"
)

func TestOpaqueSessionArtifacts(t *testing.T) {
	dir := t.TempDir()
	ref := work.Ref{Key: "queue/你好:42", Tags: map[string]string{"ticket": "opaque", "branch": "anything"}}
	if err := WriteWork(dir, ref); err != nil {
		t.Fatal(err)
	}
	got, err := ReadWork(dir)
	if err != nil || !reflect.DeepEqual(got, ref) {
		t.Fatalf("marker: %+v %v", got, err)
	}
	want := Outcome{Status: "finished", Note: "done", Work: ref}
	if err := WriteOutcome(dir, want); err != nil {
		t.Fatal(err)
	}
	outcome, ok, err := ReadOutcome(dir)
	if err != nil || !ok || !reflect.DeepEqual(outcome, want) {
		t.Fatalf("outcome: %+v %v", outcome, err)
	}
}
