package scheduler

import (
	"fmt"

	"github.com/kpenfound/agentcore/agent/agenttest"
)

// useSessions replaces the role-script binary for tests whose sessions only
// report a result and outcome. Starts claim turns across the whole fixture;
// the final turn repeats. Tests that need git, mail or GitHub edits keep the
// TestMain role script.
func (h *harness) useSessions(backend string, turns ...agenttest.Turn) *agenttest.SessionFixture {
	h.t.Helper()
	f := agenttest.Session(h.t, backend, turns...)
	switch backend {
	case "claude":
		h.sched.runner.ClaudeBin = f.Bin
	case "codex":
		h.sched.runner.CodexBin = f.Bin
	}
	h.fixture = f
	return f
}

func doneTurn() agenttest.Turn {
	return agenttest.Turn{ResultText: "ok", NumTurns: 2, CostUSD: 0.01,
		Outcome: &agenttest.SessionOutcome{Status: OutcomeDone, Note: "ok"}}
}

// reviewTurns scripts the brief, the two s-sized angles and the judge for
// requested-review tests whose judge reports an outcome without posting a
// review or sending mail. The concurrent angles claim their turns in either
// order; both report a finding, and these tests check the judge's outcome.
func reviewTurns(status, note string) []agenttest.Turn {
	turns := reviewPipelineTurns("s", "quick general", "documentation accuracy")
	judge := doneTurn()
	judge.Outcome = &agenttest.SessionOutcome{Status: status, Note: note}
	return append(turns, judge)
}

func reviewPipelineTurns(size string, kinds ...string) []agenttest.Turn {
	turns := []agenttest.Turn{{SessionID: "sid-review-brief", NumTurns: 1, CostUSD: 0.25,
		ResultText: fmt.Sprintf(`{"summary":"Adds the widget.","size":%q,"acceptance_criteria":[{"text":"a widget exists","source":"#1"}],"touched_areas":[{"name":"widgets","paths":["widget.go"],"summary":"adds Widget"}]}`, size)}}
	for _, kind := range kinds {
		turns = append(turns, agenttest.Turn{SessionID: "sid-review-" + kind, NumTurns: 1, CostUSD: 0.25,
			ResultText: fmt.Sprintf(`{"findings":[{"category":"correctness","severity":"medium","file":"widget.go","lines":[%d,%d],"side":"new","title":"%s: Widget does nothing","body":"Widget has an empty body (from the %s angle).","evidence":"func Widget() {}"}]}`, len(kind), len(kind), kind, kind)})
	}
	return turns
}
