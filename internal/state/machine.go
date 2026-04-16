package state

import "fmt"

// ServiceState represents the current state of a port-forwarding service.
type ServiceState int

const (
	Disconnected ServiceState = iota
	Starting
	Connected
	Reconnecting
	Failed
	Stopping
)

var stateNames = map[ServiceState]string{
	Disconnected: "Disconnected",
	Starting:     "Starting",
	Connected:    "Connected",
	Reconnecting: "Reconnecting",
	Failed:       "Failed",
	Stopping:     "Stopping",
}

func (s ServiceState) String() string {
	if name, ok := stateNames[s]; ok {
		return name
	}
	return fmt.Sprintf("Unknown(%d)", int(s))
}

var allowedTransitions = map[ServiceState]map[ServiceState]bool{
	Disconnected: {Starting: true},
	Starting:     {Connected: true, Failed: true},
	Connected:    {Reconnecting: true, Stopping: true},
	Reconnecting: {Starting: true, Failed: true, Stopping: true},
	Failed:       {Starting: true, Stopping: true},
	Stopping:     {Disconnected: true},
}

// Transition validates that moving from one state to another is allowed.
// Returns an error if the transition is invalid.
func Transition(from, to ServiceState) error {
	targets, ok := allowedTransitions[from]
	if !ok {
		return fmt.Errorf("unknown source state: %v", from)
	}
	if !targets[to] {
		return fmt.Errorf("invalid transition: %v -> %v", from, to)
	}
	return nil
}
