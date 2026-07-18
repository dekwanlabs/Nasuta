package log

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileDisabled(t *testing.T) {
	cases := map[string]bool{
		"":           true,
		"off":        true,
		"OFF":        true,
		"  off  ":    true,
		"/a/b/c.log": false,
	}
	for in, want := range cases {
		if got := fileDisabled(in); got != want {
			t.Errorf("fileDisabled(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRenderPlainHasNoAnsi(t *testing.T) {
	r := slog.NewRecord(time.Now(), slog.LevelError, "boom", 0)
	out := render(r, context.Background(), false)
	if strings.Contains(string(out), "\033[") {
		t.Errorf("plain render leaked ANSI codes: %q", out)
	}
	if !strings.Contains(string(out), "ERROR") {
		t.Errorf("plain render missing ERROR label: %q", out)
	}
}

func TestRenderColorHasAnsi(t *testing.T) {
	r := slog.NewRecord(time.Now(), slog.LevelWarn, "careful", 0)
	out := render(r, context.Background(), true)
	if !strings.Contains(string(out), "\033[33m") {
		t.Errorf("color render missing WARN ANSI: %q", out)
	}
}

func TestInitWritesFileNoAnsi(t *testing.T) {
	defer setDefault(nil, true) // restore console-only for other tests

	dir := t.TempDir()
	// Use a nested path to confirm Init creates intermediate dirs. Under daily
	// rotation the active file is codeloom-<today>.log, so glob for it.
	path := filepath.Join(dir, "sub", "codeloom.log")
	if err := Init(Options{
		File: path, Stdout: false,
		MaxBackups: 1, MaxAge: 1,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	Infof("hello %s", "world")

	matches, err := filepath.Glob(filepath.Join(dir, "sub", "codeloom-*.log"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 dated log file, got %v", matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "hello world") {
		t.Errorf("log file missing message; got %q", s)
	}
	if strings.Contains(s, "\033[") {
		t.Errorf("log file contains ANSI escape codes: %q", s)
	}
}

func TestInitOffCreatesNoFile(t *testing.T) {
	defer setDefault(nil, true)

	dir := t.TempDir()
	if err := Init(Options{File: "off", Stdout: false}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	Infof("nothing to see")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no files when File=off, got %v", entries)
	}
}

func mustWrite(t *testing.T, w *dailyRotateWriter, s string) {
	t.Helper()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func assertContains(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(b), want) {
		t.Errorf("%s missing %q; got %q", path, want, string(b))
	}
}

// TestDailyRollover verifies a day boundary produces a new file and keeps
// each day's content isolated.
func TestDailyRollover(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.Local)
	w, err := newDailyRotateWriter(filepath.Join(dir, "codeloom.log"),
		Options{}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer w.Close()

	mustWrite(t, w, "day1 line\n")
	now = now.Add(24 * time.Hour) // cross midnight
	mustWrite(t, w, "day2 line\n")

	day1 := filepath.Join(dir, "codeloom-2026-07-18.log")
	day2 := filepath.Join(dir, "codeloom-2026-07-19.log")
	assertContains(t, day1, "day1 line")
	assertContains(t, day2, "day2 line")
	if b, _ := os.ReadFile(day1); strings.Contains(string(b), "day2") {
		t.Errorf("day1 file leaked day2 content: %q", b)
	}
}

// TestDailySameDayAppends confirms a same-day restart appends rather than
// rolling to a new file.
func TestDailySameDayAppends(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.Local)
	path := filepath.Join(dir, "codeloom.log")
	w, err := newDailyRotateWriter(path, Options{}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	mustWrite(t, w, "first\n")
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Simulate a restart same day: new writer, same clock.
	w2, err := newDailyRotateWriter(path, Options{}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new2: %v", err)
	}
	mustWrite(t, w2, "second\n")
	if err := w2.Close(); err != nil {
		t.Fatalf("close2: %v", err)
	}

	// Exactly one file, containing both lines.
	matches, _ := filepath.Glob(filepath.Join(dir, "codeloom-*.log"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 file, got %v", matches)
	}
	assertContains(t, matches[0], "first")
	assertContains(t, matches[0], "second")
}

func TestDailyPruneByAge(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"2026-07-10", "2026-07-15", "2026-07-17"} {
		if err := os.WriteFile(filepath.Join(dir, "codeloom-"+d+".log"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.Local)
	// MaxAge=3 -> cutoff 2026-07-15; delete dates strictly older.
	w, err := newDailyRotateWriter(filepath.Join(dir, "codeloom.log"),
		Options{MaxAge: 3}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer w.Close()
	mustWrite(t, w, "today\n") // triggers prune; today's file (07-18) is active

	absent := filepath.Join(dir, "codeloom-2026-07-10.log")
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Errorf("expected 07-10 pruned by age, got err=%v", err)
	}
	// 07-15 (== cutoff, not older) and 07-17 survive.
	assertContains(t, filepath.Join(dir, "codeloom-2026-07-15.log"), "x")
	assertContains(t, filepath.Join(dir, "codeloom-2026-07-17.log"), "x")
}

func TestDailyPruneByBackups(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"2026-07-15", "2026-07-16", "2026-07-17"} {
		if err := os.WriteFile(filepath.Join(dir, "codeloom-"+d+".log"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.Local)
	// MaxBackups=2 -> keep the 2 newest backups; today (07-18) is not counted.
	w, err := newDailyRotateWriter(filepath.Join(dir, "codeloom.log"),
		Options{MaxBackups: 2}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer w.Close()
	mustWrite(t, w, "today\n")

	if _, err := os.Stat(filepath.Join(dir, "codeloom-2026-07-15.log")); !os.IsNotExist(err) {
		t.Errorf("expected oldest backup 07-15 pruned, got err=%v", err)
	}
	assertContains(t, filepath.Join(dir, "codeloom-2026-07-16.log"), "x")
	assertContains(t, filepath.Join(dir, "codeloom-2026-07-17.log"), "x")
}

func TestDailyCompress(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.Local)
	w, err := newDailyRotateWriter(filepath.Join(dir, "codeloom.log"),
		Options{Compress: true}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer w.Close()

	mustWrite(t, w, "day1 line\n")
	now = now.Add(24 * time.Hour) // rollover compresses the previous day
	mustWrite(t, w, "day2 line\n")

	gz := filepath.Join(dir, "codeloom-2026-07-18.log.gz")
	if _, err := os.Stat(gz); err != nil {
		t.Errorf("expected compressed %s, got err=%v", gz, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "codeloom-2026-07-18.log")); !os.IsNotExist(err) {
		t.Errorf("expected uncompressed day1 file removed after compress, got err=%v", err)
	}
}
