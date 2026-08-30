// Package logging owns the bees log setup: one console handler configured by
// the global flags (text or JSON, a level, quiet mode) and, for the commands
// that run sessions, a rotating JSON file in the state directory.
//
// Records carrying the attribute summary=true are the one-line session
// summaries the scheduler emits. In text format they are printed as their
// bare message so a terminal reads like a report; in JSON they are ordinary
// records with "summary":true. Quiet mode keeps them (and warnings and
// errors) on the console and drops everything else. The file always gets
// every record at debug level, whatever the console flags say.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Formats accepted by --log-format.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// SummaryKey marks a record as a session summary line.
const SummaryKey = "summary"

// Options configure the console handler.
type Options struct {
	// Format is FormatText or FormatJSON. Empty means text.
	Format string
	// Level is the console level.
	Level slog.Level
	// Quiet limits the console to summaries, warnings and errors.
	Quiet bool
	// Console is where console records go. Empty means os.Stderr.
	Console io.Writer
}

// Logger is a *slog.Logger whose handler can gain a file destination after
// the fact, so loggers already derived with With() pick it up too.
type Logger struct {
	*slog.Logger
	core *core
	// console is the writer the console handler was built with, reused when
	// SetConsole is called without one.
	console io.Writer
}

// New builds a logger with a console handler only.
func New(o Options) *Logger {
	w := o.Console
	if w == nil {
		w = os.Stderr
	}
	c := &core{}
	c.add(newConsole(w, o), nil)
	return &Logger{Logger: slog.New(newMulti(c)), core: c, console: w}
}

// SetConsole replaces the console handler, so a command that has since loaded
// bees.toml can apply its [logging] table to a logger built from the flags
// alone. An empty o.Console reuses the writer the logger was created with.
// Loggers already derived with With() pick the new handler up; the file
// handler, if any, is untouched and still gets every record at debug.
func (l *Logger) SetConsole(o Options) {
	if o.Console == nil {
		o.Console = l.console
	}
	l.console = o.Console
	l.core.replaceConsole(newConsole(o.Console, o))
}

// AttachFile adds a rotating JSON file handler writing every record at debug
// level to path. Calling it again with the same path is a no-op.
func (l *Logger) AttachFile(path string) error {
	return l.attachFile(path, defaultMaxBytes)
}

func (l *Logger) attachFile(path string, maxBytes int64) error {
	if l.core.hasFile(path) {
		return nil
	}
	w, err := newRotatingWriter(path, maxBytes)
	if err != nil {
		return err
	}
	l.core.add(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}), w)
	l.core.files = append(l.core.files, path)
	return nil
}

// Close releases the file handler, if any.
func (l *Logger) Close() error { return l.core.close() }

// ParseFormat validates a --log-format value.
func ParseFormat(s string) (string, error) {
	switch s {
	case "":
		return FormatText, nil
	case FormatText, FormatJSON:
		return s, nil
	}
	return "", fmt.Errorf("invalid log format %q: valid values are %s, %s", s, FormatText, FormatJSON)
}

// ParseLevel validates a --log-level value.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid log level %q: valid values are debug, info, warn, error", s)
}
