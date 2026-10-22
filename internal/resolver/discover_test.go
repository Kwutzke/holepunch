package resolver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverFromConfig(t *testing.T) {
	t.Parallel()

	t.Run("reads nameservers", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			content  string
			selfAddr string
			want     []string
		}{
			{
				name:     "filters self and preserves others",
				content:  "nameserver 127.0.0.1\nnameserver 10.0.0.1\nnameserver 8.8.8.8\n",
				selfAddr: "127.0.0.1:15353",
				want:     []string{"10.0.0.1:53", "8.8.8.8:53"},
			},
			{
				name:     "keeps all when self not listed",
				content:  "nameserver 10.0.0.1\nnameserver 8.8.8.8\n",
				selfAddr: "127.0.0.1:15353",
				want:     []string{"10.0.0.1:53", "8.8.8.8:53"},
			},
			{
				name:     "empty when only self",
				content:  "nameserver 127.0.0.1\n",
				selfAddr: "127.0.0.1:15353",
				want:     []string{},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				path := filepath.Join(t.TempDir(), "resolv.conf")
				require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o644))

				got, err := DiscoverFromConfig(path, tt.selfAddr)
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			})
		}
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()

		_, err := DiscoverFromConfig(filepath.Join(t.TempDir(), "nope.conf"), "127.0.0.1:15353")
		assert.Error(t, err)
	})
}

func Test_discoverFromFirst(t *testing.T) {
	t.Parallel()

	t.Run("prefers the systemd upstream file over the stub", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		systemdPath := filepath.Join(dir, "systemd-resolv.conf")
		stubPath := filepath.Join(dir, "resolv.conf")
		require.NoError(t, os.WriteFile(systemdPath, []byte("nameserver 127.0.0.1\nnameserver 10.0.0.2\n"), 0o644))
		require.NoError(t, os.WriteFile(stubPath, []byte("nameserver 127.0.0.53\n"), 0o644))

		got, err := discoverFromFirst([]string{systemdPath, stubPath}, "127.0.0.1:15353")
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.2:53"}, got, "the stub routes claimed TLDs back to holepunch")
	})

	t.Run("falls back when the systemd file is absent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		stubPath := filepath.Join(dir, "resolv.conf")
		require.NoError(t, os.WriteFile(stubPath, []byte("nameserver 192.168.1.1\n"), 0o644))

		got, err := discoverFromFirst([]string{filepath.Join(dir, "absent.conf"), stubPath}, "127.0.0.1:15353")
		require.NoError(t, err)
		assert.Equal(t, []string{"192.168.1.1:53"}, got)
	})
}
