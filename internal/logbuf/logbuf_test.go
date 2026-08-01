package logbuf

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestNewClampsTheSize(t *testing.T) {
	// A non-positive size would make Add drop every line it was just given, so
	// New has to substitute the default rather than trust the caller.
	for _, max := range []int{0, -1} {
		b := New(max)
		if b.max != 200 {
			t.Errorf("New(%d).max = %d, want 200", max, b.max)
		}
	}
	if got := New(3).max; got != 3 {
		t.Errorf("New(3).max = %d, want 3", got)
	}
}

func TestAddDropsTheOldestLine(t *testing.T) {
	b := New(3)
	for _, line := range []string{"one", "two", "three", "four"} {
		b.Add(line)
	}
	want := []string{"two", "three", "four"}
	assertLines(t, "Lines", b.Lines(), want)
}

func TestLinesReturnsACopy(t *testing.T) {
	// The UI renders what Lines returns while the logger keeps writing, so a
	// shared backing array would be a data race and a rewritten history.
	b := New(3)
	b.Add("original")

	got := b.Lines()
	got[0] = "mutated"

	assertLines(t, "Lines after caller mutation", b.Lines(), []string{"original"})
}

func TestTail(t *testing.T) {
	b := New(10)
	for _, line := range []string{"a", "b", "c"} {
		b.Add(line)
	}

	tests := []struct {
		name string
		n    int
		want []string
	}{
		{name: "zero returns everything", n: 0, want: []string{"a", "b", "c"}},
		{name: "negative returns everything", n: -1, want: []string{"a", "b", "c"}},
		{name: "larger than the buffer returns everything", n: 9, want: []string{"a", "b", "c"}},
		{name: "exactly the buffer returns everything", n: 3, want: []string{"a", "b", "c"}},
		{name: "smaller returns the newest", n: 2, want: []string{"b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertLines(t, "Tail", b.Tail(tt.n), tt.want)
		})
	}
}

func TestHandlerTeesIntoBothSinks(t *testing.T) {
	var file bytes.Buffer
	buf := New(10)
	logger := slog.New(NewHandler(slog.NewTextHandler(&file, nil), buf))

	logger.Info("tunnel removed", "tunnel", "web:80/tcp")

	if !strings.Contains(file.String(), "tunnel removed") {
		t.Errorf("inner handler received %q, want it to contain the message", file.String())
	}
	lines := buf.Lines()
	if len(lines) != 1 {
		t.Fatalf("buffer holds %d lines, want 1", len(lines))
	}
	if !strings.Contains(lines[0], "tunnel removed") || !strings.Contains(lines[0], "tunnel=web:80/tcp") {
		t.Errorf("buffered line = %q, want the message and its attribute", lines[0])
	}
}

func TestHandlerEnabledDefersToTheInnerHandler(t *testing.T) {
	// The log pane must not show records the file level filtered out, which only
	// holds while Enabled forwards to the wrapped handler.
	var file bytes.Buffer
	buf := New(10)
	inner := slog.NewTextHandler(&file, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewHandler(inner, buf)

	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Enabled(Debug) = true under a LevelInfo handler, want false")
	}
	if !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("Enabled(Warn) = false under a LevelInfo handler, want true")
	}

	slog.New(h).Debug("suppressed")
	if got := buf.Lines(); len(got) != 0 {
		t.Errorf("buffer holds %v after a filtered record, want nothing", got)
	}
}

func TestHandlerWithAttrsAndWithGroup(t *testing.T) {
	var file bytes.Buffer
	buf := New(10)
	logger := slog.New(NewHandler(slog.NewTextHandler(&file, nil), buf)).
		With("component", "engine").
		WithGroup("ssh")

	logger.Warn("connection lost", "reason", "eof")

	lines := buf.Lines()
	if len(lines) != 1 {
		t.Fatalf("buffer holds %d lines, want 1", len(lines))
	}
	for _, want := range []string{"WARN", "connection lost", "component=engine", "reason=eof"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("buffered line = %q, want it to contain %q", lines[0], want)
		}
	}
}

func TestRenderUsesTheRecordTime(t *testing.T) {
	buf := New(10)
	h := NewHandler(slog.NewTextHandler(&bytes.Buffer{}, nil), buf)

	// A fixed time keeps the assertion independent of when the suite runs.
	at := time.Date(2024, 3, 1, 14, 5, 9, 0, time.UTC)
	rec := slog.NewRecord(at, slog.LevelInfo, "scan complete", 0)
	rec.AddAttrs(slog.Int("containers", 4))

	if got, want := h.render(rec), "14:05:09 INFO  scan complete containers=4"; got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
}

func TestRenderSubstitutesTheClockForAZeroTime(t *testing.T) {
	// slog.Record zero values reach Handle through Handler.WithAttrs chains in
	// tests and tools; the line still has to be readable.
	buf := New(10)
	h := NewHandler(slog.NewTextHandler(&bytes.Buffer{}, nil), buf)

	got := h.render(slog.NewRecord(time.Time{}, slog.LevelError, "boom", 0))

	if !strings.HasSuffix(got, "ERROR boom") {
		t.Errorf("render = %q, want it to end with the level and message", got)
	}
	if strings.HasPrefix(got, "00:00:00") {
		t.Errorf("render = %q, want a substituted timestamp rather than the zero time", got)
	}
}

func assertLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}
