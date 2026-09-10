// Package nams is bees' client for the Neo4j Agent Memory REST API — the
// contract Neo4j's own SDKs speak to the hosted service
// (https://memory.neo4jlabs.com/v1) and to a self-hosted deployment of it —
// narrowed to what a role's notes need: read one text, write one text back.
// It is the backend behind notes_read and notes_write with
// notes.backend = "neo4j"; nothing else in bees talks to the service, and
// bees neither runs it nor embeds a Neo4j driver.
//
// A role's notes are one conversation per role, found by its userId
// ("bees-notes-<role>"), whose messages are the successive versions of the
// notes: a write appends the whole text as a message, a read returns the
// newest message's content. The service extracts entities from messages as
// they arrive, so the history is what the graph is built from; nothing here
// deletes anything. A role that never wrote reads the same section skeleton
// the file backend starts a role with, and no conversation is created until
// its first write.
package nams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/state"
)

// Notes reads and replaces a role's notes through the service. Its methods
// are the shape internal/mcpserver's Notes interface asks for.
type Notes struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewNotes returns a Notes backend for the service at baseURL — the REST
// base including its version segment, as notes.neo4j_url gives it —
// authenticating with apiKey as a bearer token, the way the SDKs do.
func NewNotes(baseURL, apiKey string) *Notes {
	return &Notes{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: requestTimeout},
	}
}

const (
	// userIDPrefix, followed by the role, is the userId the role's
	// conversation is created with and found by.
	userIDPrefix = "bees-notes-"
	// messageRole is the role every version of the notes is added as: the
	// notes are the agent's own words.
	messageRole = "assistant"
	// pageSize is the explicit limit every list request carries. The
	// service's default page size is its own, so a list without one could
	// miss the newest version.
	pageSize = 100
	// maxPages bounds a listing against a service that ignores offset.
	maxPages = 1000
	// requestTimeout is the SDKs' default.
	requestTimeout = 30 * time.Second
)

func userID(role string) string { return userIDPrefix + role }

// ReadNotes returns the newest version of the role's notes, or the section
// skeleton when the role has never written any.
func (n *Notes) ReadNotes(ctx context.Context, role string) (string, error) {
	conv, err := n.conversation(ctx, role)
	if err != nil {
		return "", err
	}
	if conv == "" {
		return state.NotesSkeleton(role), nil
	}
	msgs, err := n.messages(ctx, conv)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return state.NotesSkeleton(role), nil
	}
	return newest(msgs).Content, nil
}

// WriteNotes appends text as the newest version of the role's notes,
// creating the role's conversation on its first write.
func (n *Notes) WriteNotes(ctx context.Context, role, text string) error {
	conv, err := n.conversation(ctx, role)
	if err != nil {
		return err
	}
	if conv == "" {
		var created conversation
		body := map[string]any{"userId": userID(role), "metadata": map[string]string{"bees": "notes", "role": role}}
		if err := n.do(ctx, http.MethodPost, "/conversations", nil, body, &created); err != nil {
			return err
		}
		if created.ID == "" {
			return errors.New("neo4j agent memory: POST /conversations answered without an id")
		}
		conv = created.ID
	}
	body := map[string]string{"role": messageRole, "content": text}
	return n.do(ctx, http.MethodPost, "/conversations/"+url.PathEscape(conv)+"/messages", nil, body, nil)
}

// Size is the byte length of the newest version of the role's notes. The
// REST API has no size query, so this reads the notes and measures them:
// one round trip, the same a notes_read costs.
func (n *Notes) Size(ctx context.Context, role string) (int64, error) {
	notes, err := n.ReadNotes(ctx, role)
	if err != nil {
		return 0, err
	}
	return int64(len(notes)), nil
}

// conversation is the shape of a conversation the service lists, and of a
// created one; the fields not read here are dropped.
type conversation struct {
	ID        string `json:"id"`
	UserID    string `json:"userId"`
	CreatedAt string `json:"createdAt"`
}

// message is one message of a conversation as the service lists it.
type message struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"createdAt"`
}

// conversation returns the id of the role's conversation, or "" when there
// is none. The list is asked for the role's userId and checked again here,
// so a service that ignores the filter still yields the right one. Two
// sessions of one role starting at once can each create a conversation
// before seeing the other's; every later read and write then converges on
// the oldest, so the role's memory has one home.
func (n *Notes) conversation(ctx context.Context, role string) (string, error) {
	uid := userID(role)
	var mine []conversation
	err := n.list(ctx, "/conversations", url.Values{"userId": {uid}}, "conversations", func(raw json.RawMessage) error {
		var c conversation
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		if c.UserID == uid {
			mine = append(mine, c)
		}
		return nil
	})
	if err != nil || len(mine) == 0 {
		return "", err
	}
	return oldest(mine).ID, nil
}

// messages returns every message of a conversation.
func (n *Notes) messages(ctx context.Context, conv string) ([]message, error) {
	var msgs []message
	err := n.list(ctx, "/conversations/"+url.PathEscape(conv)+"/messages", nil, "messages", func(raw json.RawMessage) error {
		var m message
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		msgs = append(msgs, m)
		return nil
	})
	return msgs, err
}

// list pages through a list endpoint with an explicit limit and offset,
// calling each for every item, until a page comes back short. The service
// wraps a list under key ("conversations", "messages"); a bare array is
// accepted too, as the SDKs accept it.
func (n *Notes) list(ctx context.Context, path string, query url.Values, key string, each func(json.RawMessage) error) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("limit", strconv.Itoa(pageSize))
	for page := 0; page < maxPages; page++ {
		query.Set("offset", strconv.Itoa(page*pageSize))
		var raw json.RawMessage
		if err := n.do(ctx, http.MethodGet, path, query, nil, &raw); err != nil {
			return err
		}
		items, err := unwrap(raw, key)
		if err != nil {
			return fmt.Errorf("neo4j agent memory: GET %s: %w", path, err)
		}
		for _, item := range items {
			if err := each(item); err != nil {
				return fmt.Errorf("neo4j agent memory: GET %s: %w", path, err)
			}
		}
		if len(items) < pageSize {
			return nil
		}
	}
	return fmt.Errorf("neo4j agent memory: GET %s: more than %d pages of %d, giving up", path, maxPages, pageSize)
}

// unwrap returns the items of a list response: the array under key, or the
// response itself when it is a bare array. An empty body is an empty list.
func unwrap(raw json.RawMessage, key string) ([]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var items []json.RawMessage
	if raw[0] == '[' {
		return items, json.Unmarshal(raw, &items)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	inner, ok := envelope[key]
	if !ok {
		return nil, fmt.Errorf("response has no %q", key)
	}
	return items, json.Unmarshal(inner, &items)
}

// newest is the message with the latest createdAt; among equal timestamps,
// and among messages without one, the later in the list. The timestamps
// come from one service in one format, so they compare as strings.
func newest(msgs []message) message {
	best := msgs[0]
	for _, m := range msgs[1:] {
		if m.CreatedAt >= best.CreatedAt {
			best = m
		}
	}
	return best
}

// oldest is the conversation with the earliest createdAt; a conversation
// without one loses to any that has one, and among equals the first listed
// wins.
func oldest(convs []conversation) conversation {
	best := convs[0]
	for _, c := range convs[1:] {
		if best.CreatedAt == "" || (c.CreatedAt != "" && c.CreatedAt < best.CreatedAt) {
			best = c
		}
	}
	return best
}

// do sends one request and decodes a 2xx JSON response into out (skipped
// when out is nil or the body is empty, as it is for a 204). A 401 or 403
// names the key to check; any other failure carries the service's own
// message, which it sends as {"error": "..."}.
func (n *Notes) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := n.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("neo4j agent memory: %s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("neo4j agent memory: %s %s: reading the response: %w", method, path, err)
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return fmt.Errorf("neo4j agent memory: %s %s: HTTP %d, the service refused the API key: check notes.neo4j_api_key in bees.toml (%s)", method, path, res.StatusCode, serviceMessage(data))
	case res.StatusCode < 200 || res.StatusCode > 299:
		return fmt.Errorf("neo4j agent memory: %s %s: HTTP %d: %s", method, path, res.StatusCode, serviceMessage(data))
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("neo4j agent memory: %s %s: decoding the response: %w", method, path, err)
	}
	return nil
}

// serviceMessage is the "error" field of an error body, falling back to the
// body itself, trimmed, and to the bare status when there is none.
func serviceMessage(data []byte) string {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &e) == nil {
		switch {
		case e.Error != "" && e.Message != "" && e.Error != e.Message:
			return e.Error + ": " + e.Message
		case e.Error != "":
			return e.Error
		case e.Message != "":
			return e.Message
		}
	}
	if s := strings.TrimSpace(string(data)); s != "" {
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		return s
	}
	return "no error message in the response"
}
