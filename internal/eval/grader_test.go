package eval

import (
	"os"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/review"
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

// A rubric that names no pass score passes at DefaultPassScore, and one
// that names 0 records the score without failing the case: the two are not
// the same rubric, so the key is a pointer.
func TestRubricPassScore(t *testing.T) {
	half, none := 0.5, 0.0
	for _, tc := range []struct {
		name string
		pass *float64
		want float64
	}{
		{"unset", nil, DefaultPassScore},
		{"named", &half, 0.5},
		{"zero", &none, 0},
	} {
		if s := (Rubric{Pass: tc.pass}).PassScore(); s != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, s, tc.want)
		}
	}
}

// A pass score of 0 is a rubric that only reports: every score meets it.
func TestRubricPassScoreZeroNeverFails(t *testing.T) {
	zero := 0.0
	r := Rubric{Name: "n", Rubric: "r", Pass: &zero}
	if errs := r.validate(); len(errs) != 0 {
		t.Fatalf("pass = 0 was refused: %v", errs)
	}
	if 0 < r.PassScore() {
		t.Fatalf("a score of 0 does not reach %v", r.PassScore())
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

// The grader is built from the global review configuration and never from
// the profile the eval runs its roles on. With no config.toml, that
// configuration is the defaults file's, so the grader runs as the agent and
// model roles.reviewer.profile selects there, whatever --profile selects for
// the roles.
func TestGraderComesFromTheGlobalConfigNotTheProfile(t *testing.T) {
	global := defaultsConfig(t, `version = 5
[profiles.fast]
agent = "claude"
model = "haiku"
[profiles.slow]
agent = "claude"
model = "opus"
[global]
profile = "fast"
[roles.reviewer]
profile = "slow"
`)

	// --profile fast runs every role on the profile it names.
	sel, err := SelectProfile("fast", nil, global)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range config.Roles {
		if sel.Roles[role] != "claude haiku" {
			t.Fatalf("--profile fast: %s runs %q", role, sel.Roles[role])
		}
	}

	// The grader the run grades with is the global configuration's — the
	// defaults file's reviewer selection — and stays there however the
	// roles are selected.
	g := review.NewAgent(global)
	if g.Provider != "claude" || g.Model != "opus" {
		t.Fatalf("grader = %s/%s, want the defaults file's roles.reviewer.profile", g.Provider, g.Model)
	}

	// And it grades a case: the rubric's score is the fake grader's answer.
	g.ClaudeBin = os.Args[0]
	_, res := runRoleCase(t, config.RoleDeveloper, developerCase, answerRepo(), func(r *Runner) { r.Grader = g })
	if c := checkNamed(t, res, "the pull request says what changed"); !c.Pass || c.Score == nil || *c.Score != 0.9 {
		t.Fatalf("graded check: %+v", c)
	}
}
