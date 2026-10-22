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

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	stopped bool
}

// New creates a proxy that listens on listenAddr and forwards to targetAddr.
// listenAddr example: "127.0.0.119:443"
// targetAddr example: "127.0.0.1:59832"
func New(listenAddr, targetAddr string) *Proxy {
	return &Proxy{
		listenAddr: listenAddr,
		targetAddr: targetAddr,
		conns:      make(map[net.Conn]struct{}),
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

// Stop stops accepting new connections, force-closes any in-flight ones, and
// waits for their handler goroutines to exit. Force-closing is required:
// idle long-lived connections (e.g. an open psql session) leave io.Copy
// blocked on Read forever, which would otherwise make Stop hang — and the
// engine calls this during shutdown while holding a per-service lock.
func (p *Proxy) Stop() {
	p.mu.Lock()
	p.stopped = true
	toClose := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		toClose = append(toClose, c)
	}
	p.mu.Unlock()

	if p.listener != nil {
		p.listener.Close()
	}
	for _, c := range toClose {
		_ = c.Close()
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
	if !p.track(src) {
		return
	}
	defer p.untrack(src)

	var d net.Dialer
	dst, err := d.DialContext(ctx, "tcp", p.targetAddr)
	if err != nil {
		return
	}
	defer dst.Close()
	if !p.track(dst) {
		return
	}
	defer p.untrack(dst)

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

// track registers a conn so Stop can close it. Returns false if Stop has
// already run — caller must close the conn and return.
func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}
