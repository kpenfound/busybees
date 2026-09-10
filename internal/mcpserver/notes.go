package mcpserver

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/state"
)

// Notes is the backend behind notes_read and notes_write: a role's notes as
// one text, read whole and replaced whole. That is the shape a role uses its
// notes in — it merges, prunes and restructures them, which an append cannot
// express — and nothing narrower than "read text, write text back" is
// promised, so a second backend has no structure to map onto. It is an
// interface for the same reason Issues is, and a nil one makes the tools
// report that they are unavailable.
type Notes interface {
	ReadNotes(ctx context.Context, role string) (string, error)
	WriteNotes(ctx context.Context, role string, text string) error
	// Size is how many bytes ReadNotes would return, which is what the
	// scheduler's notes_max_bytes consolidation trigger and `bees status`
	// measure. It goes through the backend rather than the notes file so
	// that they measure whatever notes_read and notes_write actually used.
	Size(ctx context.Context, role string) (int64, error)
}

// FileNotes is the Notes backend over internal/state's notes files: the
// file `bees notes show|edit|reset|add` and a person's editor see. A read
// creates the file first, so the first session of a role reads the section
// headings rather than nothing.
func FileNotes(store *state.Store) Notes { return fileNotes{store: store} }

type fileNotes struct{ store *state.Store }

func (f fileNotes) ReadNotes(_ context.Context, role string) (string, error) {
	if err := f.store.EnsureNotes(role); err != nil {
		return "", err
	}
	return f.store.ReadNotes(role)
}

func (f fileNotes) WriteNotes(_ context.Context, role, text string) error {
	return f.store.WriteNotes(role, text)
}

// Size is a stat of the notes file, so measuring it costs no read.
func (f fileNotes) Size(_ context.Context, role string) (int64, error) {
	return f.store.NotesSize(role)
}

var errNoNotes = errors.New("notes are unavailable: bees.toml could not be loaded")

type notesReadInput struct{}

type notesWriteInput struct {
	Text string `json:"text" jsonschema:"the complete new notes, markdown, replacing everything there was: keep the standard headings and everything still true"`
}

func (s *server) addNotesTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "notes_read",
		Title: "Read your notes",
		Description: "Read your role's notes: the only memory it keeps between sessions, as one " +
			"markdown text. Nothing renders them into the prompt, so read them at the start of a " +
			"session, before anything else.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: schemaFor[notesReadInput](nil),
	}, s.notesRead)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "notes_write",
		Title: "Replace your notes",
		Description: "Replace your role's notes with the given text. The write is the whole text, " +
			"not an addition: read the notes first with notes_read, merge in what this session " +
			"learned, drop what is stale, keep the standard headings, and write the complete " +
			"result back before you report your outcome. An empty text is refused; nothing keeps " +
			"a copy of what a write replaces.",
		InputSchema: schemaFor[notesWriteInput](nil),
	}, s.notesWrite)
}

func (s *server) notesRead(ctx context.Context, _ *mcp.CallToolRequest, _ notesReadInput) (*mcp.CallToolResult, any, error) {
	if s.notes == nil {
		return nil, nil, errNoNotes
	}
	if s.env.Role == "" {
		return nil, nil, errors.New("notes belong to a role: $BEES_ROLE is not set")
	}
	notes, err := s.notes.ReadNotes(ctx, s.env.Role)
	if err != nil {
		return nil, nil, err
	}
	return text("%s", notes), nil, nil
}

func (s *server) notesWrite(ctx context.Context, _ *mcp.CallToolRequest, in notesWriteInput) (*mcp.CallToolResult, any, error) {
	if s.notes == nil {
		return nil, nil, errNoNotes
	}
	if s.env.Role == "" {
		return nil, nil, errors.New("notes belong to a role: $BEES_ROLE is not set")
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, nil, errors.New("refusing to replace your notes with nothing: pass the complete text, headings included")
	}
	if err := s.notes.WriteNotes(ctx, s.env.Role, in.Text); err != nil {
		return nil, nil, err
	}
	return text("notes replaced (%d bytes)", len(in.Text)), nil, nil
}
