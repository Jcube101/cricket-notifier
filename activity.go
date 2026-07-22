package main

// activity.go — a small, dependency-free persistent activity log.
//
// This is an *observability* layer that sits alongside slog. It writes one
// plain-text line per event (discovery check, watch poll, API error) to
// logs/activity.log so the service's decisions survive journald rotation —
// the systemd journal on this host lives on a tiny RAM disk and only retains
// ~a day (see SPEC.md / CLAUDE.md notes on the quota-conscious design).
//
// Rotation is intentionally minimal: when activity.log would exceed maxBytes
// it is renamed to activity.log.1 (overwriting any previous backup) and a
// fresh activity.log is started. One backup is kept — no compression, no
// timestamped archives, no third-party logging framework.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// activityLogMaxBytes caps activity.log before it rotates to activity.log.1.
const activityLogMaxBytes int64 = 5 * 1024 * 1024 // 5 MiB

// activityLogger is a tiny append-only, size-rotating text logger. All writes
// go through writeLine, which is safe for concurrent use by both loops.
type activityLogger struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

// newActivityLogger creates the log's parent directory if needed and opens the
// file for appending. It returns an error only if the directory or file cannot
// be created — a running service should treat that as fatal at startup.
func newActivityLogger(path string, maxBytes int64) (*activityLogger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	l := &activityLogger{path: path, maxBytes: maxBytes}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// open (re)opens the log file in append mode and records its current size.
func (l *activityLogger) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open activity log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat activity log: %w", err)
	}
	l.file = f
	l.size = info.Size()
	return nil
}

// close flushes and closes the underlying file. Safe to call on shutdown.
func (l *activityLogger) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}

// writeLine prepends a timestamp, rotates if the file would overflow, and
// appends the line. Failures are logged to slog but never propagated — losing
// an observability line must not disturb the service.
func (l *activityLogger) writeLine(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	entry := time.Now().Format("2006-01-02 15:04:05") + " " + oneLine(line) + "\n"
	if l.size+int64(len(entry)) > l.maxBytes {
		l.rotate()
	}
	n, err := l.file.WriteString(entry)
	if err != nil {
		slog.Error("activity log write failed", "err", err)
		return
	}
	l.size += int64(n)
}

// rotate closes the current file, renames it to <path>.1 (overwriting the old
// backup), and opens a fresh file. Caller must hold l.mu.
func (l *activityLogger) rotate() {
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		slog.Error("activity log rotate failed", "err", err)
	}
	if err := l.open(); err != nil {
		slog.Error("activity log reopen after rotate failed", "err", err)
	}
}

// --- event helpers ----------------------------------------------------------

// logDiscovery records one discovery check: whether a live India match was
// found (with its id) and the quota remaining reported by the API.
func (l *activityLogger) logDiscovery(found bool, matchID, remaining int) {
	if found {
		l.writeLine(fmt.Sprintf("discovery: found live India match id=%d (quota remaining=%s)",
			matchID, quotaStr(remaining)))
		return
	}
	l.writeLine(fmt.Sprintf("discovery: no live India match (quota remaining=%s)", quotaStr(remaining)))
}

// logSeed records the first (silent) snapshot the watch loop stores for a match.
func (l *activityLogger) logSeed(matchID, remaining int, state string, runs, wkts int) {
	l.writeLine(fmt.Sprintf("watch: match=%d quota remaining=%s -> seeded (state=%q %d/%d)",
		matchID, quotaStr(remaining), state, runs, wkts))
}

// logWatch records one watch poll: the quota remaining and a one-line summary
// of what the diff produced — either the events that fired or "no change".
func (l *activityLogger) logWatch(matchID, remaining int, events []string) {
	summary := "no change"
	if len(events) > 0 {
		parts := make([]string, len(events))
		for i, e := range events {
			parts[i] = oneLine(e)
		}
		summary = fmt.Sprintf("%d event(s): %s", len(events), strings.Join(parts, " | "))
	}
	l.writeLine(fmt.Sprintf("watch: match=%d quota remaining=%s -> %s",
		matchID, quotaStr(remaining), summary))
}

// logDone records a poll on which the match was already/now finished.
func (l *activityLogger) logDone(matchID int, note string) {
	l.writeLine(fmt.Sprintf("watch: match=%d -> %s", matchID, note))
}

// logAPIError records an API failure with its HTTP status and raw response
// body (newlines collapsed so the line stays on one line).
func (l *activityLogger) logAPIError(where string, status int, body []byte) {
	l.writeLine(fmt.Sprintf("error: %s: HTTP %d body=%s", where, status, oneLine(string(body))))
}

// logError records a non-HTTP failure (transport, decode, etc.).
func (l *activityLogger) logError(where string, err error) {
	l.writeLine(fmt.Sprintf("error: %s: %v", where, err))
}

// quotaStr renders the remaining count, showing "unknown" when the header was
// absent (-1).
func quotaStr(remaining int) string {
	if remaining < 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d", remaining)
}

// oneLine collapses any newlines/tabs so a value never breaks the one-line-per
// -event format.
func oneLine(s string) string {
	r := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ")
	return strings.TrimSpace(r.Replace(s))
}
