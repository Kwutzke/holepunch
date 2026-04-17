package daemon_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/Kwutzke/holepunch/internal/engine"
	"github.com/Kwutzke/holepunch/internal/ip"
)

// startTestServerWithSrv mirrors startTestServer but returns the Server too,
// for tests that need to observe server-internal state via export_test
// accessors (HasTrayClient).
func startTestServerWithSrv(t *testing.T) (string, *daemon.Server) {
	t.Helper()
	socketPath := fmt.Sprintf("/tmp/awsc-sub-%d.sock", time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	eng := engine.New(testCfg(), &fakeDNSManager{}, &fakeSessionManager{}, ip.New(), fakeProxyFactory{})
	srv, err := daemon.NewServer(socketPath, eng)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		_ = eng.Stop(context.Background(), nil)
		cancel()
	})

	go func() { _ = srv.Serve(ctx) }()

	require.Eventually(t, func() bool {
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)

	return socketPath, srv
}

func TestTrayRegister_IncrementsCounter(t *testing.T) {
	t.Parallel()

	socketPath, srv := startTestServerWithSrv(t)
	assert.False(t, srv.HasTrayClient(), "no tray client before register")

	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, daemon.ExportedWriteMessage(conn, daemon.Request{Command: daemon.CmdTrayRegister}))

	// Drain initial OK so the handler has entered its stream loop.
	var initial daemon.Response
	require.NoError(t, daemon.ExportedReadMessage(conn, &initial))
	require.True(t, initial.OK)

	require.Eventually(t, srv.HasTrayClient, time.Second, 10*time.Millisecond,
		"counter should increment while tray is registered")

	// Close the connection — counter should drop back to zero.
	_ = conn.Close()
	require.Eventually(t, func() bool { return !srv.HasTrayClient() },
		time.Second, 10*time.Millisecond,
		"counter should decrement after tray disconnects")
}

func TestSubscribe_DoesNotIncrementTrayCounter(t *testing.T) {
	t.Parallel()

	socketPath, srv := startTestServerWithSrv(t)

	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, daemon.ExportedWriteMessage(conn, daemon.Request{Command: daemon.CmdSubscribe}))

	var initial daemon.Response
	require.NoError(t, daemon.ExportedReadMessage(conn, &initial))
	require.True(t, initial.OK)

	// Counter is for tray clients only, not plain subscribers.
	time.Sleep(50 * time.Millisecond)
	assert.False(t, srv.HasTrayClient())
}

func TestTrayRegister_StreamsEnvelopes(t *testing.T) {
	t.Parallel()

	socketPath, _ := startTestServerWithSrv(t)

	received := make(chan *daemon.EventEnvelope, 4)
	streamErr := make(chan error, 1)

	go func() {
		client := daemon.NewClient(socketPath)
		streamErr <- client.StreamEvents(
			daemon.Request{Command: daemon.CmdTrayRegister},
			func(env *daemon.EventEnvelope) bool {
				select {
				case received <- env:
				default:
				}
				// Stop after the first envelope so the goroutine exits.
				return false
			},
		)
	}()

	// Drive an engine event by calling Up — the fake session transitions
	// produce state envelopes on the bus.
	client := daemon.NewClient(socketPath)
	resp, err := client.SendCommand(daemon.Request{Command: daemon.CmdUp, Targets: []string{"dev"}})
	require.NoError(t, err)
	require.True(t, resp.OK)

	select {
	case env := <-received:
		require.NotNil(t, env)
		assert.NotEmpty(t, env.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("no envelope received within 2s")
	}

	// Drain the stream-close so the test doesn't leak a pending error.
	select {
	case <-streamErr:
	case <-time.After(time.Second):
	}
}
