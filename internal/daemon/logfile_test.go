package daemon_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const logCap = 10 << 20

func TestOpenLog(t *testing.T) {
	t.Parallel()

	t.Run("a second launch appends to the first launch's log, stderr included", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), ".holepunch", "daemon.log")

		launchLogging(t, path, `echo "first launch"; echo "aws: first stderr" >&2`)
		launchLogging(t, path, `echo "second launch"`)

		b, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "first launch\naws: first stderr\nsecond launch\n", string(b))
	})

	t.Run("a log above 10 MiB moves to daemon.log.1", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.log")
		require.NoError(t, os.WriteFile(path+".1", []byte("older still\n"), 0o600))
		require.NoError(t, os.WriteFile(path, []byte("before the restart\n"), 0o600))
		require.NoError(t, os.Truncate(path, logCap+1))

		launchLogging(t, path, `echo "after the restart"`)

		b, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "after the restart\n", string(b))
		info, err := os.Stat(path + ".1")
		require.NoError(t, err)
		assert.EqualValues(t, logCap+1, info.Size(), "the full log moved, replacing the older one")
	})

	t.Run("a log at 10 MiB stays", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.log")
		require.NoError(t, os.Truncate(createEmpty(t, path), logCap))

		launchLogging(t, path, `echo "appended"`)

		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.EqualValues(t, logCap+len("appended\n"), info.Size())
		assert.NoFileExists(t, path+".1")
	})
}

// launchLogging runs script with its stdout and stderr on the log, as the daemon is launched.
func launchLogging(t *testing.T, path, script string) {
	t.Helper()
	logFile, err := daemon.OpenLog(path)
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	require.NoError(t, cmd.Run())
	require.NoError(t, logFile.Close())
}

func createEmpty(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	return path
}
