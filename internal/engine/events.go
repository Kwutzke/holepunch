package engine

import (
	"time"

	"github.com/Kwutzke/holepunch/internal/state"
)

// Event is the interface for all engine events.
// Consumers (CLI, TUI) read from the Events channel and type-switch.
type Event interface {
	eventMarker()
}

// ServiceStateChanged is emitted whenever a service transitions state.
type ServiceStateChanged struct {
	Profile     string
	ServiceName string
	DNSName     string
	From        state.ServiceState
	To          state.ServiceState
	Error       error // non-nil when transitioning to Failed
	Timestamp   time.Time
}

func (ServiceStateChanged) eventMarker() {}

// LogEntry is emitted for informational messages from the engine.
type LogEntry struct {
	Level   string // "info", "warn", "error"
	Message string
	Profile string
	Service string
	Time    time.Time
}

func (LogEntry) eventMarker() {}

// ProfileDone is emitted when all services in a profile have stopped.
type ProfileDone struct {
	Profile   string
	Timestamp time.Time
}

func (ProfileDone) eventMarker() {}
