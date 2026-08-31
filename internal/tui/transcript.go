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

// A session's transcript is the stream-json claude writes as it works, one
// JSON object per line, teed to transcript.jsonl by the runner. The view
// reads that file rather than the stream: it is already on disk, one line
// per event, and reading it needs nothing of the scheduler.
//
// What a person watching wants from it is what Claude Code itself shows —
// the assistant's own words, the tools it called and how each one answered —
// so everything else in the stream (the thought text, the init and
// rate-limit bookkeeping, the tool schemas) is reduced to a marker or
// dropped. A line the view cannot parse is dropped too: half a JSON object
// is what a transcript being written *right now* ends with, and it is worth
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
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// The fields below are the final "result" event's.
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
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
// since byte offset off, and returns the lines to show for it and the
// offset to continue from.
//
// Only whole lines are consumed: the runner is writing this file as the
// view reads it, so the last line is regularly half an object. Leaving it
// behind — rather than parsing what is there — is what makes the next read
// see it complete. A transcript that does not exist yet is not an error:
// the session directory is created before claude is started.
func readTranscript(dir string, off int64) (lines []string, next int64, err error) {
	if dir == "" {
		return nil, off, nil
	}
	f, err := os.Open(filepath.Join(dir, session.TranscriptFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, off, nil
		}
		return nil, off, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, off, err
	}
	next = off
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// No newline yet: the rest is a line still being written.
			break
		}
		next += int64(len(line))
		lines = append(lines, renderTranscriptLine(line)...)
	}
	return lines, next, nil
}

// renderTranscriptLine turns one stream-json line into the lines the view
// shows for it, or none at all.
func renderTranscriptLine(line []byte) []string {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var e transcriptEntry
	if json.Unmarshal(line, &e) != nil {
		return nil
	}
	switch e.Type {
	case "assistant":
		return assistantLines(blocksOf(e))
	case "user":
		return userLines(blocksOf(e))
	case "result":
		return []string{resultLine(e)}
	}
	// "system" (init, thinking-token bookkeeping, task notifications) and
	// "rate_limit_event" are the runner's business, not a reader's.
	return nil
}

// blocksOf reads a message's content, which is an array of blocks or — for
// a user turn typed as one string — a single string.
func blocksOf(e transcriptEntry) []transcriptBlock {
	var blocks []transcriptBlock
	if json.Unmarshal(e.Message.Content, &blocks) == nil {
		return blocks
	}
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil && s != "" {
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
