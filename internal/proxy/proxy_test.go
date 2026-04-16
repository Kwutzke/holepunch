package proxy_test

import (
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/Kwutzke/holepunch/internal/proxy"
)

func TestProxyForwardsData(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Start a simple echo server as the "target".
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { echoListener.Close() })

	go func() {
		for {
			conn, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()

	// Start the proxy pointing at the echo server.
	p := proxy.New("127.0.0.1:0", echoListener.Addr().String())
	require.NoError(t, p.Start(ctx))
	t.Cleanup(p.Stop)

	// Connect through the proxy and verify data flows.
	conn, err := net.Dial("tcp", p.ListenAddr())
	require.NoError(t, err)
	defer conn.Close()

	msg := "hello through proxy"
	fmt.Fprintln(conn, msg)
	conn.(*net.TCPConn).CloseWrite()

	buf, err := io.ReadAll(conn)
	require.NoError(t, err)
	assert.Equal(t, msg+"\n", string(buf))
}

func TestProxyMultipleConnections(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { echoListener.Close() })

	go func() {
		for {
			conn, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()

	p := proxy.New("127.0.0.1:0", echoListener.Addr().String())
	require.NoError(t, p.Start(ctx))
	t.Cleanup(p.Stop)

	for i := range 5 {
		conn, err := net.Dial("tcp", p.ListenAddr())
		require.NoError(t, err)

		msg := fmt.Sprintf("msg-%d", i)
		fmt.Fprintln(conn, msg)
		conn.(*net.TCPConn).CloseWrite()

		buf, err := io.ReadAll(conn)
		require.NoError(t, err)
		assert.Equal(t, msg+"\n", string(buf))
		conn.Close()
	}
}

func TestProxyTargetDown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Proxy points to a port with nothing listening.
	p := proxy.New("127.0.0.1:0", "127.0.0.1:1")
	require.NoError(t, p.Start(ctx))
	t.Cleanup(p.Stop)

	conn, err := net.Dial("tcp", p.ListenAddr())
	require.NoError(t, err)
	defer conn.Close()

	// Connection should close quickly since target is unreachable.
	buf, _ := io.ReadAll(conn)
	assert.Empty(t, buf)
}

func TestProxyStop(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	p := proxy.New("127.0.0.1:0", "127.0.0.1:1")
	require.NoError(t, p.Start(ctx))

	addr := p.ListenAddr()
	p.Stop()

	_, err := net.Dial("tcp", addr)
	assert.Error(t, err, "should not be able to connect after stop")
}
