package proxy_test

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/Kwutzke/holepunch/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProxy_Start(t *testing.T) {
	t.Parallel()

	t.Run("forwards data", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()

		echoListener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = echoListener.Close() })

		go func() {
			for {
				conn, err := echoListener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer func() { _ = conn.Close() }()
					_, _ = io.Copy(conn, conn)
				}()
			}
		}()

		p := proxy.New("127.0.0.1:0", echoListener.Addr().String())
		require.NoError(t, p.Start(ctx))
		t.Cleanup(p.Stop)

		conn, err := net.Dial("tcp", p.ListenAddr())
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()

		msg := "hello through proxy"
		_, _ = fmt.Fprintln(conn, msg)
		_ = conn.(*net.TCPConn).CloseWrite()

		buf, err := io.ReadAll(conn)
		require.NoError(t, err)
		assert.Equal(t, msg+"\n", string(buf))
	})

	t.Run("multiple connections", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()

		echoListener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = echoListener.Close() })

		go func() {
			for {
				conn, err := echoListener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer func() { _ = conn.Close() }()
					_, _ = io.Copy(conn, conn)
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
			_, _ = fmt.Fprintln(conn, msg)
			_ = conn.(*net.TCPConn).CloseWrite()

			buf, err := io.ReadAll(conn)
			require.NoError(t, err)
			assert.Equal(t, msg+"\n", string(buf))
			_ = conn.Close()
		}
	})

	t.Run("target down", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()

		// Proxy points to a port with nothing listening.
		p := proxy.New("127.0.0.1:0", "127.0.0.1:1")
		require.NoError(t, p.Start(ctx))
		t.Cleanup(p.Stop)

		conn, err := net.Dial("tcp", p.ListenAddr())
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()

		buf, _ := io.ReadAll(conn)
		assert.Empty(t, buf)
	})
}

func TestProxy_Stop(t *testing.T) {
	t.Parallel()

	t.Run("stops", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()

		p := proxy.New("127.0.0.1:0", "127.0.0.1:1")
		require.NoError(t, p.Start(ctx))

		addr := p.ListenAddr()
		p.Stop()

		_, err := net.Dial("tcp", addr)
		assert.Error(t, err, "should not be able to connect after stop")
	})

	t.Run("with an idle connection", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()

		// The silent target stands in for an SSM tunnel still open after its session ended.
		targetListener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = targetListener.Close() })

		accepted := make(chan net.Conn, 1)
		go func() {
			conn, err := targetListener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}()

		p := proxy.New("127.0.0.1:0", targetListener.Addr().String())
		require.NoError(t, p.Start(ctx))

		client, err := net.Dial("tcp", p.ListenAddr())
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })

		// A Stop that runs before the target conn is tracked passes without reaching the hang.
		select {
		case target := <-accepted:
			t.Cleanup(func() { _ = target.Close() })
		case <-time.After(2 * time.Second):
			t.Fatal("proxy did not dial target")
		}

		done := make(chan struct{})
		go func() {
			p.Stop()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Stop() hung on idle in-flight connection")
		}
	})
}
