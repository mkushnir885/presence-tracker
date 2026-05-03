package gui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// LogEntry is one captured log record.
type LogEntry struct {
	Time    time.Time
	Level   string
	Message string
	Attrs   string // space-separated key=value pairs
}

// logBuffer is a thread-safe rolling slog.Handler that keeps the most recent
// cap entries in memory while forwarding every record to a next handler.
type logBuffer struct {
	mu      sync.Mutex
	entries []LogEntry
	cap     int
	next    slog.Handler
}

func newLogBuffer(cap int, next slog.Handler) *logBuffer {
	return &logBuffer{cap: cap, next: next}
}

func (b *logBuffer) Enabled(ctx context.Context, level slog.Level) bool {
	return b.next.Enabled(ctx, level)
}

func (b *logBuffer) Handle(ctx context.Context, r slog.Record) error {
	var sb strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		if sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%s=%v", a.Key, a.Value)
		return true
	})

	entry := LogEntry{
		Time:    r.Time,
		Level:   r.Level.String(),
		Message: r.Message,
		Attrs:   sb.String(),
	}

	b.mu.Lock()
	b.entries = append([]LogEntry{entry}, b.entries...)
	if len(b.entries) > b.cap {
		b.entries = b.entries[:b.cap]
	}
	b.mu.Unlock()

	return b.next.Handle(ctx, r)
}

func (b *logBuffer) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logBuffer{cap: b.cap, next: b.next.WithAttrs(attrs)}
}

func (b *logBuffer) WithGroup(name string) slog.Handler {
	return &logBuffer{cap: b.cap, next: b.next.WithGroup(name)}
}

// Entries returns a snapshot of captured entries, newest first.
func (b *logBuffer) Entries() []LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]LogEntry, len(b.entries))
	copy(out, b.entries)
	return out
}
