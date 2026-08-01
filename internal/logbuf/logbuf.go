// Package logbuf keeps the most recent log records in memory so the TUI can
// show them without reading back the log file.
package logbuf

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Buffer is a fixed-size ring of rendered log lines.
type Buffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// New creates a buffer holding at most max lines.
func New(max int) *Buffer {
	if max <= 0 {
		max = 200
	}
	return &Buffer{max: max}
}

// Add appends a line, dropping the oldest when full.
func (b *Buffer) Add(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
}

// Lines returns a copy of the buffered lines, oldest first.
func (b *Buffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...)
}

// Tail returns the last n lines, oldest first.
func (b *Buffer) Tail(n int) []string {
	lines := b.Lines()
	if n <= 0 || len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// Handler mirrors every record into a Buffer while delegating to another
// handler, so the same log ends up in the file and on screen.
type Handler struct {
	inner slog.Handler
	buf   *Buffer
	attrs []slog.Attr
	group string
}

// NewHandler wraps inner, teeing records into buf.
func NewHandler(inner slog.Handler, buf *Buffer) *Handler {
	return &Handler{inner: inner, buf: buf}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	h.buf.Add(h.render(r))
	return h.inner.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{
		inner: h.inner.WithAttrs(attrs),
		buf:   h.buf,
		attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...),
		group: h.group,
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), buf: h.buf, attrs: h.attrs, group: name}
}

// render formats a record compactly: the log pane is narrow, so timestamps lose
// the date and attributes stay on one line.
func (h *Handler) render(r slog.Record) string {
	var b strings.Builder
	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	fmt.Fprintf(&b, "%s %-5s %s", ts.Format("15:04:05"), r.Level.String(), r.Message)
	for _, a := range h.attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	return b.String()
}
