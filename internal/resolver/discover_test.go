package resolver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_discoverFromConfig(t *testing.T) {
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

				got, err := discoverFromConfig(path, tt.selfAddr)
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			})
		}
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()

		_, err := discoverFromConfig(filepath.Join(t.TempDir(), "nope.conf"), "127.0.0.1:15353")
		assert.Error(t, err)
	})
}
