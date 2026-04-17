package daemon

import (
	"fmt"
	"net"
)

// Client communicates with the daemon over a unix socket.
type Client struct {
	socketPath string
}

// NewClient creates a client that connects to the daemon at the given socket path.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

// SendCommand sends a request to the daemon and returns the response.
func (c *Client) SendCommand(req Request) (Response, error) {
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return Response{}, fmt.Errorf("connecting to daemon: %w", err)
	}
	defer conn.Close()

	if err := writeMessage(conn, req); err != nil {
		return Response{}, fmt.Errorf("sending request: %w", err)
	}

	var resp Response
	if err := readMessage(conn, &resp); err != nil {
		return Response{}, fmt.Errorf("reading response: %w", err)
	}

	return resp, nil
}

// StreamLogs connects to the daemon and streams log responses.
// The callback is called for each log response. Blocks until the connection
// is closed or the callback returns false.
func (c *Client) StreamLogs(req Request, callback func(Response) bool) error {
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	defer conn.Close()

	if err := writeMessage(conn, req); err != nil {
		return fmt.Errorf("sending request: %w", err)
	}

	// Read initial OK response.
	var initial Response
	if err := readMessage(conn, &initial); err != nil {
		return fmt.Errorf("reading initial response: %w", err)
	}
	if !initial.OK {
		return fmt.Errorf("daemon error: %s", initial.Error)
	}

	for {
		var resp Response
		if err := readMessage(conn, &resp); err != nil {
			return nil // connection closed
		}
		if !callback(resp) {
			return nil
		}
	}
}

// StreamEvents sends the given streaming request (CmdSubscribe or
// CmdTrayRegister), reads the initial OK response, then invokes callback
// for every envelope received. Returns nil on graceful connection close
// or when callback returns false.
func (c *Client) StreamEvents(req Request, callback func(*EventEnvelope) bool) error {
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := writeMessage(conn, req); err != nil {
		return fmt.Errorf("sending request: %w", err)
	}

	var initial Response
	if err := readMessage(conn, &initial); err != nil {
		return fmt.Errorf("reading initial response: %w", err)
	}
	if !initial.OK {
		return fmt.Errorf("daemon error: %s", initial.Error)
	}

	for {
		var resp Response
		if err := readMessage(conn, &resp); err != nil {
			return nil
		}
		if resp.Envelope == nil {
			continue
		}
		if !callback(resp.Envelope) {
			return nil
		}
	}
}

// IsRunning checks if the daemon is reachable.
func (c *Client) IsRunning() bool {
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
