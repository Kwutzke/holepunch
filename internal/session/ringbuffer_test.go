package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRingBuffer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capacity int
		writes   []string
		want     string
	}{
		{"empty", 16, nil, ""},
		{"single write under capacity", 16, []string{"hello"}, "hello"},
		{"multiple writes under capacity", 16, []string{"hello ", "world"}, "hello world"},
		{"exactly capacity", 5, []string{"abcde"}, "abcde"},
		{"overflow single write", 5, []string{"abcdefgh"}, "defgh"},
		{"wrap around", 6, []string{"abcd", "efgh"}, "cdefgh"},
		{"many wraps", 4, []string{"aa", "bb", "cc", "dd", "ee"}, "ddee"},
		{"giant single write", 4, []string{strings.Repeat("x", 100) + "END!"}, "END!"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rb := newRingBuffer(tt.capacity)
			for _, w := range tt.writes {
				n, err := rb.Write([]byte(w))
				assert.NoError(t, err)
				assert.Equal(t, len(w), n)
			}
			assert.Equal(t, tt.want, string(rb.Bytes()))
		})
	}
}
