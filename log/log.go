package log

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"
)

var colorize bool

func init() {
	// Only emit ANSI color codes in real terminals (TERM is set) and when
	// NO_COLOR is not set. IDEA's built-in console has no TERM, so logs
	// render as plain text without escape-code artifacts.
	colorize = os.Getenv("TERM") != "" && os.Getenv("NO_COLOR") == ""
	// Console-only until Init wires up the file sink after config is loaded.
	setDefault(nil, true)
}

// Options configures the log sink. log owns these fields instead of importing
// config so it stays dependency-light; callers map config -> Options.
//
// Rotation is daily: the active file is <File> with a -YYYY-MM-DD date suffix
// inserted before the extension, so each calendar day gets its own file.
type Options struct {
	// File is the log file path (used as the base name; .log becomes
	// <base>-YYYY-MM-DD.log). Empty or "off" disables file logging.
	File string
	// Stdout mirrors INFO/WARN to stdout and ERROR to stderr when true.
	Stdout bool
	// MaxBackups is the max number of daily backup files to retain (0 = unlimited).
	MaxBackups int
	// MaxAge is the max number of days to retain daily backup files (0 = unlimited).
	MaxAge int
	// Compress gzip-compresses each day's file after the day rolls over.
	Compress bool
}

// Init reconfigures the default slog handler to mirror output to a daily-rotated
// file (when File is set) and/or the terminal. Call once after config is
// loaded; before that the package init installs a console-only handler.
// A file-open failure degrades to console-only and is returned (not fatal),
// so a misconfigured path can't take the service down.
func Init(opts Options) error {
	var fileW io.Writer
	if !fileDisabled(opts.File) {
		w, err := newDailyRotateWriter(opts.File, opts, time.Now)
		if err != nil {
			setDefault(nil, true)
			Warnf("[log] file logging disabled, console-only: %v", err)
			return err
		}
		fileW = w
	}
	setDefault(fileW, opts.Stdout)
	if fileW != nil {
		Infof("[log] file=%s daily rotation backups=%d age=%dd compress=%v stdout=%v",
			opts.File, opts.MaxBackups, opts.MaxAge, opts.Compress, opts.Stdout)
	}
	return nil
}

func fileDisabled(path string) bool {
	p := strings.TrimSpace(path)
	return p == "" || strings.EqualFold(p, "off")
}

// setDefault installs a handler writing to file (plain) and/or console
// (colored). A nil writer drops that sink.
func setDefault(fileW io.Writer, stdout bool) {
	var consoleOut, consoleErr io.Writer
	if stdout {
		consoleOut = os.Stdout
		consoleErr = os.Stderr
	}
	slog.SetDefault(slog.New(colorHandler{
		out:  consoleOut,
		errW: consoleErr,
		file: fileW,
		opts: &slog.HandlerOptions{AddSource: true},
	}))
}

// contextKey is an unexported type used for context value keys.
type contextKey int

const traceIDKey contextKey = 0

// WithTraceID injects a trace ID into the context so downstream log calls
// can extract and render it.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey, traceID)
}

// TraceIDFromContext retrieves the trace ID, or "" if none set.
func TraceIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(traceIDKey).(string)
	return id
}

// GenerateTraceID creates a random 12-character hex trace ID.
func GenerateTraceID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(fmt.Sprintf("%x", time.Now().UnixNano())))[:12]
	}
	return hex.EncodeToString(b[:])
}

// --- context-aware logging functions ---

func InfofCtx(ctx context.Context, format string, args ...any) {
	logfCtx(ctx, slog.LevelInfo, format, args...)
}
func WarnfCtx(ctx context.Context, format string, args ...any) {
	logfCtx(ctx, slog.LevelWarn, format, args...)
}
func ErrorfCtx(ctx context.Context, format string, args ...any) {
	logfCtx(ctx, slog.LevelError, format, args...)
}
func logfCtx(ctx context.Context, level slog.Level, format string, args ...any) {
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:])
	r := slog.NewRecord(time.Now(), level, fmt.Sprintf(format, args...), pcs[0])
	_ = slog.Default().Handler().Handle(ctx, r)
}

// --- legacy functions (no context -> no traceId) ---

func Infof(format string, args ...any) {
	logfCtx(context.Background(), slog.LevelInfo, format, args...)
}
func Warnf(format string, args ...any) {
	logfCtx(context.Background(), slog.LevelWarn, format, args...)
}
func Errorf(format string, args ...any) {
	logfCtx(context.Background(), slog.LevelError, format, args...)
}
func Fatalf(format string, args ...any) {
	logfCtx(context.Background(), slog.LevelError, format, args...)
	os.Exit(1)
}

type colorHandler struct {
	out  io.Writer // INFO/WARN -> stdout (nil when stdout disabled)
	errW io.Writer // ERROR -> stderr (nil when stdout disabled)
	file io.Writer // rotated log file, plain (nil when disabled)
	opts *slog.HandlerOptions
}

func (h colorHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h colorHandler) Handle(ctx context.Context, r slog.Record) error {
	// File first, plain - ANSI codes must never land in the log file.
	if h.file != nil {
		if _, err := h.file.Write(render(r, ctx, false)); err != nil {
			// Surface disk failures rather than dropping the line silently.
			fmt.Fprintf(os.Stderr, "log file write failed: %v\n", err)
		}
	}
	// Console second, colored when in a terminal.
	var w io.Writer
	if r.Level >= slog.LevelError {
		w = h.errW
	} else {
		w = h.out
	}
	if w != nil {
		if _, err := w.Write(render(r, ctx, colorize)); err != nil {
			return err
		}
	}
	return nil
}

// render formats a record. color only toggles the WARN/ERROR label's ANSI
// codes, so the file (color=false) stays free of escape sequences while the
// terminal (color=true) keeps its colors.
func render(r slog.Record, ctx context.Context, color bool) []byte {
	buf := &bytes.Buffer{}
	buf.WriteString(r.Time.Format(time.RFC3339))
	buf.WriteByte(' ')
	if tid := TraceIDFromContext(ctx); tid != "" {
		buf.WriteString(tid)
		buf.WriteByte(' ')
	}
	switch r.Level {
	case slog.LevelWarn:
		if color {
			buf.WriteString("\033[33mWARN\033[0m")
		} else {
			buf.WriteString("WARN")
		}
	case slog.LevelError:
		if color {
			buf.WriteString("\033[31mERROR\033[0m")
		} else {
			buf.WriteString("ERROR")
		}
	default:
		buf.WriteString(r.Level.String())
	}
	buf.WriteByte(' ')
	if r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()
		if f.File != "" {
			fmt.Fprintf(buf, "source=%s:%d ", f.File, f.Line)
		}
	}
	buf.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		buf.WriteByte(' ')
		buf.WriteString(a.String())
		return true
	})
	buf.WriteByte('\n')
	return buf.Bytes()
}

func (h colorHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h colorHandler) WithGroup(string) slog.Handler      { return h }
