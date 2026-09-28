// Package recent remembers the selections of the last run, such as the
// source folder that was split into ISO files and the ISO files that were
// burned, so the next run can start from them.
package recent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Recent is what the last run selected. Empty fields were never chosen.
type Recent struct {
	// Folder is the source folder last split into ISO files.
	Folder string `json:"folder,omitempty"`
	// Pattern is the file-selection regex last used for generating.
	Pattern string `json:"pattern,omitempty"`
	// ISOName is the ISO name last used for generating.
	ISOName string `json:"iso_name,omitempty"`
	// Preset is the name of the ISO size preset last used for generating.
	Preset string `json:"preset,omitempty"`
	// ISODir is the folder the ISO picker was showing when ISOs were last
	// chosen for burning.
	ISODir string `json:"iso_dir,omitempty"`
	// ISOs are the ISO files last chosen for burning.
	ISOs []string `json:"isos,omitempty"`
	// Speed is the write speed last chosen for burning, as a multiple of
	// 1x; 0 means the fastest.
	Speed int `json:"speed,omitempty"`
}

// DefaultPath returns where selections are remembered, next to the burn
// database in the user config directory.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "iso-burner", "recent.json"), nil
}

// Load reads the selections saved at path. A missing file is not an error;
// it yields empty selections.
func Load(path string) (Recent, error) {
	var r Recent
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Recent{}, err
	}
	return r, nil
}

// Save writes r to path, replacing it atomically.
func Save(path string, r Recent) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".recent-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
