package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
)

// Proxy forwards TCP connections from a listen address to a target address.
type Proxy struct {
	listenAddr string
	targetAddr string
	listener   net.Listener
	wg         sync.WaitGroup
}

// New creates a proxy that listens on listenAddr and forwards to targetAddr.
// listenAddr example: "127.0.0.119:443"
// targetAddr example: "127.0.0.1:59832"
func New(listenAddr, targetAddr string) *Proxy {
	return &Proxy{
		listenAddr: listenAddr,
		targetAddr: targetAddr,
	}
}

// Start begins accepting connections. Non-blocking.
func (p *Proxy) Start(ctx context.Context) error {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("proxy listen on %s: %w", p.listenAddr, err)
	}
	p.listener = listener

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.acceptLoop(ctx)
	}()

	return nil
}

// Stop stops accepting new connections and waits for existing ones to finish.
func (p *Proxy) Stop() {
	if p.listener != nil {
		p.listener.Close()
	}
	p.wg.Wait()
}

// ListenAddr returns the actual address the proxy is listening on.
func (p *Proxy) ListenAddr() string {
	if p.listener != nil {
		return p.listener.Addr().String()
	}
	return p.listenAddr
}

func (p *Proxy) acceptLoop(ctx context.Context) {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				return // listener closed
			}
		}

		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handleConn(ctx, conn)
		}()
	}
}

func (p *Proxy) handleConn(ctx context.Context, src net.Conn) {
	defer src.Close()

	var d net.Dialer
	dst, err := d.DialContext(ctx, "tcp", p.targetAddr)
	if err != nil {
		return
	}
	defer dst.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(dst, src)
		dst.(*net.TCPConn).CloseWrite()
	}()
	go func() {
		defer wg.Done()
		io.Copy(src, dst)
		src.(*net.TCPConn).CloseWrite()
	}()
	wg.Wait()
}
