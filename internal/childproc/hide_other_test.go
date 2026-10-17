//go:build !windows

package childproc

import (
	"os/exec"
	"strings"
	"testing"
)

// A background child runs through nice, 10 steps below this process, with
// its own arguments intact (nice execs it, so its parent is this process).
func TestBackgroundRunsThroughNice(t *testing.T) {
	c := exec.Command("sh", "-c", "echo $(( $(ps -o nice= -p $$) - $(ps -o nice= -p $PPID) ))")
	Background(c)
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "10" {
		t.Fatalf("child niceness = parent + %q, want parent + 10", got)
	}
}
