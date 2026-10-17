//go:build !windows

package outgoing

// knownFolders adds nothing outside Windows: macOS keeps the user folders
// at fixed places in the home folder, which DefaultRoots lists.
func knownFolders() []string { return nil }
