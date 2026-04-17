package daemon_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

// fakeProxyFactory implements engine.ProxyFactory with no-op proxies.
type (
	fakeProxyFactory struct{}
	fakeProxy        struct{}
)

func (fakeProxy) Stop()              {}
func (fakeProxy) ListenAddr() string { return "127.0.0.2:443" }

func (fakeProxyFactory) NewProxy(_ context.Context, _ engine.ProxyConfig) (engine.TCPProxy, error) {
	return fakeProxy{}, nil
}

// fakeSession implements session.Session for testing without real processes.
type fakeSession struct {
	done chan struct{}
	pid  int
}

func (f *fakeSession) PID() int    { return f.pid }
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

// fakeDNSManager implements engine.DNSManager without touching /etc/hosts.
type fakeDNSManager struct {
	entries []dns.Entry
}

func (d *fakeDNSManager) Add(_ context.Context, entries []dns.Entry) error {
	d.entries = append(d.entries, entries...)
	return nil
}

func (d *fakeDNSManager) Remove(_ context.Context) error {
	d.entries = nil
	return nil
}

func (d *fakeDNSManager) RemoveEntries(_ context.Context, entries []dns.Entry) error {
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

func startTestServer(t *testing.T) (string, *engine.Engine) {
	t.Helper()
	// Use /tmp directly to avoid macOS 104-byte unix socket path limit.
	socketPath := fmt.Sprintf("/tmp/awsc-test-%d.sock", time.Now().UnixNano())
	t.Cleanup(func() { os.Remove(socketPath) })

	eng := engine.New(testCfg(), &fakeDNSManager{}, &fakeSessionManager{}, ip.New(), fakeProxyFactory{})
	srv, err := daemon.NewServer(socketPath, eng)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		eng.Stop(context.Background(), nil)
		cancel()
	})

	go srv.Serve(ctx)

	// Wait for socket to be ready.
	require.Eventually(t, func() bool {
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)

	return socketPath, eng
}

func TestServerClientRoundTrip(t *testing.T) {
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
}

func TestClientIsRunning(t *testing.T) {
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

func TestProtocolReadWrite(t *testing.T) {
	t.Parallel()

	// Test the protocol by sending/receiving over a real unix socket pair.
	socketPath := fmt.Sprintf("/tmp/awsc-proto-%d.sock", time.Now().UnixNano())
	t.Cleanup(func() { os.Remove(socketPath) })

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

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

func TestPIDFile(t *testing.T) {
	t.Parallel()

	t.Run("write and read", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.pid")

		err := daemon.WritePIDFile(path)
		require.NoError(t, err)

		pid, err := daemon.ReadPIDFile(path)
		require.NoError(t, err)
		assert.Equal(t, os.Getpid(), pid)
	})

	t.Run("detects running daemon", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.pid")

		// Write current PID (which is running).
		err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
		require.NoError(t, err)

		err = daemon.WritePIDFile(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already running")
	})

	t.Run("overwrites stale PID", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.pid")

		// Write a PID that definitely doesn't exist.
		err := os.WriteFile(path, []byte("999999999"), 0o644)
		require.NoError(t, err)

		err = daemon.WritePIDFile(path)
		require.NoError(t, err)
	})

	t.Run("read nonexistent", func(t *testing.T) {
		t.Parallel()
		_, err := daemon.ReadPIDFile("/nonexistent/pid")
		require.Error(t, err)
	})

	t.Run("cleanup", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		sockPath := filepath.Join(dir, "daemon.sock")
		pidPath := filepath.Join(dir, "daemon.pid")

		require.NoError(t, os.WriteFile(sockPath, []byte{}, 0o644))
		require.NoError(t, os.WriteFile(pidPath, []byte{}, 0o644))

		daemon.Cleanup(sockPath, pidPath)

		_, err := os.Stat(sockPath)
		assert.True(t, os.IsNotExist(err))
		_, err = os.Stat(pidPath)
		assert.True(t, os.IsNotExist(err))
	})
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
