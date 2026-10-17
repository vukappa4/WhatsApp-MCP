//go:build darwin

package outgoing

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// userTempScratch adds Claude Code's folders in the user's own temp dir
// (/var/folders/.../T), asked from the OS: a daemon started by launchd may
// get no TMPDIR, while Claude Code in a terminal has it.
var userTempScratch = sync.OnceValue(func() []string {
	out, err := exec.Command("/usr/bin/getconf", "DARWIN_USER_TEMP_DIR").Output()
	dir := strings.TrimSpace(string(out))
	if err != nil || !filepath.IsAbs(dir) {
		return nil
	}
	return []string{filepath.Join(dir, "claude"), filepath.Join(dir, "claude-"+strconv.Itoa(os.Getuid()))}
})
