package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The read server gives a restricted turn whose backend has no read-only file
// tool of its own (RestrictedCapabilities.ReadServer: Codex, whose only way to
// read a file is its shell, which RunRestricted switches off) a way to read
// the workspace and nothing else. It is an MCP server served from the runner's
// own process for the length of one turn, on a fresh loopback port behind a
// fresh bearer token, with three tools: read_file, list_directory and
// search_files. Every path they take is opened through an os.Root of the
// workspace, so neither `..` nor a symbolic link reaches a file outside it,
// and none of them writes, runs or fetches anything.
const (
	// ReadServerName is the MCP server name a restricted turn reaches the
	// read server by. An inherited server of the same name is replaced by
	// it rather than disabled.
	ReadServerName = "restricted_read"
	// readServerTokenEnv carries the read server's bearer token to the
	// agent, which presents it on every request: the token goes in the
	// environment, never on the command line.
	readServerTokenEnv = "RESTRICTED_READ_TOKEN"

	// readFileLines is how many lines read_file returns when it is not
	// given a limit, and readOutputBytes the most text any tool returns in
	// one answer: a larger answer is cut, and says where.
	readFileLines   = 2000
	readOutputBytes = 256 << 10
	// searchMatches is the most matches search_files reports, and
	// searchFileBytes the largest file it reads.
	searchMatches   = 200
	searchFileBytes = 4 << 20
	// listEntries is the most entries list_directory reports.
	listEntries = 1000
)

// readServer is one turn's read server.
type readServer struct {
	url, token string
	root       *os.Root
	// rootPath is the workspace with its symbolic links resolved, so an
	// absolute path the agent was told (the prompt names the workspace as
	// the runner saw it) is recognised whichever spelling it uses.
	rootPath, dir string
	hs            *http.Server
	done          chan error
}

// startReadServer serves the read server for dir until close. The caller
// closes it when the turn ends.
func startReadServer(dir string) (*readServer, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("read server: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("read server: %w", err)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("read server: token: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("read server: %w", err)
	}
	s := &readServer{
		url:      "http://" + ln.Addr().String() + "/mcp",
		token:    hex.EncodeToString(b),
		root:     root,
		rootPath: resolved,
		dir:      filepath.Clean(dir),
		done:     make(chan error, 1),
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.server() }, nil)
	s.hs = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})}
	go func() { s.done <- s.hs.Serve(ln) }()
	return s, nil
}

// close stops serving and releases the workspace.
func (s *readServer) close() {
	_ = s.hs.Close()
	<-s.done
	_ = s.root.Close()
}

// readOnlyTool is the annotation every read server tool carries: it reads,
// changes nothing and reaches nothing outside the workspace, which is also
// what lets an agent run it without asking for approval.
func readOnlyTool() *mcp.ToolAnnotations {
	no := false
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: &no, OpenWorldHint: &no}
}

type readFileInput struct {
	Path   string `json:"path" jsonschema:"the file to read: relative to the working directory, or an absolute path inside it"`
	Offset int    `json:"offset,omitempty" jsonschema:"the first line to return, counting from 1; 1 when left out"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many lines to return; 2000 when left out"`
}

type listDirectoryInput struct {
	Path string `json:"path,omitempty" jsonschema:"the directory to list: relative to the working directory, or an absolute path inside it; the working directory when left out"`
}

type searchFilesInput struct {
	Pattern string `json:"pattern" jsonschema:"a regular expression (RE2 syntax) matched against each line"`
	Path    string `json:"path,omitempty" jsonschema:"the file or directory to search; the working directory when left out"`
	Glob    string `json:"glob,omitempty" jsonschema:"only search files whose name matches this pattern, such as *.go"`
}

// server is the MCP server one connection is given.
func (s *readServer) server() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: ReadServerName, Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a text file in the working directory, with line numbers. Read-only.",
		Annotations: readOnlyTool(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in readFileInput) (*mcp.CallToolResult, any, error) {
		out, err := s.readFile(in)
		return textResult(out), nil, err
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_directory",
		Description: "List a directory in the working directory; a directory's name ends in /. Read-only.",
		Annotations: readOnlyTool(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in listDirectoryInput) (*mcp.CallToolResult, any, error) {
		out, err := s.listDirectory(in)
		return textResult(out), nil, err
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_files",
		Description: "Search the text files under a path in the working directory for lines matching a regular expression, answered as path:line: text. Read-only.",
		Annotations: readOnlyTool(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchFilesInput) (*mcp.CallToolResult, any, error) {
		out, err := s.searchFiles(ctx, in)
		return textResult(out), nil, err
	})
	return srv
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// rel turns a path the agent gave into one inside the root, "." for the root
// itself. An absolute path outside the workspace is refused here; a relative
// one that climbs out of it, or a link that points out of it, is refused by
// the root when it is opened.
func (s *readServer) rel(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return ".", nil
	}
	if !filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	p = filepath.Clean(p)
	for _, base := range []string{s.dir, s.rootPath} {
		if r, err := filepath.Rel(base, p); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return r, nil
		}
	}
	return "", fmt.Errorf("%s is outside the working directory %s", p, s.dir)
}

func (s *readServer) readFile(in readFileInput) (string, error) {
	name, err := s.rel(in.Path)
	if err != nil {
		return "", err
	}
	f, err := s.root.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil {
		return "", err
	} else if info.IsDir() {
		return "", fmt.Errorf("%s is a directory: list it with list_directory", in.Path)
	}
	first, limit := max(in.Offset, 1), in.Limit
	if limit <= 0 {
		limit = readFileLines
	}
	var out strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), readOutputBytes)
	line, shown := 0, 0
	for sc.Scan() {
		line++
		if line < first {
			continue
		}
		if shown == limit {
			fmt.Fprintf(&out, "[more lines follow: read on from offset %d]\n", line)
			return out.String(), nil
		}
		text := sc.Text()
		if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			return "", fmt.Errorf("%s is not a text file", in.Path)
		}
		if out.Len()+len(text) > readOutputBytes {
			fmt.Fprintf(&out, "[output cut at %d bytes: read on from offset %d]\n", readOutputBytes, line)
			return out.String(), nil
		}
		fmt.Fprintf(&out, "%6d\t%s\n", line, text)
		shown++
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", in.Path, err)
	}
	if shown == 0 {
		return fmt.Sprintf("[%s has %d lines: nothing from offset %d]\n", in.Path, line, first), nil
	}
	return out.String(), nil
}

func (s *readServer) listDirectory(in listDirectoryInput) (string, error) {
	name, err := s.rel(in.Path)
	if err != nil {
		return "", err
	}
	entries, err := fs.ReadDir(s.root.FS(), filepath.ToSlash(name))
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for i, e := range entries {
		if i == listEntries {
			fmt.Fprintf(&out, "[%d more entries not listed]\n", len(entries)-i)
			break
		}
		out.WriteString(e.Name())
		if e.IsDir() {
			out.WriteString("/")
		}
		out.WriteString("\n")
	}
	if len(entries) == 0 {
		return "[empty directory]\n", nil
	}
	return out.String(), nil
}

func (s *readServer) searchFiles(ctx context.Context, in searchFilesInput) (string, error) {
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return "", fmt.Errorf("pattern: %w", err)
	}
	if in.Glob != "" {
		if _, err := path.Match(in.Glob, ""); err != nil {
			return "", fmt.Errorf("glob: %w", err)
		}
	}
	start, err := s.rel(in.Path)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	found := 0
	errFull := errors.New("full")
	fsys := s.root.FS()
	err = fs.WalkDir(fsys, filepath.ToSlash(start), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == filepath.ToSlash(start) {
				return err
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if in.Glob != "" {
			if ok, _ := path.Match(in.Glob, d.Name()); !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > searchFileBytes {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil || bytes.IndexByte(data[:min(len(data), 8<<10)], 0) >= 0 {
			return nil
		}
		line := 0
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64<<10), searchFileBytes)
		for sc.Scan() {
			line++
			if !re.Match(sc.Bytes()) {
				continue
			}
			if found == searchMatches || out.Len() > readOutputBytes {
				return errFull
			}
			found++
			fmt.Fprintf(&out, "%s:%d: %s\n", p, line, sc.Text())
		}
		return nil
	})
	switch {
	case errors.Is(err, errFull):
		fmt.Fprintf(&out, "[more matches not shown: narrow the pattern, path or glob]\n")
	case err != nil:
		return "", err
	case found == 0:
		return "[no matches]\n", nil
	}
	return out.String(), nil
}
