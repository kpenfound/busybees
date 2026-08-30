package logging

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
)

// core is the set of destinations shared by every logger derived from a
// Logger. AttachFile appends to it, so handlers that were already derived
// with WithAttrs/WithGroup start writing to the file as well.
type core struct {
	mu       sync.RWMutex
	gen      uint64
	handlers []slog.Handler
	closers  []io.Closer
	files    []string
}

func (c *core) add(h slog.Handler, closer io.Closer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Clone so handlers already holding the old slice never see it change.
	c.handlers = append(slices.Clone(c.handlers), h)
	if closer != nil {
		c.closers = append(c.closers, closer)
	}
	c.gen++
}

func (c *core) hasFile(path string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Contains(c.files, path)
}

func (c *core) snapshot() (uint64, []slog.Handler) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gen, c.handlers
}

func (c *core) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for _, cl := range c.closers {
		errs = append(errs, cl.Close())
	}
	c.closers = nil
	return errors.Join(errs...)
}

// op is one deferred WithAttrs or WithGroup call.
type op struct {
	attrs []slog.Attr
	group string
}

// multiHandler fans a record out to every destination in core. It records
// the WithAttrs/WithGroup calls made on it and replays them onto whatever
// destinations core holds at the time a record is written.
type multiHandler struct {
	core *core
	ops  []op

	mu   sync.Mutex
	gen  uint64
	kids []slog.Handler
}

func newMulti(c *core) *multiHandler { return &multiHandler{core: c} }

// children returns the destinations with this handler's attrs and groups
// applied, rebuilding them when core gained a destination.
func (m *multiHandler) children() []slog.Handler {
	gen, base := m.core.snapshot()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.kids == nil || m.gen != gen {
		kids := make([]slog.Handler, len(base))
		for i, h := range base {
			for _, o := range m.ops {
				if o.group != "" {
					h = h.WithGroup(o.group)
				} else {
					h = h.WithAttrs(slices.Clone(o.attrs))
				}
			}
			kids[i] = h
		}
		m.kids, m.gen = kids, gen
	}
	return m.kids
}

func (m *multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.children() {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.children() {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		errs = append(errs, h.Handle(ctx, r.Clone()))
	}
	return errors.Join(errs...)
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return m
	}
	return m.derive(op{attrs: attrs})
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return m
	}
	return m.derive(op{group: name})
}

func (m *multiHandler) derive(o op) slog.Handler {
	return &multiHandler{core: m.core, ops: append(slices.Clone(m.ops), o)}
}

// console is the terminal destination: a text or JSON handler plus the two
// rules that only apply to the console — quiet mode and printing summary
// records as their bare message in text format.
type console struct {
	inner slog.Handler
	// plain prints summary records as just their message (text format).
	plain bool
	quiet bool
	out   io.Writer
	mu    *sync.Mutex
}

func newConsole(w io.Writer, o Options) slog.Handler {
	opts := &slog.HandlerOptions{Level: o.Level}
	c := &console{quiet: o.Quiet, out: w, mu: &sync.Mutex{}}
	if o.Format == FormatJSON {
		c.inner = slog.NewJSONHandler(w, opts)
	} else {
		c.inner = slog.NewTextHandler(w, opts)
		c.plain = true
	}
	return c
}

func (c *console) Enabled(ctx context.Context, l slog.Level) bool {
	return c.inner.Enabled(ctx, l)
}

func (c *console) Handle(ctx context.Context, r slog.Record) error {
	summary := isSummary(r)
	if c.quiet && r.Level < slog.LevelWarn && !summary {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if summary && c.plain {
		_, err := io.WriteString(c.out, r.Message+"\n")
		return err
	}
	return c.inner.Handle(ctx, r)
}

func (c *console) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *c
	n.inner = c.inner.WithAttrs(attrs)
	return &n
}

func (c *console) WithGroup(name string) slog.Handler {
	n := *c
	n.inner = c.inner.WithGroup(name)
	return &n
}

func isSummary(r slog.Record) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == SummaryKey && a.Value.Kind() == slog.KindBool && a.Value.Bool() {
			found = true
			return false
		}
		return true
	})
	return found
}
