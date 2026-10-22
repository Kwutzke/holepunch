package daemon_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Kwutzke/holepunch/internal/config"
	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/Kwutzke/holepunch/internal/dns"
	"github.com/Kwutzke/holepunch/internal/engine"
	"github.com/Kwutzke/holepunch/internal/ip"
	"github.com/Kwutzke/holepunch/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (fakeProxy) Stop() {}

func (fakeProxy) ListenAddr() string { return "127.0.0.2:443" }
func (fakeProxyFactory) NewProxy(_ context.Context, _ engine.ProxyConfig) (engine.TCPProxy, error) {
	return fakeProxy{}, nil
}

// fakeSession implements session.Session for testing without real processes.
type fakeSession struct {
	done chan struct{}
	pid  int
}

func (f *fakeSession) PID() int { return f.pid }

func (f *fakeSession) Wait() error { <-f.done; return nil }
func (f *fakeSession) Stop(_ context.Context) error {
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return nil
}

// fakeSessionManager creates fake sessions that stay alive until stopped.
type fakeSessionManager struct {
	sessions []*fakeSession
}

func (m *fakeSessionManager) Start(_ context.Context, _ session.StartParams) (session.Session, error) {
	s := &fakeSession{done: make(chan struct{}), pid: 12345 + len(m.sessions)}
	m.sessions = append(m.sessions, s)
	return s, nil
}

// fakeBastionResolver returns profile.Target for every Resolve call.
type fakeBastionResolver struct{}

func (fakeBastionResolver) Resolve(_ context.Context, p config.Profile) (string, error) {
	return p.Target, nil
}

func (fakeBastionResolver) Invalidate(_ config.Profile) {}

// fakeDNSManager implements engine.DNSManager without touching /etc/hosts.
type fakeDNSManager struct {
	mu      sync.Mutex
	entries []dns.Entry
}

func (d *fakeDNSManager) Add(_ context.Context, entries []dns.Entry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries = append(d.entries, entries...)
	return nil
}

func (d *fakeDNSManager) Remove(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries = nil
	return nil
}

func (d *fakeDNSManager) RemoveEntries(_ context.Context, entries []dns.Entry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	remove := make(map[string]bool)
	for _, e := range entries {
		remove[e.Hostname] = true
	}
	var filtered []dns.Entry
	for _, e := range d.entries {
		if !remove[e.Hostname] {
			filtered = append(filtered, e)
		}
	}
	d.entries = filtered
	return nil
}

// fakeProxyFactory implements engine.ProxyFactory with no-op proxies.
type (
	fakeProxyFactory struct{}
	fakeProxy        struct{}
)

func TestServer_Serve(t *testing.T) {
	t.Parallel()

	t.Run("client round trip", func(t *testing.T) {
		t.Parallel()

		t.Run("status on empty engine", func(t *testing.T) {
			t.Parallel()
			socketPath, _ := startTestServer(t)
			client := daemon.NewClient(socketPath)

			resp, err := client.SendCommand(daemon.Request{Command: daemon.CmdStatus})
			require.NoError(t, err)
			assert.True(t, resp.OK)
			// Status now includes configured-but-not-running services so UIs
			// can render the full inventory. testCfg has one service.
			require.Len(t, resp.Statuses, 1)
			assert.Equal(t, "Disconnected", resp.Statuses[0].State)
		})

		t.Run("up then status", func(t *testing.T) {
			t.Parallel()
			socketPath, _ := startTestServer(t)
			client := daemon.NewClient(socketPath)

			resp, err := client.SendCommand(daemon.Request{Command: daemon.CmdUp, Targets: []string{"dev"}})
			require.NoError(t, err)
			assert.True(t, resp.OK, "up response error: %s", resp.Error)

			// Give the engine a moment to connect.
			time.Sleep(100 * time.Millisecond)

			resp, err = client.SendCommand(daemon.Request{Command: daemon.CmdStatus})
			require.NoError(t, err)
			assert.True(t, resp.OK)
			require.Len(t, resp.Statuses, 1)
			assert.Equal(t, "dev", resp.Statuses[0].Profile)
			assert.Equal(t, "opensearch", resp.Statuses[0].ServiceName)
			assert.Equal(t, "opensearch.dev", resp.Statuses[0].DNSName)
		})

		t.Run("up then down", func(t *testing.T) {
			t.Parallel()
			socketPath, _ := startTestServer(t)
			client := daemon.NewClient(socketPath)

			resp, err := client.SendCommand(daemon.Request{Command: daemon.CmdUp, Targets: []string{"dev"}})
			require.NoError(t, err)
			assert.True(t, resp.OK)

			time.Sleep(100 * time.Millisecond)

			resp, err = client.SendCommand(daemon.Request{Command: daemon.CmdDown, Targets: []string{"dev"}})
			require.NoError(t, err)
			assert.True(t, resp.OK)

			resp, err = client.SendCommand(daemon.Request{Command: daemon.CmdStatus})
			require.NoError(t, err)
			assert.True(t, resp.OK)
			// After Down, services drop back to Disconnected but remain in
			// the response (configured-but-not-running).
			require.Len(t, resp.Statuses, 1)
			assert.Equal(t, "Disconnected", resp.Statuses[0].State)
		})

		t.Run("unknown command", func(t *testing.T) {
			t.Parallel()
			socketPath, _ := startTestServer(t)
			client := daemon.NewClient(socketPath)

			resp, err := client.SendCommand(daemon.Request{Command: "bogus"})
			require.NoError(t, err)
			assert.False(t, resp.OK)
			assert.Contains(t, resp.Error, "unknown command")
		})

		t.Run("unknown profile", func(t *testing.T) {
			t.Parallel()
			socketPath, _ := startTestServer(t)
			client := daemon.NewClient(socketPath)

			resp, err := client.SendCommand(daemon.Request{Command: daemon.CmdUp, Targets: []string{"nonexistent"}})
			require.NoError(t, err)
			assert.False(t, resp.OK)
			assert.Contains(t, resp.Error, "unknown profile")
		})
	})

	t.Run("tray register counts the tray client", func(t *testing.T) {
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
	})

	t.Run("subscribe does not count as a tray client", func(t *testing.T) {
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
	})

	t.Run("tray register streams envelopes", func(t *testing.T) {
		t.Parallel()

		socketPath, _ := startTestServerWithSrv(t)

		conn, err := net.Dial("unix", socketPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		require.NoError(t, daemon.ExportedWriteMessage(conn, daemon.Request{Command: daemon.CmdTrayRegister}))

		// The server subscribes before it acks, so events published after the ack reach this stream.
		var initial daemon.Response
		require.NoError(t, daemon.ExportedReadMessage(conn, &initial))
		require.True(t, initial.OK)

		resp, err := daemon.NewClient(socketPath).SendCommand(daemon.Request{Command: daemon.CmdUp, Targets: []string{"dev"}})
		require.NoError(t, err)
		require.True(t, resp.OK)

		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		var streamed daemon.Response
		require.NoError(t, daemon.ExportedReadMessage(conn, &streamed))
		require.NotNil(t, streamed.Envelope)
		assert.NotEmpty(t, streamed.Envelope.Kind)
	})
}

// TestServerSubscribe_ReceivesAllEvents guards against regressing the
// dual-drain bug: if anything else (e.g. cmd/daemon.go's log forwarder)
// also ranges over eng.Events(), events would be split between consumers
// and this subscriber would see roughly half of them.
func TestServer_Subscribe(t *testing.T) {
	t.Parallel()

	socketPath, srv := startTestServerWithSrv(t)

	events, cancel := srv.Subscribe()
	t.Cleanup(cancel)

	client := daemon.NewClient(socketPath)
	resp, err := client.SendCommand(daemon.Request{Command: daemon.CmdUp, Targets: []string{"dev"}})
	require.NoError(t, err)
	require.True(t, resp.OK, "up failed: %s", resp.Error)

	// Expect at least one ServiceStateChanged (Disconnected -> Starting)
	// within a short window. Prior to the fix, this would flake because
	// the log forwarder could win the channel receive.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt, ok := <-events:
			require.True(t, ok, "subscriber channel closed unexpectedly")
			if _, isState := evt.(engine.ServiceStateChanged); isState {
				return
			}
		case <-deadline:
			t.Fatal("no ServiceStateChanged received within 2s")
		}
	}
}

func startTestServer(t *testing.T) (string, *engine.Engine) {
	t.Helper()
	// Use /tmp directly to avoid macOS 104-byte unix socket path limit.
	socketPath := fmt.Sprintf("/tmp/awsc-test-%d.sock", time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	eng := engine.New(testCfg(), &fakeDNSManager{}, &fakeSessionManager{}, fakeBastionResolver{}, ip.New(), fakeProxyFactory{})
	srv, err := daemon.NewServer(socketPath, eng)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		_ = eng.Stop(context.Background(), nil)
		cancel()
	})

	go func() { _ = srv.Serve(ctx) }()

	// Wait for socket to be ready.
	require.Eventually(t, func() bool {
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)

	return socketPath, eng
}

// Helper functions that mirror the internal protocol functions for testing.
// These are needed since writeMessage/readMessage are unexported.
func writeTestMessage(conn net.Conn, msg any) error {
	// Use the client/server round-trip instead — these are just for the raw protocol test.
	return daemon.ExportedWriteMessage(conn, msg)
}

func readTestMessage(conn net.Conn, msg any) error {
	return daemon.ExportedReadMessage(conn, msg)
}

// startTestServerWithSrv mirrors startTestServer but returns the Server too,
// for tests that need to observe server-internal state via export_test
// accessors (HasTrayClient).
func startTestServerWithSrv(t *testing.T) (string, *daemon.Server) {
	t.Helper()
	socketPath := fmt.Sprintf("/tmp/awsc-sub-%d.sock", time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	eng := engine.New(testCfg(), &fakeDNSManager{}, &fakeSessionManager{}, fakeBastionResolver{}, ip.New(), fakeProxyFactory{})
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

func testCfg() config.Config {
	return config.Config{
		Profiles: map[string]config.Profile{
			"dev": {
				AWSProfile: "dev-sso",
				AWSRegion:  "eu-central-1",
				Target:     "i-0abc123",
				Services: []config.Service{
					{
						Name:       "opensearch",
						DNSName:    "opensearch.dev",
						RemoteHost: "vpc-os.es.amazonaws.com",
						RemotePort: 443,
						LocalPort:  443,
					},
				},
			},
		},
	}
}
