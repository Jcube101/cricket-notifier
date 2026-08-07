package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// --- The disabled logger -----------------------------------------------------

// TestDisabledActivityLoggerIsSilent exercises every activity.* method on a
// logger with a nil file. Per LEARNINGS.md, treating a failed log open as
// fatal once crash-looped the service ~6900 times; the contract now is that
// every one of these must be a silent no-op, never a panic.
func TestDisabledActivityLoggerIsSilent(t *testing.T) {
	l := newDisabledActivityLogger()
	l.logDiscovery(true, 1, 10)
	l.logDiscovery(false, 0, 10)
	l.logSeed(1, 10, "In Progress", 100, 2)
	l.logWatch(1, 10, nil)
	l.logWatch(1, 10, []string{"x"})
	l.logSkippedExhibition(1, "Warm-up Match", "India vs Sri Lanka XI")
	l.logRejected(1, 10, 305, 284, 6, 5)
	l.logDone(1, "already finished")
	l.logAPIError("watch", 500, []byte("body"))
	l.logError("watch", errors.New("boom"))
	l.writeLine("anything")
	l.close()
}

func TestDisabledActivityLoggerCloseIdempotent(t *testing.T) {
	l := newDisabledActivityLogger()
	l.close()
	l.close() // must not panic
}

// --- Rotation ------------------------------------------------------------------

func TestRotationOnOverflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.log")
	l, err := newActivityLogger(path, 60) // tiny cap forces rotation quickly
	if err != nil {
		t.Fatalf("newActivityLogger: %v", err)
	}
	defer l.close()

	l.writeLine("alpha")   // entry ~26 bytes, size 0 -> 26
	l.writeLine("bravo")   // entry ~26 bytes, size 26 -> 52
	l.writeLine("charlie") // entry ~28 bytes; 52+28>60 -> rotates first

	backupPath := path + ".1"
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("expected a %s backup to exist after rotation: %v", backupPath, err)
	}
	newContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(newContent), "charlie") || strings.Contains(string(newContent), "alpha") {
		t.Fatalf("expected the fresh file to contain only the newest line, got %q", newContent)
	}
	backupContent, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read %s: %v", backupPath, err)
	}
	if !strings.Contains(string(backupContent), "alpha") || !strings.Contains(string(backupContent), "bravo") {
		t.Fatalf("expected the backup to hold the rotated-out lines, got %q", backupContent)
	}
}

func TestSecondRotationOverwritesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.log")
	l, err := newActivityLogger(path, 60)
	if err != nil {
		t.Fatalf("newActivityLogger: %v", err)
	}
	defer l.close()

	l.writeLine("alpha")   // 0 -> 26
	l.writeLine("bravo")   // 26 -> 52
	l.writeLine("charlie") // rotate #1: backup = alpha+bravo; new file size 0 -> 28
	l.writeLine("delta")   // 28 -> 54
	l.writeLine("echo")    // 54+25>60 -> rotate #2: backup becomes charlie+delta

	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if strings.Contains(string(backup), "alpha") {
		t.Fatalf("expected the first rotation's backup to be overwritten, still found alpha: %q", backup)
	}
	if !strings.Contains(string(backup), "charlie") || !strings.Contains(string(backup), "delta") {
		t.Fatalf("expected the backup to now hold charlie+delta, got %q", backup)
	}

	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read active file: %v", err)
	}
	if !strings.Contains(string(active), "echo") {
		t.Fatalf("expected the active file to hold echo, got %q", active)
	}
}

func TestRotationSizeTrackingAfterRotate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.log")
	l, err := newActivityLogger(path, 60)
	if err != nil {
		t.Fatalf("newActivityLogger: %v", err)
	}
	defer l.close()

	l.writeLine("alpha")   // 0 -> 26
	l.writeLine("bravo")   // 26 -> 52, rotate on next write
	l.writeLine("charlie") // rotates; new file starts at 0, then 28
	l.writeLine("delta")   // 28 -> 54, must NOT rotate (54 <= 60)

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(content), "charlie") || !strings.Contains(string(content), "delta") {
		t.Fatalf("expected charlie+delta in the active file without a premature rotation, got %q", content)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !strings.Contains(string(backup), "alpha") || !strings.Contains(string(backup), "bravo") {
		t.Fatalf("expected the backup to still hold alpha+bravo (no second rotation yet), got %q", backup)
	}
}

func TestConcurrentWriteLineProducesWellFormedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.log")
	l, err := newActivityLogger(path, activityLogMaxBytes)
	if err != nil {
		t.Fatalf("newActivityLogger: %v", err)
	}
	defer l.close()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.writeLine(fmt.Sprintf("goroutine-line-%d", i))
		}(i)
	}
	wg.Wait()
	l.close()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("expected %d well-formed lines, got %d: %q", n, len(lines), content)
	}
	linePattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} goroutine-line-\d+$`)
	for _, ln := range lines {
		if !linePattern.MatchString(ln) {
			t.Fatalf("malformed or interleaved line from concurrent writes: %q", ln)
		}
	}
}

// --- Construction and formatting -------------------------------------------------

func TestNewActivityLoggerCreatesMissingDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "sub", "activity.log")
	l, err := newActivityLogger(path, activityLogMaxBytes)
	if err != nil {
		t.Fatalf("expected the parent directory to be created, got error: %v", err)
	}
	defer l.close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected the log file to exist: %v", err)
	}
}

func TestNewActivityLoggerUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission checks don't apply")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700) // restore so t.TempDir() cleanup can remove it

	path := filepath.Join(dir, "activity.log")
	if _, err := newActivityLogger(path, activityLogMaxBytes); err == nil {
		t.Fatalf("expected an error opening a log file under a read-only directory, mirroring the systemd ReadOnlyPaths sandbox")
	}
}

func TestNewActivityLoggerAppendsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.log")
	const preexisting = "preexisting line\n"
	if err := os.WriteFile(path, []byte(preexisting), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	l, err := newActivityLogger(path, activityLogMaxBytes)
	if err != nil {
		t.Fatalf("newActivityLogger: %v", err)
	}
	defer l.close()

	if l.size != int64(len(preexisting)) {
		t.Fatalf("expected initial size %d to reflect the existing file, got %d", len(preexisting), l.size)
	}

	l.writeLine("new line")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.HasPrefix(string(content), preexisting) {
		t.Fatalf("expected append rather than truncate, got %q", content)
	}
	if !strings.Contains(string(content), "new line") {
		t.Fatalf("expected the new line to be appended, got %q", content)
	}
}

func TestWriteLineFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.log")
	l, err := newActivityLogger(path, activityLogMaxBytes)
	if err != nil {
		t.Fatalf("newActivityLogger: %v", err)
	}
	defer l.close()

	l.writeLine("hello world")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	linePattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} hello world\n$`)
	if !linePattern.MatchString(string(content)) {
		t.Fatalf("expected a timestamp-prefixed line ending in exactly one newline, got %q", content)
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	in := "line1\r\nline2\nline3\rline4\tline5  "
	want := "line1 line2 line3 line4 line5"
	if got := oneLine(in); got != want {
		t.Fatalf("oneLine(%q) = %q, want %q", in, got, want)
	}
}

func TestOneLineMultilineHTMLBody(t *testing.T) {
	// A RapidAPI 429 in practice returns a multi-line HTML error page, which is
	// exactly what logAPIError feeds into oneLine before writing it.
	in := "<html>\r\n<body>\r\n<h1>429 Too Many Requests</h1>\r\n</body>\r\n</html>\r\n"
	got := oneLine(in)
	if strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("expected all newlines/tabs collapsed to spaces, got %q", got)
	}
	if !strings.Contains(got, "429 Too Many Requests") {
		t.Fatalf("expected the body's content to be preserved, got %q", got)
	}
}

func TestQuotaStr(t *testing.T) {
	tests := []struct {
		remaining int
		want      string
	}{
		{-1, "unknown"},
		{0, "0"},
		{42, "42"},
	}
	for _, tt := range tests {
		if got := quotaStr(tt.remaining); got != tt.want {
			t.Fatalf("quotaStr(%d) = %q, want %q", tt.remaining, got, tt.want)
		}
	}
}

func TestLogWatchSummaryForms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.log")
	l, err := newActivityLogger(path, activityLogMaxBytes)
	if err != nil {
		t.Fatalf("newActivityLogger: %v", err)
	}
	defer l.close()

	l.logWatch(1, 100, nil)
	l.logWatch(1, 99, []string{"event one", "event two", "event three"})
	l.close()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), content)
	}
	if !strings.Contains(lines[0], "no change") {
		t.Fatalf("expected 'no change' with zero events, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "3 event(s): event one | event two | event three") {
		t.Fatalf("expected the 3-event summary form, got %q", lines[1])
	}
}
