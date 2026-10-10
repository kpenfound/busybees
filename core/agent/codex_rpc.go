package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"
)

// codexRPCClientInfo is the client half of codex app-server's initialize
// handshake.
var codexRPCClientInfo = map[string]any{"clientInfo": map[string]string{"name": "bees", "version": "0"}}

// codexRPCTurn is what one codex app-server turn needs from its caller: the
// turn's kind — Restricted and the built-in tools a held turn is granted,
// which together with the sandbox/approval table decide thread/start's and
// thread/resume's sandbox — and the task itself. ResumeID, when set, turns
// thread/start into thread/resume. ConfigDir, when set, asks config/read
// for that working directory before the thread starts; codexRPCRun returns
// its answer unexamined, in codexRPCOutcome.Config, for a later unit to
// check against the held settings the turn was given.
type codexRPCTurn struct {
	Cwd          string
	Model        string
	SystemPrompt string
	Prompt       string
	Restricted   bool
	// Tools are the turn's granted built-in tools: nil for an ordinary
	// turn or one granted ToolsAll, the tools a held turn is granted
	// otherwise.
	Tools    []string
	ResumeID string
	// ConfigDir, set for a held turn, asks config/read for that working
	// directory before the thread starts, on the same process the turn
	// runs on; codexRPCRun checks the answer with validateCodexSettings
	// against codexHeldSettings(Tools) and ends the session as an error,
	// before any thread/start, when it fails.
	ConfigDir string
}

// codexRPCOutcome is what one codex app-server conversation produced, in
// streamEnd's terms, plus config/read's answer, unexamined, when
// codexRPCTurn.ConfigDir asked for one, and the account's rate-limit
// snapshot from the conversation's last account/rateLimits/updated
// notification, nil when none arrived.
type codexRPCOutcome struct {
	streamEnd
	Config    json.RawMessage
	RateLimit *RateLimit
}

// codexRPCParams are the parameters thread/start and thread/resume carry.
// ephemeral and approvalsReviewer have no field here: this conversation
// never sends either.
type codexRPCParams struct {
	Cwd                   string `json:"cwd"`
	Model                 string `json:"model,omitempty"`
	DeveloperInstructions string `json:"developerInstructions"`
	ApprovalPolicy        string `json:"approvalPolicy"`
	Sandbox               string `json:"sandbox"`
}

// codexRPCInputItem is one item of turn/start's params.input array. The
// app-server protocol expects input as a sequence of typed items, not a
// bare string; bees sends exactly one "text" item per turn, carrying the
// turn's prompt verbatim.
type codexRPCInputItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// codexThreadStartParams builds thread/start's and thread/resume's
// parameters from a turn's kind alone, so both requests always carry
// exactly the same ones: cwd, model only when the profile named one,
// developerInstructions holding the system prompt, approvalPolicy
// "never", and sandbox from the sandbox/approval table — danger-full-access
// for an ordinary turn, one granted ToolsAll (tools nil) or one granted
// apply_patch, and read-only for a restricted turn and for a held turn
// (tools non-nil) without apply_patch.
func codexThreadStartParams(cwd, model, systemPrompt string, restricted bool, tools []string) codexRPCParams {
	sandbox := "danger-full-access"
	if restricted || (tools != nil && !slices.Contains(tools, "apply_patch")) {
		sandbox = "read-only"
	}
	return codexRPCParams{
		Cwd:                   cwd,
		Model:                 model,
		DeveloperInstructions: systemPrompt,
		ApprovalPolicy:        "never",
		Sandbox:               sandbox,
	}
}

// codexThreadMethod picks thread/start or thread/resume and builds its
// parameters: thread/resume carries the same params as thread/start, with
// threadId naming the thread to continue.
func codexThreadMethod(resumeID string, params codexRPCParams) (string, any) {
	if resumeID == "" {
		return "thread/start", params
	}
	return "thread/resume", struct {
		codexRPCParams
		ThreadID string `json:"threadId"`
	}{params, resumeID}
}

// codexRPCError is a JSON-RPC error object, sent and received.
type codexRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// codexRPCLine is one line of the conversation, read generically: a
// request (Method and ID set), a notification (Method set, ID empty) or a
// response to one of the driver's own requests (ID set, Method empty).
type codexRPCLine struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *codexRPCError  `json:"error"`
}

func (l codexRPCLine) hasID() bool {
	return len(l.ID) > 0 && string(l.ID) != "null"
}

// codexUnsupportedRequest answers one request the app server sent: a
// JSON-RPC error saying bees does not support it, whatever it asks, so
// none is ever left pending. answer special-cases
// mcpServer/elicitation/request with a cancelling result instead.
func codexUnsupportedRequest(method string) *codexRPCError {
	return &codexRPCError{Code: -32601, Message: fmt.Sprintf("%s is not supported in a bees session", method)}
}

// codexRateLimitWindow is one usage window ("primary" or "secondary") of
// an account/rateLimits/updated notification.
type codexRateLimitWindow struct {
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    int64   `json:"resetsAt"`
}

// codexRateLimitParams are account/rateLimits/updated's parameters. A nil
// RateLimitReachedType means the account is not rate-limited; set, its
// value becomes the RateLimit's Status and the window at 100% UsedPercent
// names its Type and ResetsAt.
type codexRateLimitParams struct {
	RateLimitReachedType *string               `json:"rateLimitReachedType"`
	Primary              *codexRateLimitWindow `json:"primary"`
	Secondary            *codexRateLimitWindow `json:"secondary"`
}

// codexRateLimit builds the RateLimit an account/rateLimits/updated
// notification reports: the allowed snapshot when rateLimitReachedType is
// null, otherwise a blocking one whose Status is rateLimitReachedType's
// value and whose Type and ResetsAt name whichever window — primary or
// secondary — is at 100% usedPercent. nil when params does not parse.
func codexRateLimit(params json.RawMessage) *RateLimit {
	var p codexRateLimitParams
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	if p.RateLimitReachedType == nil {
		return &RateLimit{Status: "allowed"}
	}
	rl := &RateLimit{Status: *p.RateLimitReachedType}
	switch {
	case p.Primary != nil && p.Primary.UsedPercent == 100:
		rl.Type = "primary"
		rl.ResetsAt = time.Unix(p.Primary.ResetsAt, 0)
	case p.Secondary != nil && p.Secondary.UsedPercent == 100:
		rl.Type = "secondary"
		rl.ResetsAt = time.Unix(p.Secondary.ResetsAt, 0)
	}
	return rl
}

// codexRateLimitMethod is the notification a codexConversation folds into
// its running rateLimit.
const codexRateLimitMethod = "account/rateLimits/updated"

// recordRateLimit replaces the conversation's rateLimit with the one
// account/rateLimits/updated's params build, the latest notification
// always winning over an earlier one.
func (c *codexConversation) recordRateLimit(params json.RawMessage) {
	if rl := codexRateLimit(params); rl != nil {
		c.rateLimit = rl
	}
}

// codexElicitationMethod is the one server request answered with a result
// rather than an error: bees never approves anything, and the server
// expects either a cancel or a decline for this one, not an error.
const codexElicitationMethod = "mcpServer/elicitation/request"

// codexAppServerEnded is the one error a codex app-server conversation ends
// with when the server exits or closes its stdout before answering method:
// the deterministic message every path to that outcome returns, whether it
// shows up as a failed write to its stdin or a read that found stdout
// already at its end.
func codexAppServerEnded(method string) error {
	return fmt.Errorf("the app server ended without answering %s", method)
}

// codexConversation drives one codex app-server process over its stdin and
// stdout: line framing, request ids, answering the server's own requests
// and folding its notifications into a running turn count, last agent
// message and turn status.
type codexConversation struct {
	stdin      io.Writer
	scanner    *bufio.Scanner
	transcript io.Writer
	nextID     int

	threadID string
	// turnID is turn/start's answer: the id turn/interrupt names alongside
	// threadID, learned before awaitTurnCompleted is ever called.
	turnID      string
	turns       int
	lastMessage string
	// status is turn/completed's status once the turn has ended: empty
	// until then.
	status string
	// rateLimit is the RateLimit built from the latest
	// account/rateLimits/updated notification seen so far, nil until one
	// arrives.
	rateLimit *RateLimit
}

func newCodexConversation(stdin io.Writer, stdout io.Reader, transcript io.Writer) *codexConversation {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	return &codexConversation{stdin: stdin, scanner: sc, transcript: transcript, nextID: 1}
}

// writeTranscript records one line the conversation read or wrote, exactly
// as tee does for every other backend's stream.
func (c *codexConversation) writeTranscript(line []byte) {
	_, _ = c.transcript.Write(line)
	_, _ = c.transcript.Write([]byte{'\n'})
}

// readLine reads and records the next line of the server's output, false
// at its end.
func (c *codexConversation) readLine() ([]byte, bool) {
	if !c.scanner.Scan() {
		return nil, false
	}
	line := c.scanner.Bytes()
	c.writeTranscript(line)
	return line, true
}

// send marshals and records v, then writes it to the server's stdin.
func (c *codexConversation) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeTranscript(data)
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

// notify sends a notification: no id, and no response to wait for.
func (c *codexConversation) notify(method string, params any) error {
	v := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		v["params"] = params
	}
	return c.send(v)
}

// call sends a request and returns its result, reading and answering every
// server request and folding every notification into the conversation's
// state until the matching response arrives. A server that exits or closes
// its stdout first ends the call with codexAppServerEnded(method), whether
// that showed up as a failed write here or as a read failure in await.
func (c *codexConversation) call(method string, params any) (json.RawMessage, error) {
	id := c.nextID
	c.nextID++
	v := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		v["params"] = params
	}
	if err := c.send(v); err != nil {
		return nil, codexAppServerEnded(method)
	}
	return c.await(id, method)
}

// await reads lines until the response to id arrives, answering every
// server request it sees along the way (including one that arrives before
// that response, so none is ever left pending) and ending the call on an
// "error" notification. method names the pending request in the error a
// closed or exited server yields.
func (c *codexConversation) await(id int, method string) (json.RawMessage, error) {
	wantID := strconv.Itoa(id)
	for {
		line, ok := c.readLine()
		if !ok {
			return nil, codexAppServerEnded(method)
		}
		var l codexRPCLine
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		switch {
		case l.hasID() && l.Method != "":
			if err := c.answer(l); err != nil {
				return nil, codexAppServerEnded(method)
			}
		case l.Method == "error":
			return nil, codexServerError(l.Params)
		case l.Method == codexRateLimitMethod:
			c.recordRateLimit(l.Params)
		case l.Method != "":
			// A notification this phase does not otherwise act on
			// (thread/tokenUsage/updated, …): already recorded in the
			// transcript by readLine.
		case string(l.ID) == wantID:
			if l.Error != nil {
				return nil, fmt.Errorf("%s: %s", method, l.Error.Message)
			}
			return l.Result, nil
		}
	}
}

// answer responds to one request the server sent: a cancelling result for
// mcpServer/elicitation/request, codexUnsupportedRequest's error for every
// other method, known or unknown. Nothing approves anything.
func (c *codexConversation) answer(l codexRPCLine) error {
	v := map[string]any{"jsonrpc": "2.0", "id": l.ID}
	if l.Method == codexElicitationMethod {
		v["result"] = map[string]any{"action": "cancel"}
	} else {
		v["error"] = codexUnsupportedRequest(l.Method)
	}
	return c.send(v)
}

// codexServerError reads an "error" notification's message.
func codexServerError(params json.RawMessage) error {
	var p struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(params, &p)
	if p.Message == "" {
		return errors.New("codex app server reported an error")
	}
	return errors.New(p.Message)
}

// awaitTurnCompleted reads notifications until turn/completed, answering
// every server request along the way: item/completed counts a turn and
// keeps its agent message as the result text, turn/completed records the
// turn's status and ends the loop, and an "error" notification ends it
// with an error. A server that exits or closes its stdout first yields the
// same codexAppServerEnded error a request answered by neither a write nor
// a read would.
//
// stop, when not nil, is the runner's own signal that the session was
// cancelled, timed out, or capped: closing it sends turn/interrupt once,
// ahead of the runner's process-group kill, and the loop goes on reading
// exactly as before, because the interrupted turn still ends through its
// own turn/completed, an "error" notification, or the server closing its
// stdout — whichever arrives first, within the runner's own WaitDelay or
// not. A dedicated goroutine does the actual reading, so this loop's
// select can watch stop without blocking on it.
func (c *codexConversation) awaitTurnCompleted(stop <-chan struct{}) error {
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		for c.scanner.Scan() {
			lines <- append([]byte(nil), c.scanner.Bytes()...)
		}
	}()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return codexAppServerEnded("turn/completed")
			}
			c.writeTranscript(line)
			var l codexRPCLine
			if json.Unmarshal(line, &l) != nil {
				continue
			}
			if l.hasID() && l.Method != "" {
				if err := c.answer(l); err != nil {
					return codexAppServerEnded("turn/completed")
				}
				continue
			}
			switch l.Method {
			case "item/completed":
				c.recordItem(l.Params)
			case "turn/completed":
				return c.recordTurnCompleted(l.Params)
			case codexRateLimitMethod:
				c.recordRateLimit(l.Params)
			case "error":
				return codexServerError(l.Params)
			}
		case <-stop:
			stop = nil // turn/interrupt is sent at most once
			_ = c.sendInterrupt()
		}
	}
}

// sendInterrupt writes turn/interrupt, carrying the thread and turn ids
// this conversation learned from thread/start's and turn/start's own
// answers. Its response, if the server ever sends one, carries no method
// and matches no id this conversation awaits, so the loop above reads and
// ignores it exactly as it does any other response to a request bees does
// not correlate: what ends the turn is turn/completed, not this answer.
func (c *codexConversation) sendInterrupt() error {
	id := c.nextID
	c.nextID++
	return c.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "turn/interrupt",
		"params":  map[string]any{"threadId": c.threadID, "turnId": c.turnID},
	})
}

// recordItem counts one item/completed notification as a turn, and keeps
// its text as the result when it is a non-empty agent message.
func (c *codexConversation) recordItem(params json.RawMessage) {
	c.turns++
	var p struct {
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if json.Unmarshal(params, &p) == nil && p.Item.Type == "agent_message" && p.Item.Text != "" {
		c.lastMessage = p.Item.Text
	}
}

// recordTurnCompleted reads turn/completed's status: "completed" ends the
// turn without error, anything else — a status codex gave, or none at all
// — is a failed turn.
func (c *codexConversation) recordTurnCompleted(params json.RawMessage) error {
	var p struct {
		Turn struct {
			Status string `json:"status"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(params, &p)
	c.status = p.Turn.Status
	if c.status != "completed" {
		status := c.status
		if status == "" {
			status = "failed"
		}
		return fmt.Errorf("turn ended with status %q", status)
	}
	return nil
}

// codexRPCRun drives one codex app-server conversation to its end:
// initialize, the initialized notification, an optional config/read whose
// answer is checked against the held settings a held turn was given,
// thread/start or thread/resume, turn/start, then notifications read and
// answered until turn/completed, after which stdin is closed so the
// server exits. stdin and stdout are the session's; every line the server
// writes and every request and response the driver sends goes to
// transcript. The outcome's RateLimit is the account/rateLimits/updated
// notification last seen, nil when none arrived. stop is the runner's own
// interrupt signal; see awaitTurnCompleted.
func codexRPCRun(stdin io.WriteCloser, stdout io.Reader, transcript io.Writer, turn codexRPCTurn, stop <-chan struct{}) (*codexRPCOutcome, error) {
	defer func() { _ = stdin.Close() }()
	c := newCodexConversation(stdin, stdout, transcript)

	if _, err := c.call("initialize", codexRPCClientInfo); err != nil {
		return nil, err
	}
	if err := c.notify("initialized", nil); err != nil {
		return nil, codexAppServerEnded("initialized")
	}

	var config json.RawMessage
	if turn.ConfigDir != "" {
		result, err := c.call("config/read", map[string]any{"cwd": turn.ConfigDir})
		if err != nil {
			return nil, err
		}
		config = result
		if turn.Tools != nil {
			var cfg codexEffectiveConfig
			if err := json.Unmarshal(result, &cfg); err != nil {
				return nil, fmt.Errorf("decode effective configuration: %w", err)
			}
			if cfg.Config == nil || cfg.Origins == nil {
				return nil, errors.New("decode effective configuration: no config or origins")
			}
			if err := validateCodexSettings(cfg, codexHeldSettings(turn.Tools)); err != nil {
				return nil, err
			}
		}
	}

	params := codexThreadStartParams(turn.Cwd, turn.Model, turn.SystemPrompt, turn.Restricted, turn.Tools)
	method, threadParams := codexThreadMethod(turn.ResumeID, params)
	result, err := c.call(method, threadParams)
	if err != nil {
		return nil, err
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &started); err != nil {
		return nil, fmt.Errorf("%s: decode response: %w", method, err)
	}
	c.threadID = started.Thread.ID

	result, err = c.call("turn/start", map[string]any{
		"threadId": c.threadID,
		"input":    []codexRPCInputItem{{Type: "text", Text: turn.Prompt}},
	})
	if err != nil {
		return nil, err
	}
	var startedTurn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(result, &startedTurn) == nil {
		c.turnID = startedTurn.Turn.ID
	}

	if err := c.awaitTurnCompleted(stop); err != nil {
		return nil, err
	}

	subtype := c.status
	if subtype == "completed" {
		subtype = "success"
	}
	return &codexRPCOutcome{streamEnd: streamEnd{
		SessionID: c.threadID,
		Result:    c.lastMessage,
		Subtype:   subtype,
		NumTurns:  c.turns,
	}, Config: config, RateLimit: c.rateLimit}, nil
}
