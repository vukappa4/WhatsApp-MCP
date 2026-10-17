//go:build windows

package childproc

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestMain doubles as the helper processes of TestChildrenDieWithParent.
func TestMain(m *testing.M) {
	switch os.Getenv("CHILDPROC_TEST_ROLE") {
	case "parent":
		if err := KillChildrenOnExit(); err != nil {
			os.Stdout.WriteString("error " + err.Error() + "\n")
			os.Exit(1)
		}
		self, _ := os.Executable()
		child := exec.Command(self)
		child.Env = append(os.Environ(), "CHILDPROC_TEST_ROLE=child")
		Hide(child)
		if err := child.Start(); err != nil {
			os.Stdout.WriteString("error " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString(strconv.Itoa(child.Process.Pid) + "\n")
		os.Exit(0) // ends without waiting: like a crashed daemon
	case "child":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A child (a transcription) dies with the daemon, however the daemon ends.
func TestChildrenDieWithParent(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(self)
	parent.Env = append(os.Environ(), "CHILDPROC_TEST_ROLE=parent")
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	parent.Wait()
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("parent said %q", line)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return // already gone
	}
	defer windows.CloseHandle(h)
	if ev, _ := windows.WaitForSingleObject(h, 10000); ev != windows.WAIT_OBJECT_0 {
		windows.TerminateProcess(h, 1)
		t.Fatal("the child outlived its parent")
	}
}
