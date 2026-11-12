package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nicorobinreal/dev/internal/state"
)

// CheckInterval is how long a release check stays fresh.
const CheckInterval = time.Hour

// Notice records a completed swap so the next run can announce it.
type Notice struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Cache is ~/.local/state/dev/update.json.
type Cache struct {
	LastCheck time.Time `json:"last_check,omitzero"`
	LastSeen  string    `json:"last_seen,omitempty"` // latest release tag seen
	Notice    *Notice   `json:"pending_notice,omitempty"`
}

// LoadCache reads the update cache. A missing or corrupt file yields an
// empty cache (so a bad file never blocks a check).
func LoadCache(p state.Paths) Cache {
	var c Cache
	b, err := os.ReadFile(p.UpdateFile())
	if err != nil {
		return Cache{}
	}
	if json.Unmarshal(b, &c) != nil {
		return Cache{}
	}
	return c
}

func saveCache(p state.Paths, c Cache) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(p.UpdateFile(), append(b, '\n'), 0o644)
}

// modifyCache does a read-modify-write of the cache under a short-lived
// flock on update.json.lock, so a run recording last_check can't clobber a
// notice written by a concurrent background update. This lock is separate
// from (and much shorter than) the global lock.
func modifyCache(p state.Paths, f func(*Cache)) (Cache, error) {
	path := p.UpdateFile() + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Cache{}, err
	}
	lf, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return Cache{}, err
	}
	defer lf.Close()
	if err := flockRetry(lf); err != nil {
		return Cache{}, fmt.Errorf("lock %s: %w", path, err)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()
	c := LoadCache(p)
	f(&c)
	return c, saveCache(p, c)
}

func flockRetry(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// ShouldCheck reports whether the last release check is older than
// CheckInterval (or never happened, or is in the future due to clock skew).
func ShouldCheck(p state.Paths, now time.Time) bool {
	last := LoadCache(p).LastCheck
	if last.IsZero() || last.After(now.Add(time.Minute)) {
		return true
	}
	return now.Sub(last) >= CheckInterval
}

// AutoEnabled reports whether automatic background updates apply to this
// binary: DEV_NO_UPDATE is not set to a true value and current is a release
// version (see IsRelease).
func AutoEnabled(current string) bool {
	switch os.Getenv("DEV_NO_UPDATE") {
	case "1", "true", "yes", "TRUE", "YES", "True":
		return false
	}
	return IsRelease(current)
}

// MarkChecked records now as the last check time.
func MarkChecked(p state.Paths, now time.Time) error {
	_, err := modifyCache(p, func(c *Cache) { c.LastCheck = now })
	return err
}

// TakeNotice returns "dev updated vX → vY" if an update swap happened since
// the last call, and clears it, so the line prints exactly once. It returns
// "" when there is nothing to announce.
func TakeNotice(p state.Paths) (string, error) {
	if LoadCache(p).Notice == nil { // fast path: no lock, no write
		return "", nil
	}
	var n *Notice
	_, err := modifyCache(p, func(c *Cache) {
		n = c.Notice
		c.Notice = nil
	})
	if err != nil || n == nil {
		return "", err
	}
	return fmt.Sprintf("dev updated %s → %s", n.From, n.To), nil
}
