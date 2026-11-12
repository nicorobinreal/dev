package state

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger appends to ~/.local/state/dev/logs/<date>.log and, with --verbose,
// mirrors to stderr.
type Logger struct {
	mu     sync.Mutex
	f      *os.File
	Mirror io.Writer
	prefix string
}

// OpenLogger opens today's log file. Failures degrade to a no-op logger.
func OpenLogger(p Paths, verbose bool) *Logger {
	l := &Logger{prefix: fmt.Sprintf("[%d] ", os.Getpid())}
	if verbose {
		l.Mirror = os.Stderr
	}
	if err := os.MkdirAll(p.LogsDir(), 0o755); err == nil {
		name := filepath.Join(p.LogsDir(), time.Now().Format("2006-01-02")+".log")
		if f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			l.f = f
		}
	}
	return l
}

// Logf writes one timestamped line.
func (l *Logger) Logf(format string, args ...any) {
	if l == nil {
		return
	}
	line := time.Now().Format("15:04:05.000 ") + l.prefix + fmt.Sprintf(format, args...) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_, _ = l.f.WriteString(line)
	}
	if l.Mirror != nil {
		_, _ = io.WriteString(l.Mirror, line)
	}
}

// Close closes the file.
func (l *Logger) Close() {
	if l != nil && l.f != nil {
		_ = l.f.Close()
	}
}
