package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// DefaultDir returns the default directory for daemon state files.
func DefaultDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".holepunch")
}

// DefaultSocketPath returns the default unix socket path.
func DefaultSocketPath() string {
	return filepath.Join(DefaultDir(), "daemon.sock")
}

// DefaultPIDPath returns the default PID file path.
func DefaultPIDPath() string {
	return filepath.Join(DefaultDir(), "daemon.pid")
}

// WritePIDFile writes the current process PID to the given path.
// Returns an error if another daemon is already running.
func WritePIDFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating PID directory: %w", err)
	}

	// Check if a PID file already exists with a running process.
	if existing, err := ReadPIDFile(path); err == nil {
		if isProcessRunning(existing) {
			return fmt.Errorf("daemon already running with PID %d", existing)
		}
		// Stale PID file — remove it.
	}

	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// ReadPIDFile reads the PID from the given file.
func ReadPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid PID file content: %w", err)
	}
	return pid, nil
}

// RemovePIDFile removes the PID file.
func RemovePIDFile(path string) error {
	return os.Remove(path)
}

// Cleanup removes the socket and PID files.
func Cleanup(socketPath, pidPath string) {
	os.Remove(socketPath)
	os.Remove(pidPath)
}

func isProcessRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks if process exists without sending a signal.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}
