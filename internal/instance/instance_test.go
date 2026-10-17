package instance

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMain points the app dir, where Unix lock files live, at a temp dir.
func TestMain(m *testing.M) {
	if runtime.GOOS != "windows" {
		home, err := os.MkdirTemp("", "instance-test-")
		if err != nil {
			panic(err)
		}
		os.Setenv("HOME", home)
		os.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
		code := m.Run()
		os.RemoveAll(home)
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func TestSecondAcquireRefused(t *testing.T) {
	l, err := Acquire("unit-test")
	if err != nil {
		t.Fatal(err)
	}
	if !Held("unit-test") {
		t.Fatal("Held must see the lock")
	}
	if _, err := Acquire("unit-test"); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Acquire = %v, want ErrRunning", err)
	}
	l.Release()
	if Held("unit-test") {
		t.Fatal("lock still held after Release")
	}
}

// A lock this user may not open (another user's, or an elevated process's)
// still counts as held.
func TestProbeInaccessibleLock(t *testing.T) {
	release := createLockedDown(t, "unit-test-denied")
	defer release()
	if got := Probe("unit-test-denied"); got != HeldInaccessible {
		t.Fatalf("Probe = %v, want HeldInaccessible", got)
	}
	if !Held("unit-test-denied") {
		t.Fatal("an inaccessible lock must count as held")
	}
	if Probe("unit-test-nobody") != Free {
		t.Fatal("free lock reported held")
	}
}
