package dns_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/Kwutzke/holepunch/internal/dns"
)

// directWrite writes directly to the file without sudo — used for testing.
func directWrite(_ context.Context, path string, content []byte) error {
	return os.WriteFile(path, content, 0o644)
}

func setupHostsFile(t *testing.T, initialContent string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	err := os.WriteFile(path, []byte(initialContent), 0o644)
	require.NoError(t, err)
	return path
}

func readHostsFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(content)
}

func TestAdd(t *testing.T) {
	t.Parallel()

	t.Run("adds block to empty hosts file", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		path := setupHostsFile(t, "127.0.0.1 localhost\n")
		mgr := dns.New(path, directWrite)

		err := mgr.Add(ctx, []dns.Entry{
			{IP: net.IPv4(127, 0, 0, 2), Hostname: "opensearch.dev"},
		})
		require.NoError(t, err)

		content := readHostsFile(t, path)
		assert.Contains(t, content, "# BEGIN holepunch")
		assert.Contains(t, content, "127.0.0.2 opensearch.dev")
		assert.Contains(t, content, "# END holepunch")
		assert.Contains(t, content, "127.0.0.1 localhost")
	})

	t.Run("appends to existing managed block", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		path := setupHostsFile(t, "127.0.0.1 localhost\n# BEGIN holepunch\n127.0.0.2 opensearch.dev\n# END holepunch\n")
		mgr := dns.New(path, directWrite)

		err := mgr.Add(ctx, []dns.Entry{
			{IP: net.IPv4(127, 0, 0, 3), Hostname: "rds.dev"},
		})
		require.NoError(t, err)

		content := readHostsFile(t, path)
		assert.Contains(t, content, "127.0.0.2 opensearch.dev")
		assert.Contains(t, content, "127.0.0.3 rds.dev")
	})

	t.Run("idempotent for same hostname", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		path := setupHostsFile(t, "127.0.0.1 localhost\n")
		mgr := dns.New(path, directWrite)

		entry := dns.Entry{IP: net.IPv4(127, 0, 0, 2), Hostname: "opensearch.dev"}
		require.NoError(t, mgr.Add(ctx, []dns.Entry{entry}))
		require.NoError(t, mgr.Add(ctx, []dns.Entry{entry}))

		content := readHostsFile(t, path)
		// Should appear exactly once.
		count := countOccurrences(content, "opensearch.dev")
		assert.Equal(t, 1, count, "hostname should appear exactly once")
	})

	t.Run("multiple entries at once", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		path := setupHostsFile(t, "127.0.0.1 localhost\n")
		mgr := dns.New(path, directWrite)

		err := mgr.Add(ctx, []dns.Entry{
			{IP: net.IPv4(127, 0, 0, 2), Hostname: "opensearch.dev"},
			{IP: net.IPv4(127, 0, 0, 3), Hostname: "rds.dev"},
			{IP: net.IPv4(127, 0, 0, 4), Hostname: "redis.dev"},
		})
		require.NoError(t, err)

		content := readHostsFile(t, path)
		assert.Contains(t, content, "127.0.0.2 opensearch.dev")
		assert.Contains(t, content, "127.0.0.3 rds.dev")
		assert.Contains(t, content, "127.0.0.4 redis.dev")
	})

	t.Run("preserves content after managed block", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		path := setupHostsFile(t, "127.0.0.1 localhost\n# BEGIN holepunch\n127.0.0.2 old.dev\n# END holepunch\n192.168.1.1 other.host\n")
		mgr := dns.New(path, directWrite)

		err := mgr.Add(ctx, []dns.Entry{
			{IP: net.IPv4(127, 0, 0, 3), Hostname: "new.dev"},
		})
		require.NoError(t, err)

		content := readHostsFile(t, path)
		assert.Contains(t, content, "127.0.0.1 localhost")
		assert.Contains(t, content, "127.0.0.2 old.dev")
		assert.Contains(t, content, "127.0.0.3 new.dev")
		assert.Contains(t, content, "192.168.1.1 other.host")
	})
}

func TestRemove(t *testing.T) {
	t.Parallel()

	t.Run("removes managed block", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		path := setupHostsFile(t, "127.0.0.1 localhost\n# BEGIN holepunch\n127.0.0.2 opensearch.dev\n# END holepunch\n")
		mgr := dns.New(path, directWrite)

		err := mgr.Remove(ctx)
		require.NoError(t, err)

		content := readHostsFile(t, path)
		assert.NotContains(t, content, "holepunch")
		assert.NotContains(t, content, "opensearch.dev")
		assert.Contains(t, content, "127.0.0.1 localhost")
	})

	t.Run("noop when no managed block exists", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		original := "127.0.0.1 localhost\n"
		path := setupHostsFile(t, original)
		mgr := dns.New(path, directWrite)

		err := mgr.Remove(ctx)
		require.NoError(t, err)

		content := readHostsFile(t, path)
		assert.Contains(t, content, "localhost")
	})

	t.Run("preserves surrounding content", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		path := setupHostsFile(t, "127.0.0.1 localhost\n# BEGIN holepunch\n127.0.0.2 test.dev\n# END holepunch\n192.168.1.1 other.host\n")
		mgr := dns.New(path, directWrite)

		err := mgr.Remove(ctx)
		require.NoError(t, err)

		content := readHostsFile(t, path)
		assert.Contains(t, content, "127.0.0.1 localhost")
		assert.Contains(t, content, "192.168.1.1 other.host")
		assert.NotContains(t, content, "test.dev")
	})
}

func TestRemoveEntries(t *testing.T) {
	t.Parallel()

	t.Run("removes specific entries", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		path := setupHostsFile(t, "127.0.0.1 localhost\n# BEGIN holepunch\n127.0.0.2 opensearch.dev\n127.0.0.3 rds.dev\n127.0.0.4 redis.dev\n# END holepunch\n")
		mgr := dns.New(path, directWrite)

		err := mgr.RemoveEntries(ctx, []dns.Entry{
			{IP: net.IPv4(127, 0, 0, 3), Hostname: "rds.dev"},
		})
		require.NoError(t, err)

		content := readHostsFile(t, path)
		assert.Contains(t, content, "opensearch.dev")
		assert.NotContains(t, content, "rds.dev")
		assert.Contains(t, content, "redis.dev")
	})

	t.Run("removes block when last entry removed", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		path := setupHostsFile(t, "127.0.0.1 localhost\n# BEGIN holepunch\n127.0.0.2 only.dev\n# END holepunch\n")
		mgr := dns.New(path, directWrite)

		err := mgr.RemoveEntries(ctx, []dns.Entry{
			{IP: net.IPv4(127, 0, 0, 2), Hostname: "only.dev"},
		})
		require.NoError(t, err)

		content := readHostsFile(t, path)
		assert.NotContains(t, content, "holepunch")
	})
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	path := setupHostsFile(t, "127.0.0.1 localhost\n")
	mgr := dns.New(path, directWrite)

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			entry := dns.Entry{
				IP:       net.IPv4(127, 0, 0, byte(idx+2)),
				Hostname: fmt.Sprintf("svc%d.dev", idx),
			}
			err := mgr.Add(ctx, []dns.Entry{entry})
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()

	content := readHostsFile(t, path)
	assert.Contains(t, content, "# BEGIN holepunch")
	assert.Contains(t, content, "# END holepunch")
}

func TestEntryString(t *testing.T) {
	t.Parallel()

	e := dns.Entry{IP: net.IPv4(127, 0, 0, 5), Hostname: "test.dev"}
	assert.Equal(t, "127.0.0.5 test.dev", e.String())
}

func countOccurrences(s, substr string) int {
	count := 0
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			count++
		}
	}
	return count
}
