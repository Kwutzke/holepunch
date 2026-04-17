package daemon

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/Kwutzke/holepunch/internal/engine"
)

// Command constants for the daemon protocol.
const (
	CmdUp           = "up"
	CmdDown         = "down"
	CmdStatus       = "status"
	CmdLogs         = "logs"
	CmdSubscribe    = "subscribe"
	CmdTrayRegister = "tray_register"
)

// Envelope kind constants.
const (
	EnvelopeKindLog          = "log"
	EnvelopeKindState        = "state"
	EnvelopeKindCredsExpired = "creds_expired"
	EnvelopeKindProfileDone  = "profile_done"
)

// Request is a message from the CLI to the daemon.
//
// Targets is a list of selectors, each either "profile" (all services in
// the profile) or "profile/service" (a single service). Empty means
// "everything" — up starts every profile, down stops everything running.
type Request struct {
	Command string   `json:"command"`
	Targets []string `json:"targets,omitempty"`
	Follow  bool     `json:"follow,omitempty"` // for logs command
}

// Response is a message from the daemon to the CLI.
type Response struct {
	OK       bool             `json:"ok"`
	Error    string           `json:"error,omitempty"`
	Statuses []StatusEntry    `json:"statuses,omitempty"`
	Event    *engine.LogEntry `json:"event,omitempty"`    // legacy logs stream
	Envelope *EventEnvelope   `json:"envelope,omitempty"` // subscribe/tray_register stream
}

// StatusEntry represents a single service status in a response.
type StatusEntry struct {
	Profile     string `json:"profile"`
	ServiceName string `json:"service_name"`
	DNSName     string `json:"dns_name"`
	LocalPort   int    `json:"local_port,omitempty"`
	LocalAddr   string `json:"local_addr,omitempty"`
	State       string `json:"state"`
	ConnectedAt string `json:"connected_at,omitempty"`
	Error       string `json:"error,omitempty"`
}

// EventEnvelope is a tagged union carrying typed engine events over the wire.
// Exactly one of the pointer fields is populated; Kind selects which one.
type EventEnvelope struct {
	Kind        string                  `json:"kind"`
	Log         *LogEntryDTO            `json:"log,omitempty"`
	State       *ServiceStateChangedDTO `json:"state,omitempty"`
	Creds       *CredentialsExpiredDTO  `json:"creds,omitempty"`
	ProfileDone *ProfileDoneDTO         `json:"profile_done,omitempty"`
}

// LogEntryDTO mirrors engine.LogEntry with explicit JSON tags for wire format.
type LogEntryDTO struct {
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Profile string    `json:"profile,omitempty"`
	Service string    `json:"service,omitempty"`
	Time    time.Time `json:"time"`
}

// ServiceStateChangedDTO mirrors engine.ServiceStateChanged with string states
// and a flattened error field (state.ServiceState is an int, error is an
// interface — neither marshals cleanly).
type ServiceStateChangedDTO struct {
	Profile     string    `json:"profile"`
	ServiceName string    `json:"service_name"`
	DNSName     string    `json:"dns_name,omitempty"`
	From        string    `json:"from"`
	To          string    `json:"to"`
	Error       string    `json:"error,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
}

// CredentialsExpiredDTO mirrors engine.CredentialsExpired on the wire.
type CredentialsExpiredDTO struct {
	Profile     string    `json:"profile"`
	AWSProfile  string    `json:"aws_profile"`
	ServiceName string    `json:"service_name,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
}

// ProfileDoneDTO mirrors engine.ProfileDone on the wire.
type ProfileDoneDTO struct {
	Profile   string    `json:"profile"`
	Timestamp time.Time `json:"timestamp"`
}

// eventToEnvelope converts an engine.Event into a wire envelope.
// Returns (nil, false) for event kinds we don't stream.
func eventToEnvelope(evt engine.Event) (*EventEnvelope, bool) {
	switch e := evt.(type) {
	case engine.LogEntry:
		return &EventEnvelope{
			Kind: EnvelopeKindLog,
			Log: &LogEntryDTO{
				Level:   e.Level,
				Message: e.Message,
				Profile: e.Profile,
				Service: e.Service,
				Time:    e.Time,
			},
		}, true
	case engine.ServiceStateChanged:
		dto := &ServiceStateChangedDTO{
			Profile:     e.Profile,
			ServiceName: e.ServiceName,
			DNSName:     e.DNSName,
			From:        e.From.String(),
			To:          e.To.String(),
			Timestamp:   e.Timestamp,
		}
		if e.Error != nil {
			dto.Error = e.Error.Error()
		}
		return &EventEnvelope{Kind: EnvelopeKindState, State: dto}, true
	case engine.CredentialsExpired:
		return &EventEnvelope{
			Kind: EnvelopeKindCredsExpired,
			Creds: &CredentialsExpiredDTO{
				Profile:     e.Profile,
				AWSProfile:  e.AWSProfile,
				ServiceName: e.ServiceName,
				Detail:      e.Detail,
				Timestamp:   e.Timestamp,
			},
		}, true
	case engine.ProfileDone:
		return &EventEnvelope{
			Kind: EnvelopeKindProfileDone,
			ProfileDone: &ProfileDoneDTO{
				Profile:   e.Profile,
				Timestamp: e.Timestamp,
			},
		}, true
	}
	return nil, false
}

// writeMessage writes a length-prefixed JSON message to a connection.
func writeMessage(conn net.Conn, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}

	// Write 4-byte big-endian length prefix.
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	if _, err := conn.Write(lenBuf); err != nil {
		return fmt.Errorf("writing length: %w", err)
	}
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("writing data: %w", err)
	}
	return nil
}

// readMessage reads a length-prefixed JSON message from a connection.
func readMessage(conn net.Conn, msg any) error {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return fmt.Errorf("reading length: %w", err)
	}

	length := binary.BigEndian.Uint32(lenBuf)
	if length > 10*1024*1024 { // 10MB sanity limit
		return fmt.Errorf("message too large: %d bytes", length)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return fmt.Errorf("reading data: %w", err)
	}

	if err := json.Unmarshal(data, msg); err != nil {
		return fmt.Errorf("unmarshaling message: %w", err)
	}
	return nil
}
