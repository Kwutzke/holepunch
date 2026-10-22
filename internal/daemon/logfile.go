package daemon

import (
	"fmt"
	"os"
	"path/filepath"
)

// logMaxBytes is the size above which OpenLog moves the log aside before appending to a fresh one.
const logMaxBytes = 10 << 20

// DefaultLogPath returns the daemon log path.
func DefaultLogPath() string {
	return filepath.Join(DefaultDir(), "daemon.log")
}

// OpenLog opens the daemon log at path for appending. A log above 10 MiB is first renamed to
// path.1, replacing the previous one.
func OpenLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}
	if info, err := os.Stat(path); err == nil && info.Size() > logMaxBytes {
		if err := os.Rename(path, path+".1"); err != nil {
			return nil, fmt.Errorf("rotating daemon log: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening daemon log: %w", err)
	}
	return f, nil
}
