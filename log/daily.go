package log

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// dateFmt is the daily-suffix format used in filenames: codeloom-2006-01-02.log.
const dateFmt = "2006-01-02"

// dailyRotateWriter writes one log file per calendar day, rolling over at
// local midnight. It replaces lumberjack's size-based rotation with time-based
// daily rotation: the active file is <dir>/<base>-YYYY-MM-DD<ext>, so each day
// lands in its own file and operators can isolate a single day's logs.
//
// Retention is bounded two ways, both defaulting to "unlimited" at 0:
//   - MaxAge: delete backups whose name-date is older than this many days
//   - MaxBackups: keep at most this many daily backup files (oldest dropped first)
//
// When Compress is set, the just-closed day's file is gzip-compressed (and the
// uncompressed original removed) at rollover; compressed files share the same
// age/count retention. Today's active file is never compressed or deleted.
//
// now is injected so tests can drive a fake clock across midnight; production
// passes time.Now.
type dailyRotateWriter struct {
	dir        string
	base       string // filename without extension, e.g. "codeloom"
	ext        string // including leading dot, e.g. ".log"
	maxAge     int    // days; 0 = unlimited
	maxBackups int    // count; 0 = unlimited
	compress   bool

	now func() time.Time

	mu      sync.Mutex
	curDate string // "" before first open
	curFile *os.File
}

func newDailyRotateWriter(path string, o Options, now func() time.Time) (*dailyRotateWriter, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	base = strings.TrimSuffix(base, ext)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create log dir %q: %w", dir, err)
		}
	}
	return &dailyRotateWriter{
		dir:        dir,
		base:       base,
		ext:        ext,
		maxAge:     o.MaxAge,
		maxBackups: o.MaxBackups,
		compress:   o.Compress,
		now:        now,
	}, nil
}

func (w *dailyRotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	today := w.now().Format(dateFmt)
	if w.curFile == nil || today != w.curDate {
		// First write, or the day rolled over since the last write: archive the
		// previous day, prune retained backups, then open today's file.
		if w.curFile != nil {
			prev := w.curDate
			_ = w.curFile.Close()
			w.curFile = nil
			if w.compress {
				w.compressDay(prev)
			}
		}
		w.prune(today)
		if err := w.openToday(today); err != nil {
			return 0, err
		}
	}
	return w.curFile.Write(p)
}

func (w *dailyRotateWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.curFile == nil {
		return nil
	}
	err := w.curFile.Close()
	w.curFile = nil
	return err
}

func (w *dailyRotateWriter) pathFor(date string) string {
	return filepath.Join(w.dir, w.base+"-"+date+w.ext)
}

func (w *dailyRotateWriter) openToday(today string) error {
	f, err := os.OpenFile(w.pathFor(today), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", w.pathFor(today), err)
	}
	w.curFile = f
	w.curDate = today
	return nil
}

// compressDay gzip-compresses the given day's file alongside itself
// (<base>-<date><ext>.gz) and removes the uncompressed original. Missing or
// already-compressed days are no-ops. Errors are swallowed: a failed compress
// leaves the uncompressed file in place, where MaxAge will eventually retire it.
func (w *dailyRotateWriter) compressDay(date string) {
	src := w.pathFor(date)
	dst := src + ".gz"
	if _, err := os.Stat(dst); err == nil {
		_ = os.Remove(src) // compressed copy already exists; drop stray original
		return
	}
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return
	}
	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return
	}
	if err := gz.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return
	}
	_ = os.Remove(src)
}

type backup struct {
	date string
	path string
}

// pattern matches this writer's backup files and captures the date:
// ^<base>-YYYY-MM-DD<ext>(\.gz)?$
func (w *dailyRotateWriter) pattern() *regexp.Regexp {
	expr := "^" + regexp.QuoteMeta(w.base) + `-(\d{4}-\d{2}-\d{2})` + regexp.QuoteMeta(w.ext) + `(\.gz)?$`
	return regexp.MustCompile(expr)
}

// listBackups scans the log dir for dated backups, excluding today's active file.
func (w *dailyRotateWriter) listBackups(excludeDate string) []backup {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil
	}
	rx := w.pattern()
	out := make([]backup, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := rx.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if m[1] == excludeDate {
			continue // today's active file
		}
		out = append(out, backup{date: m[1], path: filepath.Join(w.dir, e.Name())})
	}
	return out
}

// prune enforces MaxAge (days) and MaxBackups (count) on dated backups.
func (w *dailyRotateWriter) prune(today string) {
	if w.maxAge <= 0 && w.maxBackups <= 0 {
		return
	}
	backups := w.listBackups(today)
	if len(backups) == 0 {
		return
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].date < backups[j].date })

	if w.maxAge > 0 {
		cutoff := w.now().AddDate(0, 0, -w.maxAge).Format(dateFmt)
		for _, b := range backups {
			if b.date < cutoff {
				_ = os.Remove(b.path)
			}
		}
	}
	if w.maxBackups > 0 {
		// Re-scan after age deletes so the survivor count is accurate.
		backups = w.listBackups(today)
		sort.Slice(backups, func(i, j int) bool { return backups[i].date < backups[j].date })
		for len(backups) > w.maxBackups {
			_ = os.Remove(backups[0].path)
			backups = backups[1:]
		}
	}
}
