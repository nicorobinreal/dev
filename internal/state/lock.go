package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrLockHeld means another invocation holds the lock (exit code 3).
var ErrLockHeld = errors.New("lock held by another dev invocation")

// LockWait is how long Lock waits before giving up.
var LockWait = 30 * time.Second

// Lock is a held flock.
type Lock struct{ f *os.File }

// LockProject locks ~/.local/state/dev/locks/<name>.lock.
func LockProject(p Paths, name string) (*Lock, error) {
	return lockFile(filepath.Join(p.LocksDir(), name+".lock"))
}

// LockGlobal locks the global lock used for self-update and SSH config writes.
func LockGlobal(p Paths) (*Lock, error) {
	return lockFile(filepath.Join(p.LocksDir(), "_global.lock"))
}

func lockFile(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(LockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("%w: %s", ErrLockHeld, path)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Unlock releases the lock. Safe on nil.
func (l *Lock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
