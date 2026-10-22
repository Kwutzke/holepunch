package daemon_test

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_IsRunning(t *testing.T) {
	t.Parallel()

	t.Run("returns true when daemon is running", func(t *testing.T) {
		t.Parallel()
		socketPath, _ := startTestServer(t)
		client := daemon.NewClient(socketPath)
		assert.True(t, client.IsRunning())
	})

	t.Run("returns false when no daemon", func(t *testing.T) {
		t.Parallel()
		client := daemon.NewClient("/tmp/nonexistent-test-socket.sock")
		assert.False(t, client.IsRunning())
	})
}

func TestClient_SendCommand(t *testing.T) {
	t.Parallel()

	// Test the protocol by sending/receiving over a real unix socket pair.
	socketPath := fmt.Sprintf("/tmp/awsc-proto-%d.sock", time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// Echo back whatever we receive as a Response.
		var req daemon.Request
		if err := readTestMessage(conn, &req); err != nil {
			return
		}
		_ = writeTestMessage(conn, daemon.Response{OK: true, Statuses: []daemon.StatusEntry{{Profile: req.Targets[0]}}})
	}()

	client := daemon.NewClient(socketPath)
	resp, err := client.SendCommand(daemon.Request{Command: daemon.CmdStatus, Targets: []string{"test-profile"}})
	require.NoError(t, err)
	assert.True(t, resp.OK)
	require.Len(t, resp.Statuses, 1)
	assert.Equal(t, "test-profile", resp.Statuses[0].Profile)

	<-done
}
