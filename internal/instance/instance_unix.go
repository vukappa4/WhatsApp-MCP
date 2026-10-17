//go:build unix

// Package instance guarantees one daemon per profile with an exclusive
// flock on <AppDir>/locks/<profile>.lock.
package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

// State is who holds a profile lock.
type State int

const (
	Free             State = iota // no process holds it
	HeldHere                      // a process of this user holds it
	HeldInaccessible              // this user may not open the lock file (another user's, or root's)
)

// ErrRunning means another process holds the profile lock.
var ErrRunning = errors.New("another whatsapp-mcp process is already running for this profile")

// Lock is an open lock file holding an exclusive flock. The kernel drops the
// flock when the process exits, however it exits, so a crash never leaves a
// stale lock. The file itself stays: deleting it while held would let a
// second process lock a new file of the same name.
type Lock struct{ f *os.File }

// lockPath is the profile's lock file. It sits in the app dir, never in a
// temp dir, where cleaners delete old files.
func lockPath(profile string) (string, error) {
	root, err := config.AppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "locks", profile+".lock"), nil
}

// acquireTries and acquireRetry let Acquire ride out a Probe, which holds a
// shared flock for an instant.
const (
	acquireTries = 5
	acquireRetry = 20 * time.Millisecond
)

// Acquire takes the profile's exclusive flock.
func Acquire(profile string) (*Lock, error) {
	path, err := lockPath(profile)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// 0644: a status run by another account can still open it to probe.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	for i := 0; ; i++ {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if i == acquireTries-1 {
			f.Close()
			return nil, ErrRunning
		}
		time.Sleep(acquireRetry)
	}
}

// Held reports whether some process currently holds the profile lock.
func Held(profile string) bool { return Probe(profile) != Free }

// Probe reports who holds the profile lock. It opens the file anew and
// tries a shared flock: flocks belong to each open file, so this sees a
// lock held by this very process too.
func Probe(profile string) State {
	path, err := lockPath(profile)
	if err != nil {
		return Free
	}
	f, err := os.Open(path)
	switch {
	case errors.Is(err, os.ErrPermission):
		return HeldInaccessible
	case err != nil:
		return Free
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return HeldHere
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return Free
}

// Release drops the flock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	l.f.Close()
	l.f = nil
}
