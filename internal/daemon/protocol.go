package daemon

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"

	"github.com/Kwutzke/holepunch/internal/engine"
)

// Command constants for the daemon protocol.
const (
	CmdUp     = "up"
	CmdDown   = "down"
	CmdStatus = "status"
	CmdLogs   = "logs"
)

// Request is a message from the CLI to the daemon.
type Request struct {
	Command  string   `json:"command"`
	Profiles []string `json:"profiles,omitempty"`
	Follow   bool     `json:"follow,omitempty"` // for logs command
}

// Response is a message from the daemon to the CLI.
type Response struct {
	OK       bool             `json:"ok"`
	Error    string           `json:"error,omitempty"`
	Statuses []StatusEntry    `json:"statuses,omitempty"`
	Event    *engine.LogEntry `json:"event,omitempty"` // for streaming logs
}

// StatusEntry represents a single service status in a response.
type StatusEntry struct {
	Profile     string `json:"profile"`
	ServiceName string `json:"service_name"`
	DNSName     string `json:"dns_name"`
	LocalAddr   string `json:"local_addr"`
	State       string `json:"state"`
	ConnectedAt string `json:"connected_at,omitempty"`
	Error       string `json:"error,omitempty"`
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
