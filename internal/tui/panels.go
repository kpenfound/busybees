package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/state"
)

// recentRows is how many finished sessions the view remembers. It is a view
// of what just happened, not a log: ledger.jsonl has every session's turns
// and cost, and bees.log has every record.
const recentRows = 8

// recentPanel lists the sessions that have finished, newest first: who ran
// it, what it was about, how it ended and what it cost. Everything in it
// arrived on a session-ended event — nothing is looked up.
func (m Model) recentPanel(w, rows, from int) string {
	if len(m.recent) == 0 {
		return hintStyle.Render("no sessions have finished yet")
	}
	out := []string{headerStyle.Render(clip(recentRow("  ", "role", "issue", "pr", "outcome", "took", "cost", "note"), w))}
	out = append(out, listRows(len(m.recent), rows, func(i int) string {
		f := m.recent[i]
		return clip(recentRow(
			mark(m.cursor, from+i),
			prompts.Title(f.role),
			number(f.issue),
			number(f.pr),
			clip(outcomeText(f.outcome), outcomeWidth),
			dur(f.took),
			fmt.Sprintf("$%.2f", f.cost),
			oneLine(f.note),
		), w)
	})...)
	return strings.Join(out, "\n")
}

// outcomeWidth is the width of the outcome column, which every cell is cut
// to. An outcome is one of a short known set (session.ValidOutcomes) and
// none of them is this long, but the note beside it is a session's own prose
// and must not be able to push it around.
const outcomeWidth = 18

func recentRow(sel, role, issue, pr, outcome, took, cost, note string) string {
	return fmt.Sprintf("%s%-16s %-5s %-5s %-*s %8s %8s  %s", sel, role, issue, pr, outcomeWidth, outcome, took, cost, note)
}

// outcomeText is what a finished session reported, or the word for having
// reported nothing at all.
func outcomeText(o string) string {
	if o == "" {
		return "no outcome"
	}
	return o
}

// needsHumanPanel lists what the factory has given up on and why: the panel
// that tells a person the factory is stuck and waiting for them. Both the
// list and the reasons come from status.json, where the scheduler put them
// on the poll that counted the queues.
func (m Model) needsHumanPanel(w, rows, from int) string {
	if len(m.status.NeedsHuman) == 0 {
		return hintStyle.Render("nothing is waiting for a person")
	}
	out := []string{headerStyle.Render(clip(escalatedRow("  ", "issue", "waiting", "title", "why"), w))}
	out = append(out, listRows(len(m.status.NeedsHuman), rows, func(i int) string {
		e := m.status.NeedsHuman[i]
		return clip(escalatedRow(
			mark(m.cursor, from+i),
			number(e.Issue),
			age(e.Since, m.deps.Now()),
			clip(e.Title, titleWidth),
			reasonText(e),
		), w)
	})...)
	return strings.Join(out, "\n")
}

// titleWidth is the width of the title column in the two issue lists.
const titleWidth = 26

func escalatedRow(sel, issue, waiting, title, why string) string {
	return fmt.Sprintf("%s%-5s %-9s %-*s  %s", sel, issue, waiting, titleWidth, title, why)
}

// reasonText is why the factory gave an issue up. An issue a person labelled
// bees:needs-human by hand — and one escalated before this state directory
// existed — has no reason recorded, and saying so is better than an empty
// cell that reads like a rendering fault.
func reasonText(e state.Escalated) string {
	if e.Reason == "" {
		return "(no reason recorded: labelled by hand, or by an earlier run)"
	}
	return oneLine(e.Reason)
}

// approvedPanel lists the pull requests the reviewer approved that are
// waiting for a person to merge, oldest first.
func (m Model) approvedPanel(w, rows, from int) string {
	if len(m.status.Approved) == 0 {
		return hintStyle.Render("no approved pull requests are waiting to be merged")
	}
	out := []string{headerStyle.Render(clip(approvedRow("  ", "pr", "issue", "open", "title"), w))}
	out = append(out, listRows(len(m.status.Approved), rows, func(i int) string {
		a := m.status.Approved[i]
		return clip(approvedRow(
			mark(m.cursor, from+i),
			number(a.PR),
			number(a.Issue),
			age(a.Since, m.deps.Now()),
			a.Title,
		), w)
	})...)
	return strings.Join(out, "\n")
}

func approvedRow(sel, pr, issue, open, title string) string {
	return fmt.Sprintf("%s%-5s %-5s %-9s  %s", sel, pr, issue, open, title)
}

// mark draws the selection: the row the cursor is on is the one k stops and
// o opens, so every panel's rows carry the same two columns for it.
func mark(cursor, row int) string {
	if cursor == row {
		return "▸ "
	}
	return "  "
}

// shown is how many of a list's n entries are drawn in the rows it has been
// given. It is the one answer both listRows and targets read, so the cursor
// can only ever be on a row that is on screen.
//
// A list longer than its rows spends one of them accounting for the entries
// that did not fit. With a single row there is no space for that line and
// the row goes to an entry instead: one of the pull requests waiting to be
// merged says more than the news that some are, and the Queues panel counts
// them either way.
func shown(n, rows int) int {
	switch {
	case rows <= 0:
		return 0
	case n <= rows:
		return n
	case rows == 1:
		return 1
	default:
		return rows - 1
	}
}

// listRows draws a list of n entries in the rows it has been given: row(i)
// per drawn entry, and a last row accounting for the ones that did not fit,
// so what is on screen and what is not always add up. It draws exactly rows
// lines whenever the list has that many entries, which is what lets the
// layout add its panels up (see Model.layout).
func listRows(n, rows int, row func(i int) string) []string {
	show := shown(n, rows)
	out := make([]string, 0, show+1)
	for i := range show {
		out = append(out, row(i))
	}
	if show < n && rows > 1 {
		out = append(out, hintStyle.Render(fmt.Sprintf("  … %d more", n-show)))
	}
	return out
}

// age renders how long ago something happened, coarsely: a person reading a
// queue of things waiting for them wants "3d", not "72h0m0s". A zero time is
// one the factory never recorded.
func age(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	switch d := now.Sub(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// share divides avail rows between the lists, each of which wants as many
// rows as it has entries. A list with nothing in it takes no rows — its
// panel says it is empty in a line of its own — and every list that has
// something keeps one row, which Model.layout makes room for before it calls
// share. What is left over is dealt out a row at a time to whoever still
// wants one, so a list of twenty cannot starve a list of two.
func share(want []int, avail int) []int {
	got := make([]int, len(want))
	for i := range want {
		if want[i] > 0 && avail > 0 {
			got[i], avail = 1, avail-1
		}
	}
	for avail > 0 {
		dealt := false
		for i := range want {
			if avail == 0 {
				break
			}
			if got[i] > 0 && got[i] < want[i] {
				got[i]++
				avail--
				dealt = true
			}
		}
		if !dealt {
			break // every list has all the rows it wants
		}
	}
	return got
}
