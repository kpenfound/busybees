// Package statemigrate upgrades busybees runtime state before it is accessed.
// Legacy decoding exists only here; ordinary readers use the current schema.
package statemigrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/kpenfound/busybees/core/work"
	"github.com/kpenfound/busybees/internal/ghwork"
)

const Marker = "schema.json"
const Version = 1

type schema struct {
	Version int `json:"version"`
}

// Ensure serializes upgrades across processes. The marker is published last;
// a failed upgrade can always be retried with its recoverable records intact.
func Ensure(dir string) error {
	complete, err := completed(dir)
	if err != nil || complete {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".schema.lock"), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	complete, err = completed(dir)
	if err != nil || complete {
		return err
	}
	if err := migrate(dir, nil); err != nil {
		return fmt.Errorf("migrate factory state %s: %w", dir, err)
	}
	return nil
}
func completed(dir string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, Marker))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var s schema
	if err := json.Unmarshal(b, &s); err != nil {
		return false, fmt.Errorf("invalid state schema: %w", err)
	}
	if s.Version != Version {
		return false, fmt.Errorf("unsupported state schema %d (want %d)", s.Version, Version)
	}
	return true, nil
}

// Atomic replaces a file using a unique temporary file in the same directory.
func Atomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".migration-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0644); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

type object map[string]json.RawMessage

func encode(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return b
}

// identity preserves unknown fields and arbitrary tags. It never invents a
// second identity when a previous attempt already rewrote the record.
func identity(m object, numberField string) (work.Ref, error) {
	var r work.Ref
	if raw, ok := m["work"]; ok {
		if err := json.Unmarshal(raw, &r); err != nil {
			return r, err
		}
	}
	for _, f := range []string{numberField, "pr"} {
		raw, ok := m[f]
		if !ok {
			continue
		}
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return r, err
		}
		if n > 0 {
			if f == "pr" {
				r = ghwork.WithPR(r, n)
			} else {
				r = ghwork.WithIssue(r, n)
			}
		}
		delete(m, f)
	}
	m["work"] = encode(r)
	return r, nil
}

func rewrite(path string, transform func(object) error, after func(string) error) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var m object
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if m == nil {
		return fmt.Errorf("%s: expected an object", path)
	}
	if err := transform(m); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	out := encode(m)
	if !bytes.Equal(b, out) {
		if err := Atomic(path, out); err != nil {
			return err
		}
		return step(after, path)
	}
	return nil
}
func step(after func(string) error, path string) error {
	if after != nil {
		return after(path)
	}
	return nil
}
func subjects(m object) error { _, err := identity(m, "issue"); return err }

func migrate(dir string, after func(string) error) error {
	mail, err := mailFiles(filepath.Join(dir, "mail"))
	if err != nil {
		return err
	}
	for _, path := range mail {
		if err := rewrite(path, subjects, after); err != nil {
			return err
		}
	}
	if err := ledger(filepath.Join(dir, "ledger.jsonl"), after); err != nil {
		return err
	}
	books, err := jsonFiles(filepath.Join(dir, "issues"))
	if err != nil {
		return err
	}
	for _, path := range books {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		n, err := strconv.Atoi(name)
		if err != nil {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var current struct {
				Work work.Ref `json:"work"`
			}
			if err := json.Unmarshal(b, &current); err != nil {
				return err
			}
			if current.Work.Key == "" || filepath.Base(path) != current.Work.Key.Filename() {
				return fmt.Errorf("invalid bookkeeping identity in %s", path)
			}
			continue
		}
		if n <= 0 {
			return fmt.Errorf("invalid legacy bookkeeping filename %s", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var m object
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if m == nil {
			return fmt.Errorf("%s: expected an object", path)
		}
		r, err := identity(m, "number")
		if err != nil {
			return err
		}
		if r.Key == "" {
			r = ghwork.New(n, ghwork.PR(r))
			m["work"] = encode(r)
		}
		if raw, ok := m["reviewed_sha"]; ok && string(raw) != `""` && string(raw) != "null" && ghwork.PR(r) == 0 && emptyString(m["branch"]) && emptyString(m["worker_stage"]) {
			r = ghwork.New(0, n)
			m["work"] = encode(r)
		}
		if err := keyList(m, "open_children"); err != nil {
			return err
		}
		if ghwork.Number(r.Key) != n {
			return fmt.Errorf("bookkeeping identity disagrees with filename %s", path)
		}
		dst := filepath.Join(filepath.Dir(path), r.Key.Filename())
		if err := move(path, dst, encode(m), after); err != nil {
			return err
		}
	}
	sessions, err := os.ReadDir(filepath.Join(dir, "sessions"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range sessions {
		if !e.IsDir() {
			continue
		}
		sd := filepath.Join(dir, "sessions", e.Name())
		if err := sessionMarker(sd, after); err != nil {
			return err
		}
		if err := rewrite(filepath.Join(sd, "outcome.json"), subjects, after); err != nil {
			return err
		}
		if err := rewrite(filepath.Join(sd, "result.json"), func(m object) error {
			raw, ok := m["outcome"]
			if !ok || string(raw) == "null" {
				return nil
			}
			var out object
			if err := json.Unmarshal(raw, &out); err != nil {
				return err
			}
			if err := subjects(out); err != nil {
				return err
			}
			m["outcome"] = encode(out)
			return nil
		}, after); err != nil {
			return err
		}
	}
	if err := rewrite(filepath.Join(dir, "status.json"), func(m object) error {
		if err := keyList(m, "priority"); err != nil {
			return err
		}
		if raw, ok := m["waiting_on_deps"]; ok && string(raw) != "null" {
			var deps map[string]json.RawMessage
			if err := json.Unmarshal(raw, &deps); err != nil {
				return err
			}
			converted := map[string]json.RawMessage{}
			for key, value := range deps {
				dest := key
				if n, err := strconv.Atoi(key); err == nil {
					dest = string(ghwork.IssueKey(n))
				}
				obj := object{"items": value}
				if err := keyList(obj, "items"); err != nil {
					return err
				}
				if _, ok := converted[dest]; ok {
					return fmt.Errorf("conflicting dependency key %s", dest)
				}
				converted[dest] = obj["items"]
			}
			m["waiting_on_deps"] = encode(converted)
		}
		for _, field := range []string{"workers", "needs_human", "approved"} {
			raw, ok := m[field]
			if !ok {
				continue
			}
			var records []object
			if err := json.Unmarshal(raw, &records); err != nil {
				return err
			}
			for _, r := range records {
				if _, legacy := r["issue"]; legacy && field == "workers" && string(r["stage"]) == `"requested review"` {
					r["pr"] = r["issue"]
					delete(r, "issue")
				}
				if err := subjects(r); err != nil {
					return err
				}
			}
			m[field] = encode(records)
		}
		return nil
	}, after); err != nil {
		return err
	}
	return Atomic(filepath.Join(dir, Marker), encode(schema{Version: Version}))
}

// move publishes the new destination before removing the source. An interrupted
// rename step can leave both; only equivalent records may be coalesced.
func move(src, dst string, b []byte, after func(string) error) error {
	old, err := os.ReadFile(dst)
	switch {
	case err == nil:
		var a, c any
		if json.Unmarshal(old, &a) != nil || json.Unmarshal(b, &c) != nil || !reflect.DeepEqual(a, c) {
			return fmt.Errorf("migration destination conflicts with recoverable source: %s and %s", src, dst)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := Atomic(dst, b); err != nil {
			return err
		}
		if err := step(after, dst); err != nil {
			return err
		}
	default:
		return err
	}
	if err := os.Remove(src); err != nil {
		return err
	}
	return step(after, src)
}

func ledger(path string, after func(string) error) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var out []byte
	for _, line := range bytes.SplitAfter(b, []byte("\n")) {
		var m object
		if json.Unmarshal(line, &m) != nil || m == nil {
			out = append(out, line...)
			continue
		}
		if err := subjects(m); err != nil {
			return err
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			return err
		}
		out = append(out, encoded...)
		if bytes.HasSuffix(line, []byte("\n")) {
			out = append(out, '\n')
		}
	}
	if bytes.Equal(b, out) {
		return nil
	}
	if err := Atomic(path, out); err != nil {
		return err
	}
	return step(after, path)
}

var legacySessionName = regexp.MustCompile(`^\d{8}-\d{6}-(developer-issue|reviewer-pr)-(\d+)-`)

func sessionMarker(dir string, after func(string) error) error {
	src := filepath.Join(dir, "issue")
	dst := filepath.Join(dir, "work.json")
	b, err := os.ReadFile(src)
	if err == nil {
		n, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid session issue marker %s", src)
		}
		return move(src, dst, encode(ghwork.New(n, 0)), after)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m := legacySessionName.FindStringSubmatch(filepath.Base(dir))
	if m == nil {
		return nil
	}
	n, _ := strconv.Atoi(m[2])
	r := ghwork.New(n, 0)
	if m[1] == "reviewer-pr" {
		r = ghwork.New(0, n)
	}
	if err := Atomic(dst, encode(r)); err != nil {
		return err
	}
	return step(after, dst)
}

// keyList accepts legacy integer lists only during the one-time upgrade.
func keyList(m object, field string) error {
	raw, ok := m[field]
	if !ok || string(raw) == "null" {
		return nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	keys := make([]work.Key, 0, len(values))
	for _, v := range values {
		var n int
		if err := json.Unmarshal(v, &n); err == nil {
			keys = append(keys, ghwork.IssueKey(n))
			continue
		}
		var key work.Key
		if err := json.Unmarshal(v, &key); err != nil {
			return err
		}
		keys = append(keys, key)
	}
	m[field] = encode(keys)
	return nil
}

func jsonFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}
func mailFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := jsonFiles(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
	}
	return out, nil
}

func emptyString(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == `""` || string(raw) == "null"
}
