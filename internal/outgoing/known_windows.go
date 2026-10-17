//go:build windows

package outgoing

import "golang.org/x/sys/windows"

// knownFolders are the user's Desktop, Documents, Downloads, Pictures,
// Videos and Music wherever Windows keeps them (moved to another drive or
// redirected to OneDrive).
func knownFolders() []string {
	var out []string
	for _, id := range []*windows.KNOWNFOLDERID{windows.FOLDERID_Desktop, windows.FOLDERID_Documents, windows.FOLDERID_Downloads,
		windows.FOLDERID_Pictures, windows.FOLDERID_Videos, windows.FOLDERID_Music} {
		if p, err := windows.KnownFolderPath(id, 0); err == nil && p != "" {
			out = append(out, p)
		}
	}
	return out
}
