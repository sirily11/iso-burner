// Package logfile sends the default slog logger to a file, since the TUI owns
// the terminal and log lines on stderr would corrupt it.
package logfile

import (
	"log/slog"
	"os"
	"path/filepath"
)

// maxSize is how large the log may grow before it is rotated to "<path>.1".
const maxSize = 5 << 20

// DefaultPath returns where the debug log is kept: iso-burner.log in the user
// config directory, next to the burn database.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "iso-burner", "iso-burner.log"), nil
}

// Open appends the default slog logger's records to the file at path,
// rotating it once it grows past maxSize. Records are written unbuffered, so
// the file needs no closing. If it cannot be opened, logging is discarded
// rather than written to stderr.
func Open(path string) error {
	f, err := open(path)
	if err != nil {
		slog.SetDefault(slog.New(slog.DiscardHandler))
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return nil
}

func open(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxSize {
		os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
