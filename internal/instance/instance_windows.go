//go:build windows

// Package instance guarantees one daemon per profile with a named mutex.
package instance

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// State is who holds a profile lock.
type State int

const (
	Free             State = iota // no process holds it
	HeldHere                      // a process this user can open holds it
	HeldInaccessible              // held, but this user may not open it (another user, or elevated)
)

// ErrRunning means another process holds the profile lock.
var ErrRunning = errors.New("another whatsapp-mcp process is already running for this profile")

// Lock is an open handle on the named mutex. Its existence is the lock;
// Windows closes the handle when the process exits.
type Lock struct{ h windows.Handle }

// Acquire takes the mutex Global\whatsapp-mcp-<profile>. Global, not Local:
// two logon sessions (or an elevated and a normal shell) must not both run a profile.
func Acquire(profile string) (*Lock, error) {
	name, err := windows.UTF16PtrFromString(`Global\whatsapp-mcp-` + profile)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return nil, ErrRunning
	}
	if err != nil {
		return nil, fmt.Errorf("create mutex: %w", err)
	}
	return &Lock{h: h}, nil
}

// Held reports whether some process currently holds the profile mutex.
func Held(profile string) bool { return Probe(profile) != Free }

// Probe reports who holds the profile mutex: nobody, a process this user
// can open, or a process whose mutex this user may not open (another user,
// or an elevated process seen from a normal one).
func Probe(profile string) State {
	name, err := windows.UTF16PtrFromString(`Global\whatsapp-mcp-` + profile)
	if err != nil {
		return Free
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, name)
	switch {
	case err == nil:
		windows.CloseHandle(h)
		return HeldHere
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return HeldInaccessible
	}
	return Free
}

// Release frees the mutex.
func (l *Lock) Release() {
	if l == nil || l.h == 0 {
		return
	}
	windows.CloseHandle(l.h)
	l.h = 0
}
