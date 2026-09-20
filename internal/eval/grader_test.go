package eval

import (
	"strings"
	"testing"
)

// A grader answers the JSON object it was asked for, whatever it wraps it
// in; anything that is not a score between 0 and 1 is no answer at all.
func TestParseGrade(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       Grade
		wantErr    string
	}{
		{name: "bare", text: `{"score": 0.8, "reasons": "it reads well"}`, want: Grade{Score: 0.8, Reasons: "it reads well"}},
		{name: "fenced", text: "Here is my answer:\n```json\n{\"score\": 1, \"reasons\": \" yes \"}\n```\n", want: Grade{Score: 1, Reasons: "yes"}},
		{name: "zero", text: `{"score": 0, "reasons": "no"}`, want: Grade{Reasons: "no"}},
		{name: "no object", text: "I could not tell.", wantErr: "no JSON object"},
		{name: "not json", text: "{nope}", wantErr: "invalid character"},
		{name: "over one", text: `{"score": 5}`, wantErr: "scored 5, which is not between 0 and 1"},
		{name: "under zero", text: `{"score": -0.5}`, wantErr: "scored -0.5, which is not between 0 and 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGrade(tc.text)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			case got != tc.want:
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A rubric that names no pass score passes at DefaultPassScore.
func TestRubricPassScore(t *testing.T) {
	if s := (Rubric{}).PassScore(); s != DefaultPassScore {
		t.Fatalf("default: %v", s)
	}
	if s := (Rubric{Pass: 0.5}).PassScore(); s != 0.5 {
		t.Fatalf("named: %v", s)
	}
}

// A case's score is the mean of its graded checks, and nothing at all when
// it has none: a mechanical check is not a score of zero.
func TestMeanScore(t *testing.T) {
	if got := meanScore([]Check{{Pass: true}, {Pass: false}}); got != nil {
		t.Fatalf("mechanical checks scored %v", *got)
	}
	if got := scoreText(nil); got != "-" {
		t.Fatalf("no score prints %q", got)
	}
	low, high := 0.25, 0.75
	got := meanScore([]Check{{Score: &low}, {Pass: true}, {Score: &high}})
	if got == nil || *got != 0.5 {
		t.Fatalf("mean: %v", got)
	}
	if text := scoreText(got); text != "0.50" {
		t.Fatalf("score prints %q", text)
	}
}

// A transcript longer than the grader is given keeps its beginning and its
// end, and says where the middle went.
func TestCutKeepsBothEnds(t *testing.T) {
	s := strings.Repeat("a", 40) + strings.Repeat("z", 40)
	if got := cut(s, 200); got != s {
		t.Fatalf("a short text was cut: %q", got)
	}
	got := cut(s, 60)
	if len(got) > 60 {
		t.Fatalf("%d bytes, want at most 60", len(got))
	}
	if !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "zzzz") || !strings.Contains(got, "cut here") {
		t.Fatalf("cut: %q", got)
	}
}
