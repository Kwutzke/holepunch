package reconnect

import (
	"math"
	"time"
)

// Backoff calculates exponential backoff delays with a cap.
type Backoff struct {
	InitialDelay time.Duration
	MaxDelay     time.Duration
	Multiplier   float64
	StableAfter  time.Duration // reset attempt count after this duration connected

	attempt     int
	connectedAt time.Time
}

// NewBackoff creates a Backoff with sensible defaults.
func NewBackoff() *Backoff {
	return &Backoff{
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
		Multiplier:   2.0,
		StableAfter:  30 * time.Second,
	}
}

// NextDelay returns the delay before the next reconnection attempt
// and increments the internal attempt counter.
func (b *Backoff) NextDelay() time.Duration {
	delay := min(
		time.Duration(float64(b.InitialDelay)*math.Pow(b.Multiplier, float64(b.attempt))),
		b.MaxDelay,
	)
	b.attempt++
	return delay
}

// Attempt returns the current attempt number (0-based, before NextDelay increments it).
func (b *Backoff) Attempt() int {
	return b.attempt
}

// MarkConnected records the time a connection was established.
// Call this when transitioning to Connected state.
func (b *Backoff) MarkConnected() {
	b.connectedAt = time.Now()
}

// ShouldReset returns true if the connection has been stable long enough
// to reset the backoff counter.
func (b *Backoff) ShouldReset() bool {
	if b.connectedAt.IsZero() {
		return false
	}
	return time.Since(b.connectedAt) >= b.StableAfter
}

// Reset resets the attempt counter to zero.
func (b *Backoff) Reset() {
	b.attempt = 0
	b.connectedAt = time.Time{}
}
