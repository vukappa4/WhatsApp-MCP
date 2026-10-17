package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Settings are the optional user settings of a profile, read from
// <data dir>\config.json. A missing file means the defaults.
type Settings struct {
	// SendFileRoots replaces the folders whatsapp_send_file may read from
	// (absolute paths). Empty keeps the defaults.
	SendFileRoots []string `json:"send_file_roots,omitempty"`
	// SendFileExtraRoots adds folders to the default (or replaced) list.
	SendFileExtraRoots []string `json:"send_file_extra_roots,omitempty"`
}

// SettingsFile is <data dir>\config.json.
func SettingsFile(dataDir string) string { return filepath.Join(dataDir, "config.json") }

// LoadSettings reads <dataDir>\config.json. Unknown fields and relative
// paths are errors: a typo must not silently fall back to the defaults.
func LoadSettings(dataDir string) (Settings, error) {
	var s Settings
	path := SettingsFile(dataDir)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF"))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Settings{}, fmt.Errorf("%s: %w", path, err)
	}
	for _, list := range [][]string{s.SendFileRoots, s.SendFileExtraRoots} {
		for _, r := range list {
			if !filepath.IsAbs(r) {
				return Settings{}, fmt.Errorf("%s: send-file folder %q is not an absolute path", path, r)
			}
		}
	}
	return s, nil
}
