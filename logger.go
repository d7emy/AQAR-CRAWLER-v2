package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// maxLogSizeBytes: when the log file exceeds this, it is rotated to
// crawler.log.1 (previous .1 is discarded).
const maxLogSizeBytes = 20 << 20 // 20 MB

// Logger writes leveled messages to the console and to a file in logDir.
// The file is size-rotated; a fresh section is logged on every process start.
type Logger struct {
	mu    sync.Mutex
	l     *log.Logger
	out   *os.File
	path  string
	quiet bool // TUI/web mode: console output suppressed
}

func NewLogger(dir string) (*Logger, error) {
	if dir == "" {
		dir = "logs"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating log dir: %w", err)
	}
	path := filepath.Join(dir, "crawler.log")
	f, err := openLogFile(path)
	if err != nil {
		return nil, err
	}
	l := log.New(io.MultiWriter(os.Stdout, f), "", log.LstdFlags|log.Lmicroseconds)
	return &Logger{l: l, out: f, path: path}, nil
}

func openLogFile(path string) (*os.File, error) {
	// rotate if the existing log is too big
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogSizeBytes {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// SetQuiet switches between console+file and file-only output.
func (lg *Logger) SetQuiet(q bool) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.quiet = q
	if q {
		lg.l.SetOutput(lg.out)
	} else {
		lg.l.SetOutput(io.MultiWriter(os.Stdout, lg.out))
	}
}

// maybeRotate checks the log size and rotates if needed.
func (lg *Logger) maybeRotate() {
	fi, err := os.Stat(lg.path)
	if err != nil || fi.Size() <= maxLogSizeBytes {
		return
	}
	lg.out.Close()
	f, err := openLogFile(lg.path)
	if err != nil {
		return
	}
	lg.out = f
	if lg.quiet {
		lg.l.SetOutput(lg.out)
	} else {
		lg.l.SetOutput(io.MultiWriter(os.Stdout, lg.out))
	}
}

func (lg *Logger) Infof(format string, args ...any) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.l.Printf("INFO  "+format, args...)
	lg.maybeRotate()
}

func (lg *Logger) Warnf(format string, args ...any) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.l.Printf("WARN  "+format, args...)
	lg.maybeRotate()
}

func (lg *Logger) Errorf(format string, args ...any) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.l.Printf("ERROR "+format, args...)
	lg.maybeRotate()
}

func (lg *Logger) Fatalf(format string, args ...any) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.l.Printf("FATAL "+format, args...)
	lg.maybeRotate()
}

// APIRejectedWarning is the loud, unambiguous message printed when the API
// stops accepting our requests (e.g. Cloudflare started blocking us, or the
// server now requires headers we do not send).
func (lg *Logger) APIRejectedWarning() {
	const msg = `
========================================================================
  The AQAR API is REFUSING our requests.
  The crawler stopped on purpose (nothing was lost; the DB is safe).
  Possible causes:
    - Cloudflare/anti-bot started blocking this machine/IP.
    - The API now requires extra headers or a valid Req-Hash pair.
  Fix: re-capture a fresh session from the AQAR-Saudi app with Burp,
  compare the request headers, and update config.json. Then start the
  crawler again - it resumes where it stopped.
========================================================================`
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.l.Print(msg)
	lg.maybeRotate()
}

func (lg *Logger) Close() {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if lg.out != nil {
		lg.out.Close()
	}
}
