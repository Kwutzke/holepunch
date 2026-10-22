package daemon_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWritePIDFile(t *testing.T) {
	t.Parallel()

	t.Run("write and read", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.pid")

		err := daemon.WritePIDFile(path)
		require.NoError(t, err)

		pid, err := daemon.ReadPIDFile(path)
		require.NoError(t, err)
		assert.Equal(t, os.Getpid(), pid)
	})

	t.Run("detects running daemon", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.pid")

		err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
		require.NoError(t, err)

		err = daemon.WritePIDFile(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already running")
	})

	t.Run("overwrites stale PID", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.pid")

		err := os.WriteFile(path, []byte("999999999"), 0o644)
		require.NoError(t, err)

		err = daemon.WritePIDFile(path)
		require.NoError(t, err)
	})
}

func TestReadPIDFile(t *testing.T) {
	t.Parallel()

	_, err := daemon.ReadPIDFile("/nonexistent/pid")
	require.Error(t, err)
}

func TestCleanup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "daemon.sock")
	pidPath := filepath.Join(dir, "daemon.pid")

	require.NoError(t, os.WriteFile(sockPath, []byte{}, 0o644))
	require.NoError(t, os.WriteFile(pidPath, []byte{}, 0o644))

	daemon.Cleanup(sockPath, pidPath)

	_, err := os.Stat(sockPath)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(pidPath)
	assert.True(t, os.IsNotExist(err))
}
