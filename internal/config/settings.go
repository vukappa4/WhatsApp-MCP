package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// UpdateCheck false stops the daemon's daily question to GitHub for a
	// newer release. Unset or true keeps it.
	UpdateCheck *bool `json:"update_check,omitempty"`
}

// UpdateCheckOn reports whether the daemon may ask GitHub for a newer release.
func (s Settings) UpdateCheckOn() bool { return s.UpdateCheck == nil || *s.UpdateCheck }

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
	// Decode stops after the first value: a stray brace would hide the rest.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Settings{}, fmt.Errorf("%s: unexpected content after the settings object", path)
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
