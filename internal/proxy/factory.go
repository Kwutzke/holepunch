package proxy

import "context"

// StartNew creates and starts a proxy, returning it. Convenience for callers
// that need to create proxies programmatically.
func StartNew(ctx context.Context, listenAddr, targetAddr string) (*Proxy, error) {
	p := New(listenAddr, targetAddr)
	if err := p.Start(ctx); err != nil {
		return nil, err
	}
	return p, nil
}
