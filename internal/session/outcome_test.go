package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidOutcomes(t *testing.T) {
	for role, want := range map[string]string{
		"developer": "pr-opened", "reviewer": "approved", "qa": "done",
		"product_manager": "idle", "project_manager": "idle",
	} {
		valid := ValidOutcomes(role)
		if len(valid) == 0 {
			t.Fatalf("%s has no outcomes", role)
		}
		if err := ValidateOutcome(role, want); err != nil {
			t.Errorf("%s: %v", role, err)
		}
		if err := ValidateOutcome(role, "nonsense"); err == nil {
			t.Errorf("%s accepted a nonsense status", role)
		}
	}
	// An unknown role accepts anything, so `bees exec` debugging still works.
	if err := ValidateOutcome("", "whatever"); err != nil {
		t.Errorf("unknown role: %v", err)
	}
	if ValidOutcomes("nobody") != nil {
		t.Error("unknown role should have no enumerated outcomes")
	}
}

func TestReport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    string
		in      Outcome
		wantErr string
		want    Outcome
	}{
		{name: "normalised", role: "reviewer", in: Outcome{Status: " Approved ", Note: "lgtm"},
			want: Outcome{Status: "approved", Note: "lgtm"}},
		{name: "pr carried through", role: "developer", in: Outcome{Status: "pr-opened", PR: 7, Issue: 3},
			want: Outcome{Status: "pr-opened", PR: 7, Issue: 3}},
		{name: "wrong status for role", role: "developer", in: Outcome{Status: "approved"},
			wantErr: `status "approved" is not valid for developer`},
		{name: "pr-opened needs a pr", role: "developer", in: Outcome{Status: "pr-opened"},
			wantErr: "pr-opened requires a pull request number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			got, err := Report(dir, tc.role, tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if _, err := os.Stat(filepath.Join(dir, OutcomeFile)); !os.IsNotExist(err) {
					t.Fatal("a rejected outcome was written anyway")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("outcome = %+v, want %+v", got, tc.want)
			}
			// Read the file back rather than trusting the returned value.
			b, err := os.ReadFile(filepath.Join(dir, OutcomeFile))
			if err != nil {
				t.Fatal(err)
			}
			var written Outcome
			if err := json.Unmarshal(b, &written); err != nil {
				t.Fatal(err)
			}
			if written != tc.want {
				t.Fatalf("%s = %+v, want %+v", OutcomeFile, written, tc.want)
			}
		})
	}
}

func TestReportOutsideASession(t *testing.T) {
	if _, err := Report("", "developer", Outcome{Status: "question"}); err == nil {
		t.Fatal("want an error without a session directory")
	}
}
