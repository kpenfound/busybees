package tui

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/text"
)

// A session's transcript is what its agent writes as it works — claude's
// stream-json, codex's event stream, or opencode's — one JSON object per
// line, teed to transcript.jsonl by the runner. The view reads that file
// rather than the stream: it is already on disk, one line per event, and
// reading it needs nothing of the scheduler.
//
// What a person watching wants from it is what Claude Code itself shows —
// the assistant's own words, the tools it called and how each one answered —
// so everything else in the stream (the thought text, the init and
// rate-limit bookkeeping, the tool schemas) is reduced to a marker or
// dropped. A codex transcript is read to the same lines: each completed
// item is what the session said or did, and the end of its turn is the
// session's end. An opencode transcript is read the same way too: its
// "text" events are what the session said, and a "step_finish" whose
// reason is "stop" is the session's end, with the cost opencode reports per
// step summed into a running total — unlike codex, which never reports
// one. A line the view cannot parse is dropped too: half a JSON object is
// what a transcript being written *right now* ends with, and it is worth
// nothing to a reader.

// Markers each kind of transcript line is prefixed with. They are the ones
// Claude Code's own output uses, so a person who has watched a session in a
// terminal reads this the same way.
const (
	sayMark    = "● "
	thinkMark  = "✻ "
	resultMark = "  ⎿ "
	userMark   = "› "
)

// maxTranscriptLines is how many rendered lines of one session's transcript
// the view keeps in memory. A long session runs to a few thousand; past
// that the oldest are dropped, because the view follows the tail and a
// scrollback nobody can reach is only memory.
const maxTranscriptLines = 4000

// transcriptEntry is one line of the transcript, in the shape the view
// renders. It is deliberately a small subset of claude's stream-json:
// anything not read here is something the view does not show.
type transcriptEntry struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	// Message is claude's message object (whose content blocksOf reads),
	// or the message string of a codex "error" event, so it is decoded by
	// whichever reads it.
	Message json.RawMessage `json:"message"`
	// The fields below are the final "result" event's.
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	// The fields below are codex's: the item of an "item.completed" event,
	// and the error of a "turn.failed" one (an "error" event carries its
	// message at the top level instead).
	Item struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Command string `json:"command"`
		Server  string `json:"server"`
		Tool    string `json:"tool"`
		Status  string `json:"status"`
		Output  string `json:"aggregated_output"`
	} `json:"item"`
	// Error is codex's ("turn.failed"'s Message) and opencode's ("error"'s
	// Name and nested Data.Message) at once: the two never collide, since
	// each backend's stream sets only its own fields.
	Error struct {
		Message string `json:"message"`
		Name    string `json:"name"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
	// Part is opencode's: the part of a "text" or "step_finish" event,
	// mirroring opencodeEvent in internal/session.
	Part struct {
		Type   string  `json:"type"`
		Text   string  `json:"text"`
		Reason string  `json:"reason"`
		Cost   float64 `json:"cost"`
	} `json:"part"`
}

// transcriptBlock is one item of a message's content array. An assistant
// message carries text, thinking and tool_use blocks; a user message in a
// headless session carries tool_result blocks, and (when a turn was typed
// rather than answered) text.
type transcriptBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// readTranscript reads whatever has been appended to a session's transcript
// since byte offset off, and returns the lines to show for it, the offset
// to continue from and the running cost to carry into the next read — an
// opencode transcript reports cost per step rather than once at the end, so
// it is threaded through the same way off is.
//
// Only whole lines are consumed: the runner is writing this file as the
// view reads it, so the last line is regularly half an object. Leaving it
// behind — rather than parsing what is there — is what makes the next read
// see it complete. A transcript that does not exist yet is not an error:
// the session directory is created before claude is started.
func readTranscript(dir string, off int64, cost float64) (lines []string, next int64, nextCost float64, err error) {
	if dir == "" {
		return nil, off, cost, nil
	}
	f, err := os.Open(filepath.Join(dir, session.TranscriptFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, off, cost, nil
		}
		return nil, off, cost, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, off, cost, err
	}
	next, nextCost = off, cost
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// No newline yet: the rest is a line still being written.
			break
		}
		next += int64(len(line))
		var rendered []string
		rendered, nextCost = renderTranscriptLine(line, nextCost)
		lines = append(lines, rendered...)
	}
	return lines, next, nextCost, nil
}

// renderTranscriptLine turns one stream-json line into the lines the view
// shows for it, or none at all, and the running cost to carry forward — an
// opencode "step_finish" event adds to it, everything else passes it
// through unchanged.
func renderTranscriptLine(line []byte, cost float64) ([]string, float64) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, cost
	}
	var e transcriptEntry
	if json.Unmarshal(line, &e) != nil {
		return nil, cost
	}
	switch e.Type {
	case "assistant":
		return assistantLines(blocksOf(e)), cost
	case "user":
		return userLines(blocksOf(e)), cost
	case "result":
		return []string{resultLine(e)}, cost
	case "item.completed":
		return codexItemLines(e), cost
	case "turn.completed", "turn.failed", "error":
		return []string{codexEndLine(e)}, cost
	case "text":
		return prefixed(sayMark, e.Part.Text, 0), cost
	case "step_finish":
		return stepFinishLines(e, cost)
	}
	// "system" (init, thinking-token bookkeeping, task notifications) and
	// "rate_limit_event" are the runner's business, not a reader's; so are
	// codex's "thread.started", "turn.started" and "item.started". So is
	// opencode's "tool_use": internal/session.opencodeBackend.consume does
	// not decode a tool call either, and there is no verified field shape
	// to render one from yet.
	return nil, cost
}

// stepFinishLines renders one opencode step's end, the same event
// internal/session.opencodeBackend.consume reads its cost and outcome
// from: the step's cost is added to the running total, reported so far by
// every step that has finished; a reason of "stop" ends the session well,
// "" and "tool-calls" mean the run goes on and render nothing, and any
// other reason ends it as a failure named after the reason, the same name
// consume gives it.
func stepFinishLines(e transcriptEntry, cost float64) ([]string, float64) {
	cost += e.Part.Cost
	switch e.Part.Reason {
	case "stop":
		return []string{fmt.Sprintf("%ssession ended: ok, $%.2f", sayMark, cost)}, cost
	case "", "tool-calls":
		return nil, cost
	default:
		return []string{sayMark + "session ended: step_" + strings.ReplaceAll(e.Part.Reason, "-", "_")}, cost
	}
}

// codexItemLines renders one completed codex item the way an assistant
// message and the tool result under it are rendered: what the session
// said, the command or MCP tool it called and the first line of the answer.
// The other item kinds (a file change, a web search, a todo list) are
// named; reasoning is a marker, as a thought is.
func codexItemLines(e transcriptEntry) []string {
	it := e.Item
	switch it.Type {
	case "agent_message":
		return prefixed(sayMark, it.Text, 0)
	case "reasoning":
		return []string{thinkMark + "thinking"}
	case "command_execution":
		out := []string{sayMark + "Bash(" + oneLine(it.Command) + ")"}
		return append(out, resultMark+toolResult(transcriptBlock{Content: rawString(it.Output), IsError: it.Status == "failed"}))
	case "mcp_tool_call":
		name := it.Tool
		if it.Server != "" {
			name = "mcp__" + it.Server + "__" + it.Tool
		}
		return []string{sayMark + name + "()"}
	case "":
		return nil
	}
	return []string{sayMark + it.Type}
}

// rawString is a string as the JSON a tool result's content may be.
func rawString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// codexEndLine renders the end of a codex turn: the session is over, and
// codex reports no cost, so none is shown. A "turn.failed" event says why
// under "error"; a bare "error" event says it at the top level, as the
// runner's codex backend reads it too — the same bare "error" type
// opencode's backend ends a session with, whose message is nested under
// "error" instead (Data.Message, falling back to Name), so that is tried
// first.
func codexEndLine(e transcriptEntry) string {
	if e.Type == "turn.completed" {
		return sayMark + "session ended: ok"
	}
	how := "failed"
	msg := e.Error.Data.Message
	if msg == "" {
		msg = e.Error.Name
	}
	if msg == "" {
		msg = e.Error.Message
	}
	if msg == "" {
		var s string
		if json.Unmarshal(e.Message, &s) == nil {
			msg = s
		}
	}
	if msg != "" {
		how += ": " + oneLine(msg)
	}
	return sayMark + "session ended: " + how
}

// blocksOf reads a message's content, which is an array of blocks or — for
// a user turn typed as one string — a single string.
func blocksOf(e transcriptEntry) []transcriptBlock {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(e.Message, &m) != nil {
		return nil
	}
	var blocks []transcriptBlock
	if json.Unmarshal(m.Content, &blocks) == nil {
		return blocks
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil && s != "" {
		return []transcriptBlock{{Type: "text", Text: s}}
	}
	return nil
}

// assistantLines renders what the session said and what it called.
func assistantLines(blocks []transcriptBlock) []string {
	var out []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, prefixed(sayMark, b.Text, 0)...)
		case "thinking":
			// The thought itself is long, private and not what a person
			// watching is looking for; that it happened is.
			out = append(out, thinkMark+"thinking")
		case "tool_use":
			out = append(out, sayMark+b.Name+"("+toolSummary(b.Name, b.Input)+")")
		}
	}
	return out
}

// userLines renders how a tool answered, and a user turn when there is one.
// A headless session's user messages are tool results; the turn a person
// typed appears only in a session started with --input-format stream-json.
func userLines(blocks []transcriptBlock) []string {
	var out []string
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			out = append(out, resultMark+toolResult(b))
		case "text":
			out = append(out, prefixed(userMark, b.Text, 3)...)
		}
	}
	return out
}

// resultLine renders the final event of the stream: the session is over,
// and this is what it cost.
func resultLine(e transcriptEntry) string {
	how := "ok"
	if e.IsError || (e.Subtype != "" && e.Subtype != "success") {
		how = "failed"
		if e.Subtype != "" && e.Subtype != "error" {
			how = e.Subtype
		}
	}
	return fmt.Sprintf("%ssession ended: %s, %s, $%.2f",
		sayMark, how, text.Count(e.NumTurns, "turn"), e.TotalCostUSD)
}

// toolArg names the field of a tool's input that says what the call is
// about, so a call reads as `Bash(git status)` rather than as its JSON. A
// tool that is not listed — every MCP tool, including bees' own — falls
// back to its string arguments, which is what makes the list a convenience
// rather than something that has to be kept complete.
var toolArg = map[string]string{
	"Bash":         "command",
	"Read":         "file_path",
	"Write":        "file_path",
	"Edit":         "file_path",
	"NotebookEdit": "notebook_path",
	"Glob":         "pattern",
	"Grep":         "pattern",
	"WebFetch":     "url",
	"WebSearch":    "query",
	"Task":         "description",
	"Skill":        "skill",
}

// toolSummary renders a tool call's input as one line.
func toolSummary(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return oneLine(string(input))
	}
	if k, ok := toolArg[name]; ok {
		if v, ok := m[k].(string); ok {
			return oneLine(v)
		}
	}
	var args []string
	for k, v := range m {
		switch v := v.(type) {
		case string:
			if v != "" {
				args = append(args, k+"="+oneLine(v))
			}
		case bool, float64:
			args = append(args, fmt.Sprintf("%s=%v", k, v))
		}
	}
	slices.Sort(args)
	if len(args) == 0 {
		// A call whose arguments are all objects or lists: its JSON says
		// more than nothing at all.
		return oneLine(string(input))
	}
	return strings.Join(args, ", ")
}

// toolResult renders how a tool answered: its first line, and how many more
// there were. A reader who wants the rest reads the transcript file.
func toolResult(b transcriptBlock) string {
	body := ""
	var blocks []transcriptBlock
	switch {
	case json.Unmarshal(b.Content, &blocks) == nil:
		var parts []string
		for _, c := range blocks {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		body = strings.Join(parts, "\n")
	default:
		_ = json.Unmarshal(b.Content, &body)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		body = "(no output)"
	}
	lines := strings.Split(body, "\n")
	out := strings.TrimSpace(lines[0])
	if n := len(lines) - 1; n > 0 {
		out += fmt.Sprintf(" (+%s)", text.Count(n, "line"))
	}
	if b.IsError {
		out = "error: " + out
	}
	return out
}

// prefixed splits a block of text into lines, marks the first and indents
// the rest under it. A zero max keeps every line; a positive one keeps that
// many and says how many were left behind, which is how a user turn long
// enough to be a whole task prompt stays one entry in the view.
func prefixed(mark, s string, max int) []string {
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	dropped := 0
	if max > 0 && len(lines) > max {
		dropped, lines = len(lines)-max, lines[:max]
	}
	indent := strings.Repeat(" ", len([]rune(mark)))
	out := make([]string, 0, len(lines)+1)
	for i, l := range lines {
		if i == 0 {
			out = append(out, mark+l)
			continue
		}
		out = append(out, indent+l)
	}
	if dropped > 0 {
		out = append(out, indent+fmt.Sprintf("(+%s)", text.Count(dropped, "line")))
	}
	return out
}
