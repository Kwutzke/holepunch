package reconnect_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/Kwutzke/holepunch/internal/reconnect"
)

func TestBackoffNextDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		attempt  int
		expected time.Duration
	}{
		{"attempt 0", 0, 1 * time.Second},
		{"attempt 1", 1, 2 * time.Second},
		{"attempt 2", 2, 4 * time.Second},
		{"attempt 3", 3, 8 * time.Second},
		{"attempt 4", 4, 16 * time.Second},
		{"attempt 5 hits cap", 5, 30 * time.Second},
		{"attempt 6 stays at cap", 6, 30 * time.Second},
		{"attempt 10 stays at cap", 10, 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := reconnect.NewBackoff()
			// Advance to the desired attempt.
			for range tt.attempt {
				b.NextDelay()
			}
			got := b.NextDelay()
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestBackoffSequence(t *testing.T) {
	t.Parallel()

	b := reconnect.NewBackoff()
	expected := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}

	for i, want := range expected {
		got := b.NextDelay()
		require.Equal(t, want, got, "attempt %d", i)
		assert.Equal(t, i+1, b.Attempt(), "attempt counter after call %d", i)
	}
}

func TestBackoffReset(t *testing.T) {
	t.Parallel()

	b := reconnect.NewBackoff()
	b.NextDelay() // 1s
	b.NextDelay() // 2s
	b.NextDelay() // 4s
	assert.Equal(t, 3, b.Attempt())

	b.Reset()
	assert.Equal(t, 0, b.Attempt())
	assert.Equal(t, 1*time.Second, b.NextDelay(), "should restart from initial delay after reset")
}

func TestBackoffShouldReset(t *testing.T) {
	t.Parallel()

	t.Run("false when never connected", func(t *testing.T) {
		t.Parallel()
		b := reconnect.NewBackoff()
		assert.False(t, b.ShouldReset())
	})

	t.Run("false immediately after MarkConnected", func(t *testing.T) {
		t.Parallel()
		b := reconnect.NewBackoff()
		b.MarkConnected()
		assert.False(t, b.ShouldReset())
	})

	t.Run("true after StableAfter duration", func(t *testing.T) {
		t.Parallel()
		b := &reconnect.Backoff{
			InitialDelay: 1 * time.Second,
			MaxDelay:     30 * time.Second,
			Multiplier:   2.0,
			StableAfter:  1 * time.Millisecond, // very short for testing
		}
		b.MarkConnected()
		time.Sleep(2 * time.Millisecond)
		assert.True(t, b.ShouldReset())
	})
}

func TestBackoffCustomConfig(t *testing.T) {
	t.Parallel()

	b := &reconnect.Backoff{
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     500 * time.Millisecond,
		Multiplier:   3.0,
		StableAfter:  10 * time.Second,
	}

	assert.Equal(t, 100*time.Millisecond, b.NextDelay()) // 100ms * 3^0
	assert.Equal(t, 300*time.Millisecond, b.NextDelay()) // 100ms * 3^1
	assert.Equal(t, 500*time.Millisecond, b.NextDelay()) // 100ms * 3^2 = 900ms, capped to 500ms
}
