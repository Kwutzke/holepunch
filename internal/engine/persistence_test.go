package engine_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/Kwutzke/holepunch/internal/engine"
	"github.com/Kwutzke/holepunch/internal/state"
)

func TestStatePersisterSaveAndLoad(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	persister := engine.NewStatePersister(path)

	now := time.Now()
	statuses := []engine.ServiceStatus{
		{
			Profile:     "dev",
			ServiceName: "opensearch",
			DNSName:     "opensearch.dev",
			LocalAddr:   "127.0.0.2:443",
			State:       state.Connected,
			ConnectedAt: now,
		},
		{
			Profile:     "prod",
			ServiceName: "rds",
			DNSName:     "rds.prod",
			LocalAddr:   "127.0.0.3:5432",
			State:       state.Failed,
			Error:       assert.AnError,
		},
	}

	err := persister.Save(statuses)
	require.NoError(t, err)

	loaded, err := persister.Load()
	require.NoError(t, err)
	require.Len(t, loaded.Services, 2)

	assert.Equal(t, "dev", loaded.Services[0].Profile)
	assert.Equal(t, "opensearch", loaded.Services[0].ServiceName)
	assert.Equal(t, "opensearch.dev", loaded.Services[0].DNSName)
	assert.Equal(t, "127.0.0.2:443", loaded.Services[0].LocalAddr)
	assert.Equal(t, "Connected", loaded.Services[0].State)
	assert.NotEmpty(t, loaded.Services[0].ConnectedAt)

	assert.Equal(t, "prod", loaded.Services[1].Profile)
	assert.Equal(t, "Failed", loaded.Services[1].State)
	assert.Equal(t, "assert.AnError general error for testing", loaded.Services[1].Error)
	assert.Empty(t, loaded.Services[1].ConnectedAt)

	assert.False(t, loaded.UpdatedAt.IsZero())
}

func TestStatePersisterSaveEmpty(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	persister := engine.NewStatePersister(path)

	err := persister.Save(nil)
	require.NoError(t, err)

	loaded, err := persister.Load()
	require.NoError(t, err)
	assert.Empty(t, loaded.Services)
}

func TestStatePersisterLoadNonexistent(t *testing.T) {
	t.Parallel()

	persister := engine.NewStatePersister("/nonexistent/state.json")
	_, err := persister.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading state")
}

func TestStatePersisterRemove(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	persister := engine.NewStatePersister(path)

	require.NoError(t, persister.Save(nil))
	require.NoError(t, persister.Remove())

	_, err := persister.Load()
	require.Error(t, err)
}

func TestStatePersisterRemoveNonexistent(t *testing.T) {
	t.Parallel()

	persister := engine.NewStatePersister(filepath.Join(t.TempDir(), "nope.json"))
	err := persister.Remove()
	require.NoError(t, err, "removing nonexistent file should not error")
}

func TestStatePersisterCreatesDirectory(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "dir", "state.json")
	persister := engine.NewStatePersister(path)

	err := persister.Save([]engine.ServiceStatus{
		{Profile: "dev", ServiceName: "svc", State: state.Connected},
	})
	require.NoError(t, err)

	loaded, err := persister.Load()
	require.NoError(t, err)
	require.Len(t, loaded.Services, 1)
}
