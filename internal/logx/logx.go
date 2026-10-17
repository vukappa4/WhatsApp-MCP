// Package logx writes the daemon log to a size-rotated file and points
// runtime crash output at the same file.
package logx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	maxSize  = 10 << 20 // rotate when the active file passes 10 MB
	keepOlds = 5        // whatsapp-mcp.log.1 .. .5
)

// RotatingFile is an io.Writer over <dir>/whatsapp-mcp.log. On rotation it
// moves the crash output to the new file, so panics land next to the log.
type RotatingFile struct {
	mu   sync.Mutex
	base string // whatsapp-mcp.log
	path string // the file written now: base, or a numbered fallback
	f    *os.File
	size int64
}

// OpenRotating opens (or creates) the log file in dir and sets it as crash output.
func OpenRotating(dir string) (*RotatingFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	base := filepath.Join(dir, "whatsapp-mcp.log")
	r := &RotatingFile{base: base, path: base}
	if err := r.open(); err != nil {
		return nil, err
	}
	if r.size > maxSize {
		if err := r.rotate(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return debug.SetCrashOutput(f, debug.CrashOptions{})
}

// rotate requires every handle on the file to be closed first: Windows refuses
// to rename an open file, and SetCrashOutput holds a duplicated handle.
//
// When the rename fails (another process holds the file open), logging moves
// to a new numbered file instead of retrying the rotation on every write.
func (r *RotatingFile) rotate() error {
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
	r.f.Close()
	for i := keepOlds - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", r.base, i), fmt.Sprintf("%s.%d", r.base, i+1))
	}
	if err := os.Rename(r.path, r.base+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.path = fmt.Sprintf("%s.%d.log", strings.TrimSuffix(r.base, ".log"), time.Now().UnixNano())
	} else {
		r.path = r.base
	}
	return r.open()
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// An empty file is never rotated: that would push the oldest log out for nothing.
	if r.size > 0 && r.size+int64(len(p)) > maxSize {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
	return r.f.Close()
}

// New returns a text slog logger on w. Debug level when debug is true.
func New(w io.Writer, debugLevel bool) *slog.Logger {
	lvl := slog.LevelInfo
	if debugLevel {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl}))
}

// Quiet returns a logger that writes Info records at Debug level and keeps
// warnings and errors. The MCP SDK logs every stateless request ("server
// connecting", "session initialized"...) at Info. Nil stays nil.
func Quiet(l *slog.Logger) *slog.Logger {
	if l == nil {
		return nil
	}
	return slog.New(demote{l.Handler()})
}

type demote struct{ h slog.Handler }

func lower(l slog.Level) slog.Level {
	if l == slog.LevelInfo {
		return slog.LevelDebug
	}
	return l
}

func (d demote) Enabled(ctx context.Context, l slog.Level) bool { return d.h.Enabled(ctx, lower(l)) }
func (d demote) Handle(ctx context.Context, r slog.Record) error {
	r.Level = lower(r.Level)
	return d.h.Handle(ctx, r)
}
func (d demote) WithAttrs(as []slog.Attr) slog.Handler { return demote{d.h.WithAttrs(as)} }
func (d demote) WithGroup(name string) slog.Handler    { return demote{d.h.WithGroup(name)} }

// WA adapts slog to whatsmeow's logger interface.
func WA(l *slog.Logger, module string) waLog.Logger {
	return waLogger{base: l, l: l.With("module", module), module: module}
}

type waLogger struct {
	base   *slog.Logger
	l      *slog.Logger
	module string
}

func (w waLogger) Warnf(msg string, args ...any)  { w.l.Warn(fmt.Sprintf(msg, args...)) }
func (w waLogger) Errorf(msg string, args ...any) { w.l.Error(fmt.Sprintf(msg, args...)) }
func (w waLogger) Infof(msg string, args ...any)  { w.l.Info(fmt.Sprintf(msg, args...)) }
func (w waLogger) Debugf(msg string, args ...any) { w.l.Debug(fmt.Sprintf(msg, args...)) }
func (w waLogger) Sub(module string) waLog.Logger {
	return WA(w.base, strings.TrimPrefix(w.module+"/"+module, "/"))
}
