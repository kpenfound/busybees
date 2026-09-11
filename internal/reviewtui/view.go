package reviewtui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/kpenfound/busybees/internal/review"
	"github.com/kpenfound/busybees/internal/text"
)

// The screen: a header naming the finding and where it stands in the
// queue, the diff pane and the finding pane, side by side in a terminal
// wide enough and one above the other otherwise, and a footer with the keys,
// the text being typed, or what the last key came to.

// The kinds of row the diff pane has, beyond the three kinds of diff line
// (review.LineAdded, LineRemoved, LineContext), which are rows too.
const (
	// rowFile starts a file: its path, or both paths of a rename.
	rowFile = "file"
	// rowHunk is a hunk's header.
	rowHunk = "hunk"
	// rowMarked is a diff line the finding is about, whatever its kind.
	rowMarked = "marked"
	// rowSuggestion is a line of the finding's suggestion, under the last
	// marked line.
	rowSuggestion = "suggestion"
	// rowNote is a remark of the pane's own: a file with no lines, an
	// empty diff.
	rowNote = "note"
)

// diffRow is one row of the diff pane, laid out and not yet painted.
type diffRow struct {
	kind string
	text string
}

// leadIn is how many lines above the first marked line the diff pane
// opens on.
const leadIn = 5

// sideBySide is the narrowest terminal the two panes are drawn beside each
// other in; narrower, they are stacked.
const sideBySide = 100

// diffRows lays the diff out as rows, one per file, hunk and line, with
// the finding's lines marked in the gutter and its suggestion under the last
// of them, and returns them with the row to scroll to: a few lines above
// the first marked line, never above its hunk's header, and the top when
// nothing is marked.
func diffRows(v *review.DiffView) ([]diffRow, int) {
	var rows []diffRow
	top, hunkRow, marked := 0, 0, false
	gutter := gutterWidth(v)
	for i, f := range v.Files {
		if i > 0 {
			rows = append(rows, diffRow{kind: rowNote})
		}
		name := f.Path
		if f.OldPath != f.Path {
			name = f.OldPath + " → " + f.Path
		}
		rows = append(rows, diffRow{kind: rowFile, text: "─ " + name})
		if len(f.Hunks) == 0 {
			rows = append(rows, diffRow{kind: rowNote, text: "  (no lines: a binary file, a mode change or a rename with no edit)"})
		}
		for j, h := range f.Hunks {
			if v.InDiff && i == v.MarkedFile && j == v.MarkedHunk {
				hunkRow = len(rows)
			}
			rows = append(rows, diffRow{kind: rowHunk, text: h.Header})
			for _, l := range h.Lines {
				if l.Marked && !marked {
					marked = true
					top = max(hunkRow, len(rows)-leadIn)
				}
				rows = append(rows, lineRow(l, gutter))
				if l.Suggestion != "" {
					rows = append(rows, diffRow{kind: rowNote, text: strings.Repeat(" ", gutter) + " suggestion:"})
					for _, s := range strings.Split(strings.TrimRight(l.Suggestion, "\n"), "\n") {
						rows = append(rows, diffRow{kind: rowSuggestion, text: strings.Repeat(" ", gutter) + " │ " + expand(s)})
					}
				}
			}
		}
	}
	if len(rows) == 0 {
		rows = []diffRow{{kind: rowNote, text: "the diff is empty"}}
	}
	return rows, top
}

// lineRow lays one diff line out: a mark in the gutter for a line the
// finding is about, its number on each side, the diff's own sign and the
// text.
func lineRow(l review.DiffLine, gutter int) diffRow {
	kind, mark := l.Kind, " "
	if l.Marked {
		kind, mark = rowMarked, "▌"
	}
	sign := " "
	switch l.Kind {
	case review.LineAdded:
		sign = "+"
	case review.LineRemoved:
		sign = "-"
	}
	w := (gutter - 2) / 2
	return diffRow{kind: kind, text: fmt.Sprintf("%s%*s %*s %s%s", mark, w, number(l.Old), w, number(l.New), sign, expand(l.Text))}
}

// gutterWidth is how many columns the mark and the two line numbers take:
// wide enough for the diff's largest line number on either side.
func gutterWidth(v *review.DiffView) int {
	largest := 0
	for _, f := range v.Files {
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				largest = max(largest, l.Old, l.New)
			}
		}
	}
	return 2 + 2*max(3, len(strconv.Itoa(largest)))
}

// number renders a line number, and nothing for the side a line is not on.
func number(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// expand replaces tabs, which a terminal would put anywhere, with four
// spaces.
func expand(s string) string { return strings.ReplaceAll(s, "\t", "    ") }

// ---- drawing ---------------------------------------------------------------

// View draws the screen.
func (m Model) View() string {
	w := m.width
	var panes string
	if w >= sideBySide {
		lw := w * 3 / 5
		h := max(1, m.height-5)
		panes = lipgloss.JoinHorizontal(lipgloss.Top, m.diffPanel(lw-4, h), m.findingPanel(w-lw-4, h))
	} else {
		rows := m.height - 2
		h := max(1, rows/2-3)
		panes = lipgloss.JoinVertical(lipgloss.Left, m.diffPanel(w-4, h), m.findingPanel(w-4, rows-h-6))
	}
	return m.header(w) + "\n" + panes + "\n" + m.footer(w)
}

// header names the finding and where it stands in the queue, the way the
// console's heading does, with the severity painted.
func (m Model) header(w int) string {
	if m.total == 0 {
		return hintStyle.Render(clip("nothing left undecided", w))
	}
	f := m.finding
	where := fmt.Sprintf("[%d of %s undecided] ", m.pos+1, text.Count(m.total, "finding"))
	rest := fmt.Sprintf(" · %s · %s", f.Angle, f.Category)
	ref := m.q.Artifact.Brief.Ref.String()
	line := clip(where+f.ID+" · "+f.Severity+rest, w)
	if pad := w - lipgloss.Width(line) - lipgloss.Width(ref) - 2; pad > 0 {
		line += strings.Repeat(" ", pad) + hintStyle.Render(ref)
	}
	// The severity is painted after the line has been cut, so a header too
	// wide for the terminal is cut where an unpainted one would be.
	return strings.Replace(line, " · "+f.Severity+" · ", " · "+severityStyle(f.Severity).Render(f.Severity)+" · ", 1)
}

// diffPanel draws the diff pane: h rows of the diff from where it is
// scrolled to, each cut to w and then painted by its kind.
func (m Model) diffPanel(w, h int) string {
	first, last := window(m.diffScroll, len(m.rows), h)
	lines := make([]string, 0, h)
	for _, r := range m.rows[first:last] {
		lines = append(lines, rowStyle(r.kind).Render(clip(r.text, w)))
	}
	title := "Diff"
	if m.view.InDiff {
		title += " · " + m.view.Files[m.view.MarkedFile].Path
	}
	return m.panel(paneDiff, title, first, last, len(m.rows), lines, w, h)
}

// findingPanel draws the finding pane: the finding, wrapped to w, or in
// its place the help or the comment text being edited.
func (m Model) findingPanel(w, h int) string {
	var body []string
	title := "Finding"
	switch {
	case m.mode == modeHelp:
		title, body = "Keys", wrap(help, w)
	case m.mode == modeInput && m.input.multiline:
		title = "Finding · " + m.input.prompt
		body = wrap(m.input.draft+"▏", w)
	default:
		body = wrap(m.text, w)
	}
	first, last := window(m.findingScroll, len(body), h)
	lines := make([]string, 0, h)
	for _, l := range body[first:last] {
		lines = append(lines, clip(l, w))
	}
	return m.panel(paneFinding, title, first, last, len(body), lines, w, h)
}

// window is the range of n lines a pane h high shows from a scroll
// position, clamped so the last page is full.
func window(scroll, n, h int) (first, last int) {
	first = min(max(0, scroll), max(0, n-h))
	return first, min(n, first+h)
}

// panel draws one pane: a titled box w wide inside, h rows high, saying
// in the title where in its lines it is when they do not all fit, and
// which pane the scroll keys move.
func (m Model) panel(p pane, title string, first, last, n int, lines []string, w, h int) string {
	if n > h {
		title += fmt.Sprintf(" · %d-%d of %d", first+1, last, n)
	}
	style := plainTitleStyle
	if m.focus == p && m.mode != modeHelp {
		title, style = "▸ "+title, titleStyle
	}
	body := strings.Join(lines, "\n")
	return boxStyle(style).Width(w + 2).Height(h + 1).Render(style.Render(clip(title, w)) + "\n" + body)
}

// footer is the keys, the text being typed, what an ask is waiting on, or
// what the last key came to.
func (m Model) footer(w int) string {
	switch {
	case m.mode == modeAsking:
		return hintStyle.Render(clip(fmt.Sprintf("asking the %s angle…", m.asking), w))
	case m.mode == modeInput && m.input.multiline:
		return hintStyle.Render(clip("enter starts a new line · ctrl-d selects with this text · esc cancels", w))
	case m.mode == modeInput:
		return clip(m.input.prompt+m.input.draft+"▏", w)
	case m.mode == modeHelp:
		return hintStyle.Render(clip("any key closes the help", w))
	case m.notice != "":
		return warnStyle.Render(clip(m.notice, w))
	}
	return hintStyle.Render(clip(keys, w))
}

// keys is the footer at rest.
const keys = "s select · e edit · d dismiss · f defer · a ask · n next · tab pane · ↑/↓ scroll · q quit · ? help"

// help says what each key does, in the console's words for the keys the
// two share.
const help = `  s  select: the finding goes into the review's output as written
  e  edit the comment text on screen, then select
     (enter starts a new line, ctrl-d selects with the text, esc cancels)
  d  dismiss: leave it out, and record why in your reviewer notes
  f  defer: leave it out of this review, and record nothing
  a  ask the angle that found it a question; a finding the answer
     turns up joins the queue
  n  next: leave it undecided for now, and show the next one
  q  quit: what has been decided is kept, and the rest is still here
     next time

  tab       move the scroll keys to the other pane
  ↑/↓ j/k   scroll a line · pgup/pgdn a page · g/G to the top/bottom
  ?  this help`

// pageHeight is how many lines a page key moves: the height of a pane.
func (m Model) pageHeight() int {
	if m.width >= sideBySide {
		return max(1, m.height-5)
	}
	return max(1, (m.height-2)/2-3)
}

// wrap breaks every line of s that is wider than w at the last space that
// fits, keeping the line's indentation on the lines it breaks into, and cuts
// a word wider than w. A w of 0 or less wraps nothing.
func wrap(s string, w int) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		line = expand(line)
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		for {
			r := []rune(line)
			if w <= 0 || len(r) <= w {
				out = append(out, line)
				break
			}
			at := w
			if i := strings.LastIndex(string(r[:w+1]), " "); i > len(indent) {
				at = len([]rune(string(r[:w+1])[:i]))
			}
			out = append(out, strings.TrimRight(string(r[:at]), " "))
			line = indent + strings.TrimLeft(string(r[at:]), " ")
			if strings.TrimSpace(line) == "" {
				break
			}
		}
	}
	return out
}

// clip cuts s to w cells, ending it with an ellipsis when it had to.
func clip(s string, w int) string {
	if w <= 0 || lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}
