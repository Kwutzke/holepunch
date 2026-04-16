package state_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/Kwutzke/holepunch/internal/state"
)

func TestTransition(t *testing.T) {
	t.Parallel()

	validTransitions := []struct {
		name string
		from state.ServiceState
		to   state.ServiceState
	}{
		{"Disconnected to Starting", state.Disconnected, state.Starting},
		{"Starting to Connected", state.Starting, state.Connected},
		{"Starting to Failed", state.Starting, state.Failed},
		{"Connected to Reconnecting", state.Connected, state.Reconnecting},
		{"Connected to Stopping", state.Connected, state.Stopping},
		{"Reconnecting to Starting", state.Reconnecting, state.Starting},
		{"Reconnecting to Failed", state.Reconnecting, state.Failed},
		{"Reconnecting to Stopping", state.Reconnecting, state.Stopping},
		{"Failed to Starting", state.Failed, state.Starting},
		{"Failed to Stopping", state.Failed, state.Stopping},
		{"Stopping to Disconnected", state.Stopping, state.Disconnected},
	}

	for _, tt := range validTransitions {
		t.Run("valid/"+tt.name, func(t *testing.T) {
			t.Parallel()
			err := state.Transition(tt.from, tt.to)
			require.NoError(t, err)
		})
	}

	invalidTransitions := []struct {
		name string
		from state.ServiceState
		to   state.ServiceState
	}{
		{"Disconnected to Connected", state.Disconnected, state.Connected},
		{"Disconnected to Reconnecting", state.Disconnected, state.Reconnecting},
		{"Disconnected to Failed", state.Disconnected, state.Failed},
		{"Disconnected to Stopping", state.Disconnected, state.Stopping},
		{"Disconnected to Disconnected", state.Disconnected, state.Disconnected},
		{"Starting to Starting", state.Starting, state.Starting},
		{"Starting to Reconnecting", state.Starting, state.Reconnecting},
		{"Starting to Stopping", state.Starting, state.Stopping},
		{"Starting to Disconnected", state.Starting, state.Disconnected},
		{"Connected to Connected", state.Connected, state.Connected},
		{"Connected to Starting", state.Connected, state.Starting},
		{"Connected to Failed", state.Connected, state.Failed},
		{"Connected to Disconnected", state.Connected, state.Disconnected},
		{"Reconnecting to Connected", state.Reconnecting, state.Connected},
		{"Reconnecting to Reconnecting", state.Reconnecting, state.Reconnecting},
		{"Reconnecting to Disconnected", state.Reconnecting, state.Disconnected},
		{"Failed to Connected", state.Failed, state.Connected},
		{"Failed to Reconnecting", state.Failed, state.Reconnecting},
		{"Failed to Failed", state.Failed, state.Failed},
		{"Failed to Disconnected", state.Failed, state.Disconnected},
		{"Stopping to Starting", state.Stopping, state.Starting},
		{"Stopping to Connected", state.Stopping, state.Connected},
		{"Stopping to Reconnecting", state.Stopping, state.Reconnecting},
		{"Stopping to Failed", state.Stopping, state.Failed},
		{"Stopping to Stopping", state.Stopping, state.Stopping},
	}

	for _, tt := range invalidTransitions {
		t.Run("invalid/"+tt.name, func(t *testing.T) {
			t.Parallel()
			err := state.Transition(tt.from, tt.to)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid transition")
		})
	}
}

func TestServiceStateString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state    state.ServiceState
		expected string
	}{
		{state.Disconnected, "Disconnected"},
		{state.Starting, "Starting"},
		{state.Connected, "Connected"},
		{state.Reconnecting, "Reconnecting"},
		{state.Failed, "Failed"},
		{state.Stopping, "Stopping"},
		{state.ServiceState(99), "Unknown(99)"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, tt.state.String())
		})
	}
}
