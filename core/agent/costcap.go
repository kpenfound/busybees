package agent

import (
	"errors"
	"fmt"
	"math"
)

// SubtypeCostCap is the ErrorSubtype of a session whose known cost reached
// Request.CostCapUSD, whether the runner stopped it in flight or it ended
// on its own with a final cost at or over the cap.
const SubtypeCostCap = "cost_cap"

// errCostCap is the cause a running turn is cancelled with when its known
// cost reaches the cap, which is what tells that stop apart from the
// caller's cancellation.
var errCostCap = errors.New("session cost cap reached")

// validCostCap refuses a cap that cannot be compared with a cost.
func validCostCap(usd float64) error {
	if usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return fmt.Errorf("cost cap %v: want a finite amount of USD, or 0 for none", usd)
	}
	return nil
}

// costMeter is what a running turn is known to have cost so far. A
// backend's consume reports every cost its stream carries, in the form the
// backend reports it (add for a step's cost, total for a running total),
// and the meter stops the turn once a known cost reaches the cap. A stream
// that carries no cost leaves it unknown, which never reaches the cap.
type costMeter struct {
	// cap is Request.CostCapUSD; zero is no cap.
	cap float64
	// stop cancels the running turn; called once, when the cap is reached.
	stop    func()
	usd     float64
	known   bool
	stopped bool
}

// add records the cost of one step, for a backend that reports each
// step's cost on its own (opencode, pi).
func (m *costMeter) add(usd float64) {
	if !validCost(usd) {
		return
	}
	m.observe(m.usd + usd)
}

// total records the session's cost so far, for a backend whose events
// carry a running total (claude's result event).
func (m *costMeter) total(usd float64) {
	if !validCost(usd) {
		return
	}
	m.observe(usd)
}

func (m *costMeter) observe(usd float64) {
	m.usd, m.known = usd, true
	if m.reached() && !m.stopped && m.stop != nil {
		m.stopped = true
		m.stop()
	}
}

// reached reports whether a known cost has reached the cap.
func (m *costMeter) reached() bool {
	return m.cap > 0 && m.known && m.usd >= m.cap
}

// validCost says whether a reported cost is one: a stream value that is
// negative or not a number says nothing about what was spent.
func validCost(usd float64) bool {
	return usd >= 0 && !math.IsInf(usd, 0)
}
