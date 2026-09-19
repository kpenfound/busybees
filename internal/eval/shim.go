package eval

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/internal/fakegh"
)

// A session runs in its own process, and so does the bees MCP server it
// starts, and both run gh. An eval puts a directory first on their PATH
// whose gh is a script running `bees eval gh <url> <args>`, and that
// command (Shim) sends the call to the eval's fake GitHub, which serves it
// over HTTP on the loopback (serve). Nothing a session runs as gh reaches
// GitHub.

// ShimCommand is the bees subcommand the gh script runs, with the server's
// URL and gh's arguments after it.
var ShimCommand = []string{"eval", "gh"}

// shimRequest is one gh call: its arguments and, when one of them reads
// standard input, the input.
type shimRequest struct {
	Args  []string `json:"args"`
	Stdin *string  `json:"stdin,omitempty"`
}

// shimResponse is what the call printed, or the error it failed with.
type shimResponse struct {
	Stdout string `json:"stdout"`
	Error  string `json:"error,omitempty"`
}

// server serves one fake GitHub to the gh shims of one case.
type server struct {
	URL string
	srv *http.Server
}

// serve starts serving f on a loopback port, at a path no other process on
// the machine can guess.
func serve(f *fakegh.GitHub) (*server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		_ = ln.Close()
		return nil, err
	}
	path := "/" + hex.EncodeToString(token)
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		var req shimRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var out []byte
		if req.Stdin != nil {
			out, err = f.ExecStdin(r.Context(), *req.Stdin, req.Args...)
		} else {
			out, err = f.Exec(r.Context(), req.Args...)
		}
		resp := shimResponse{Stdout: string(out)}
		if err != nil {
			resp.Error = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	s := &server{URL: "http://" + ln.Addr().String() + path, srv: &http.Server{Handler: mux}}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

func (s *server) Close() error { return s.srv.Close() }

// writeShim writes dir's gh, the script that runs bees's ShimCommand
// against url, and a bees beside it, a link to bees, so that dir can go
// first on a session's PATH.
func writeShim(dir, bees, url string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	script := "#!/bin/sh\n# The eval's gh: every call goes to its fake GitHub.\nexec " + shellQuote(bees)
	for _, a := range ShimCommand {
		script += " " + shellQuote(a)
	}
	script += " " + shellQuote(url) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		return err
	}
	return os.Symlink(bees, filepath.Join(dir, "bees"))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Shim runs one gh call against the fake GitHub at url, from dir, and
// returns gh's exit status: 0, or 1 with the error on stderr. File
// arguments are made absolute, since the server does not run in dir, and
// standard input is read only when an argument reads it.
func Shim(ctx context.Context, url, dir string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	req := shimRequest{Args: shimArgs(args, dir)}
	if readsStdin(req.Args) {
		b, err := io.ReadAll(stdin)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "gh (bees eval):", err)
			return 1
		}
		in := string(b)
		req.Stdin = &in
	}
	resp, err := post(ctx, url, req)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "gh (bees eval): the eval's GitHub did not answer:", err)
		return 1
	}
	_, _ = io.WriteString(stdout, resp.Stdout)
	if resp.Error != "" {
		_, _ = fmt.Fprintln(stderr, resp.Error)
		return 1
	}
	return 0
}

func post(ctx context.Context, url string, req shimRequest) (shimResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return shimResponse{}, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return shimResponse{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		return shimResponse{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return shimResponse{}, errors.New(res.Status + ": " + strings.TrimSpace(string(b)))
	}
	var resp shimResponse
	err = json.NewDecoder(res.Body).Decode(&resp)
	return resp, err
}

// fileFlags take a file, "-" for standard input; fieldFlags take
// key=value, "@file" in the value for a file's content.
var (
	fileFlags  = []string{"--body-file", "--input"}
	fieldFlags = []string{"-F", "--field"}
)

// shimArgs is args with every --flag=value of a file flag split in two and
// every relative file made absolute against dir.
func shimArgs(args []string, dir string) []string {
	var out []string
	for _, a := range args {
		if name, value, ok := strings.Cut(a, "="); ok && strings.HasPrefix(name, "--") && (isFileFlag(name) || isFieldFlag(name)) {
			out = append(out, name, value)
			continue
		}
		out = append(out, a)
	}
	abs := func(p string) string {
		if p == "-" || p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	for i := 0; i+1 < len(out); i++ {
		switch {
		case isFieldFlag(out[i]):
			if k, v, ok := strings.Cut(out[i+1], "="); ok {
				if file, isFile := strings.CutPrefix(v, "@"); isFile {
					out[i+1] = k + "=@" + abs(file)
				}
			}
			i++
		case isFileFlag(out[i]):
			out[i+1] = abs(out[i+1])
			i++
		}
	}
	return out
}

// readsStdin reports whether a call reads standard input: a file flag
// given "-", or a field given "@-".
func readsStdin(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		switch {
		case isFieldFlag(args[i]) && strings.HasSuffix(args[i+1], "=@-"):
			return true
		case isFileFlag(args[i]) && args[i+1] == "-":
			return true
		}
	}
	return false
}

func isFileFlag(name string) bool  { return slices.Contains(fileFlags, name) }
func isFieldFlag(name string) bool { return slices.Contains(fieldFlags, name) }
