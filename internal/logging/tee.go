package logging

import (
	"context"
	"errors"
	"io"
	"log/slog"
)

// Tee returns a logger that writes every record to l's destinations and, at
// debug level, to a rotating JSON file of its own at path, which no other
// logger derived from l writes to. It is how several projects in one process
// share one console while each keeps its own bees.log. Close the returned
// closer when the logger is done with.
func (l *Logger) Tee(path string) (*slog.Logger, io.Closer, error) {
	w, err := newRotatingWriter(path, defaultMaxBytes)
	if err != nil {
		return nil, nil, err
	}
	file := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(teeHandler{shared: l.Handler(), file: file}), w, nil
}

// teeHandler sends a record to shared and to file.
type teeHandler struct {
	shared, file slog.Handler
}

func (t teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return t.file.Enabled(ctx, level) || t.shared.Enabled(ctx, level)
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	if t.shared.Enabled(ctx, r.Level) {
		errs = append(errs, t.shared.Handle(ctx, r.Clone()))
	}
	if t.file.Enabled(ctx, r.Level) {
		errs = append(errs, t.file.Handle(ctx, r.Clone()))
	}
	return errors.Join(errs...)
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{shared: t.shared.WithAttrs(attrs), file: t.file.WithAttrs(attrs)}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{shared: t.shared.WithGroup(name), file: t.file.WithGroup(name)}
}
