package nams

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/busybees/internal/state"
)

// fakeNAMS stands in for the Neo4j Agent Memory service: the conversation
// and message endpoints bees uses, under /v1, behind a bearer key, with the
// service's list envelopes, offset pagination at a small default page size
// (so a client that does not ask for a page size of its own misses items)
// and 202 on a message write, the way the service accepts one.
type fakeNAMS struct {
	mu            sync.Mutex
	key           string
	conversations []conversation
	messages      map[string][]message
	calls         []string // "METHOD /path?query"
	contentTypes  []string // of every request with a body
	// ignoreUserFilter answers a conversation list with every conversation,
	// whatever userId the query asked for.
	ignoreUserFilter bool
	// noTimestamps leaves createdAt off every listed message.
	noTimestamps bool
	// failWith, when set, is answered to every request; failAfter, when
	// set, lets that many requests through first and answers 503 to the rest.
	failWith  int
	failAfter int
	clock     int
	url       string
}

const defaultPage = 20

func newFake(t *testing.T) (*fakeNAMS, *Notes) {
	t.Helper()
	f := &fakeNAMS{key: "nams_test", messages: map[string][]message{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.url = srv.URL + "/v1"
	return f, NewNotes(f.url, "nams_test")
}

func (f *fakeNAMS) now() string {
	f.clock++
	return fmt.Sprintf("2026-09-09T12:%02d:%02dZ", f.clock/60, f.clock%60)
}

func (f *fakeNAMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		call += "?" + r.URL.RawQuery
	}
	f.calls = append(f.calls, call)
	if r.Body != nil && r.ContentLength != 0 {
		f.contentTypes = append(f.contentTypes, r.Header.Get("Content-Type"))
	}
	w.Header().Set("Content-Type", "application/json")
	if f.failWith != 0 {
		w.WriteHeader(f.failWith)
		fmt.Fprintf(w, `{"error":"injected failure %d"}`, f.failWith)
		return
	}
	switch {
	case f.failAfter > 0:
		f.failAfter--
		if f.failAfter == 0 {
			f.failAfter = -1
		}
	case f.failAfter < 0:
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"injected failure 503"}`)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.key {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid API key"}`)
		return
	}
	limit, offset := defaultPage, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, _ = strconv.Atoi(v)
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	switch {
	case r.Method == http.MethodGet && path == "/conversations":
		var out []conversation
		for _, c := range f.conversations {
			if f.ignoreUserFilter || r.URL.Query().Get("userId") == "" || c.UserID == r.URL.Query().Get("userId") {
				out = append(out, c)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"conversations": page(out, limit, offset)})
	case r.Method == http.MethodPost && path == "/conversations":
		var in struct {
			UserID   string            `json:"userId"`
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}
		c := conversation{ID: "conv-" + strconv.Itoa(len(f.conversations)+1), UserID: in.UserID, CreatedAt: f.now()}
		f.conversations = append(f.conversations, c)
		w.WriteHeader(http.StatusCreated)
		// Create answers without createdAt, as the service does.
		_ = json.NewEncoder(w).Encode(map[string]string{"id": c.ID, "userId": c.UserID, "workspaceId": "ws"})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/conversations/") && strings.HasSuffix(path, "/messages"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/conversations/"), "/messages")
		msgs := page(f.messages[id], limit, offset)
		if f.noTimestamps {
			for i := range msgs {
				msgs[i].CreatedAt = ""
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": msgs})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/conversations/") && strings.HasSuffix(path, "/messages"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/conversations/"), "/messages")
		if !f.has(id) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"conversation not found"}`)
			return
		}
		var in struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Role == "" || in.Content == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"content and role are required"}`)
			return
		}
		m := message{ID: fmt.Sprintf("%s-m%d", id, len(f.messages[id])+1), Role: in.Role, Content: in.Content, CreatedAt: f.now()}
		f.messages[id] = append(f.messages[id], m)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": m.ID, "conversationId": id, "role": m.Role, "content": m.Content})
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found"}`)
	}
}

func (f *fakeNAMS) has(id string) bool {
	for _, c := range f.conversations {
		if c.ID == id {
			return true
		}
	}
	return false
}

func page[T any](items []T, limit, offset int) []T {
	if offset >= len(items) {
		return []T{}
	}
	items = items[offset:]
	if len(items) > limit {
		items = items[:limit]
	}
	return append([]T{}, items...)
}

// seed plants a conversation for a role holding versions, oldest first,
// as if earlier sessions had written them.
func (f *fakeNAMS) seed(role string, versions ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := conversation{ID: "conv-" + strconv.Itoa(len(f.conversations)+1), UserID: userID(role), CreatedAt: f.now()}
	f.conversations = append(f.conversations, c)
	for i, v := range versions {
		f.messages[c.ID] = append(f.messages[c.ID], message{ID: fmt.Sprintf("%s-m%d", c.ID, i+1), Role: messageRole, Content: v, CreatedAt: f.now()})
	}
	return c.ID
}

func (f *fakeNAMS) posts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "POST ") {
			out = append(out, c)
		}
	}
	return out
}

// A role that never wrote reads the same skeleton the file backend starts
// it with, and the read creates nothing: reading is not a write.
func TestFirstReadIsTheSkeletonAndCreatesNothing(t *testing.T) {
	f, n := newFake(t)
	got, err := n.ReadNotes(context.Background(), "developer")
	if err != nil {
		t.Fatal(err)
	}
	if got != state.NotesSkeleton("developer") {
		t.Errorf("first read = %q, want the skeleton", got)
	}
	if posts := f.posts(); len(posts) != 0 || len(f.conversations) != 0 {
		t.Errorf("a read wrote: %v %v", posts, f.conversations)
	}
	if len(f.calls) != 1 || f.calls[0] != "GET /v1/conversations?limit=100&offset=0&userId=bees-notes-developer" {
		t.Errorf("calls = %v", f.calls)
	}
}

// A write creates the role's conversation once and appends the text as a
// message of the agent's own; a read returns it; the next write appends
// again and the read returns the newest, the older versions staying where
// the service extracted them from. Each role has a conversation of its own.
func TestWritesAppendVersionsAndReadsReturnTheNewest(t *testing.T) {
	f, n := newFake(t)
	ctx := context.Background()
	const v1 = "# developer notes\n\n## Project facts\n\n- run dagger check\n"
	if err := n.WriteNotes(ctx, "developer", v1); err != nil {
		t.Fatal(err)
	}
	if got, err := n.ReadNotes(ctx, "developer"); err != nil || got != v1 {
		t.Errorf("read after the first write = %q, %v", got, err)
	}
	const v2 = "# developer notes\n\n## Project facts\n\n- run dagger check\n- gofmt too\n"
	if err := n.WriteNotes(ctx, "developer", v2); err != nil {
		t.Fatal(err)
	}
	if got, err := n.ReadNotes(ctx, "developer"); err != nil || got != v2 {
		t.Errorf("read after the second write = %q, %v", got, err)
	}
	if err := n.WriteNotes(ctx, "qa", "# qa notes\n"); err != nil {
		t.Fatal(err)
	}
	if got, _ := n.ReadNotes(ctx, "qa"); got != "# qa notes\n" {
		t.Errorf("qa reads %q", got)
	}
	if got, _ := n.ReadNotes(ctx, "developer"); got != v2 {
		t.Errorf("developer reads %q after qa wrote", got)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.conversations) != 2 || f.conversations[0].UserID != "bees-notes-developer" || f.conversations[1].UserID != "bees-notes-qa" {
		t.Fatalf("conversations = %+v", f.conversations)
	}
	dev := f.messages[f.conversations[0].ID]
	if len(dev) != 2 || dev[0].Content != v1 || dev[1].Content != v2 || dev[0].Role != "assistant" {
		t.Errorf("developer messages = %+v", dev)
	}
	for _, ct := range f.contentTypes {
		if ct != "application/json" {
			t.Errorf("a request with a body was sent as %q", ct)
		}
	}
	if len(f.contentTypes) != 5 {
		t.Errorf("%d requests with a body, want 5 (2 creates, 3 writes): %v", len(f.contentTypes), f.calls)
	}
}

// The newest version is the message with the latest createdAt, wherever
// the service lists it, and the listing asks for pages of its own size:
// with 150 versions and the newest in the middle of the second page, a
// client that took the service's default page, or the last item, would
// answer a stale version.
func TestReadPagesThroughAndPicksByTimestamp(t *testing.T) {
	f, n := newFake(t)
	versions := make([]string, 150)
	for i := range versions {
		versions[i] = "version " + strconv.Itoa(i+1)
	}
	id := f.seed("reviewer", versions...)
	f.mu.Lock()
	f.messages[id][120].CreatedAt = "2026-12-31T00:00:00Z" // the newest, listed 121st of 150
	f.mu.Unlock()

	got, err := n.ReadNotes(context.Background(), "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if got != "version 121" {
		t.Errorf("read %q, want the version with the latest timestamp", got)
	}
	var pages []string
	for _, c := range f.calls {
		if strings.Contains(c, "/messages?") {
			pages = append(pages, c[strings.Index(c, "?"):])
		}
	}
	if want := []string{"?limit=100&offset=0", "?limit=100&offset=100"}; strings.Join(pages, " ") != strings.Join(want, " ") {
		t.Errorf("message pages requested: %v, want %v", pages, want)
	}
}

// Without timestamps the last listed message is the newest.
func TestReadWithoutTimestampsTakesTheLastListed(t *testing.T) {
	f, n := newFake(t)
	f.seed("qa", "old", "older still", "newest")
	f.noTimestamps = true
	if got, err := n.ReadNotes(context.Background(), "qa"); err != nil || got != "newest" {
		t.Errorf("read %q, %v", got, err)
	}
}

// A service that ignores the userId filter lists every role's conversation;
// the client still picks the one whose userId is the role's.
func TestConversationIsMatchedByUserIDNotByPosition(t *testing.T) {
	f, n := newFake(t)
	f.seed("developer", "developer notes")
	f.seed("reviewer", "reviewer notes")
	f.ignoreUserFilter = true
	if got, _ := n.ReadNotes(context.Background(), "reviewer"); got != "reviewer notes" {
		t.Errorf("reviewer read %q", got)
	}
	if got, _ := n.ReadNotes(context.Background(), "developer"); got != "developer notes" {
		t.Errorf("developer read %q", got)
	}
	if got, _ := n.ReadNotes(context.Background(), "qa"); got != state.NotesSkeleton("qa") {
		t.Errorf("qa read %q, want the skeleton: another role's conversation was taken", got)
	}
}

// Two sessions of one role that started at once can each have created a
// conversation; from then on every read and write uses the oldest, so the
// role's memory has one home rather than alternating between two.
func TestConcurrentCreatesConvergeOnTheOldestConversation(t *testing.T) {
	f, n := newFake(t)
	first := f.seed("developer", "from the first session")
	second := f.seed("developer", "from the second session")
	ctx := context.Background()
	if got, _ := n.ReadNotes(ctx, "developer"); got != "from the first session" {
		t.Errorf("read %q, want the oldest conversation's notes", got)
	}
	if err := n.WriteNotes(ctx, "developer", "merged"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if len(f.messages[first]) != 2 || len(f.messages[second]) != 1 || len(f.conversations) != 2 {
		t.Errorf("write went to %v", f.messages)
	}
	f.mu.Unlock()
	if got, _ := n.ReadNotes(ctx, "developer"); got != "merged" {
		t.Errorf("read back %q", got)
	}
}

// A refused key names the bees.toml key to check; any other failure carries
// the service's own message and status; a service that cannot be reached
// says so rather than returning empty notes.
func TestErrorsNameTheCause(t *testing.T) {
	ctx := context.Background()
	f, _ := newFake(t)
	wrong := NewNotes(f.url, "nams_wrong")
	_, err := wrong.ReadNotes(ctx, "developer")
	if err == nil || !strings.Contains(err.Error(), "notes.neo4j_api_key") || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("wrong key: %v", err)
	}
	if err := wrong.WriteNotes(ctx, "developer", "x"); err == nil || !strings.Contains(err.Error(), "notes.neo4j_api_key") {
		t.Errorf("wrong key on write: %v", err)
	}

	f, n := newFake(t)
	f.failWith = http.StatusInternalServerError
	if _, err := n.ReadNotes(ctx, "developer"); err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "injected failure 500") {
		t.Errorf("500: %v", err)
	}
	if err := n.WriteNotes(ctx, "developer", "x"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("500 on write: %v", err)
	}
	// A write whose lookup succeeds and whose create fails names the create.
	f.failWith = 0
	f.failAfter = 1
	if err := n.WriteNotes(ctx, "developer", "x"); err == nil || !strings.Contains(err.Error(), "POST /conversations: HTTP 503") {
		t.Errorf("503 on create: %v", err)
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := NewNotes(url+"/v1", "k").ReadNotes(ctx, "developer"); err == nil || !strings.Contains(err.Error(), "GET /conversations") {
		t.Errorf("unreachable: %v", err)
	}
}

// The base URL is taken as written, trailing slash or not, and the paths
// go under it: a self-hosted deployment need not live at the root.
func TestBaseURLIsHonoured(t *testing.T) {
	f := &fakeNAMS{key: "k", messages: map[string][]message{}}
	srv := httptest.NewServer(http.StripPrefix("/memory", f))
	t.Cleanup(srv.Close)
	for _, base := range []string{srv.URL + "/memory/v1", srv.URL + "/memory/v1/"} {
		n := NewNotes(base, "k")
		if err := n.WriteNotes(context.Background(), "qa", "under a prefix"); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if got, err := n.ReadNotes(context.Background(), "qa"); err != nil || got != "under a prefix" {
			t.Errorf("%s: read %q, %v", base, got, err)
		}
	}
	for _, c := range f.calls {
		if strings.Contains(c, "//") {
			t.Errorf("a doubled slash in %q", c)
		}
	}
}

// The list envelopes are the service's; a bare array, which the SDKs also
// accept, works too, and an empty body is an empty list.
func TestUnwrapAcceptsEnvelopeAndBareArray(t *testing.T) {
	for raw, want := range map[string]int{
		`{"messages":[{"id":"a"},{"id":"b"}]}`: 2,
		`[{"id":"a"}]`:                         1,
		`{"messages":[]}`:                      0,
		``:                                     0,
		`null`:                                 0,
	} {
		items, err := unwrap([]byte(raw), "messages")
		if err != nil || len(items) != want {
			t.Errorf("%q: %d items, %v; want %d", raw, len(items), err, want)
		}
	}
	if _, err := unwrap([]byte(`{"conversations":[]}`), "messages"); err == nil {
		t.Error("an envelope under another key was accepted")
	}
}
