package engine

import (
	"context"
	"net"

	"github.com/Kwutzke/holepunch/internal/config"
	"github.com/Kwutzke/holepunch/internal/dns"
	"github.com/Kwutzke/holepunch/internal/session"
)

// DNSManager is the interface the engine uses for DNS operations.
// Satisfied by *dns.Manager.
type DNSManager interface {
	Add(ctx context.Context, entries []dns.Entry) error
	Remove(ctx context.Context) error
	RemoveEntries(ctx context.Context, entries []dns.Entry) error
}

// SessionManager is the interface the engine uses for SSM sessions.
// Satisfied by *session.SSMManager.
type SessionManager interface {
	Start(ctx context.Context, params session.StartParams) (session.Session, error)
}

// BastionResolver resolves the SSM target instance ID for a profile. The
// engine calls Resolve before each SSM session start and Invalidate when a
// session fails with session.ErrTargetNotConnected so the next attempt
// picks a freshly-running instance. Satisfied by *bastion.ASGResolver.
type BastionResolver interface {
	Resolve(ctx context.Context, profile config.Profile) (string, error)
	Invalidate(profile config.Profile)
}

// IPAllocator is the interface the engine uses for IP allocation.
// Satisfied by *ip.Allocator.
type IPAllocator interface {
	Allocate(key string) (net.IP, error)
	Release(key string)
}

// TCPProxy represents a running TCP proxy.
type TCPProxy interface {
	Stop()
	ListenAddr() string
}

// ProxyFactory creates TCP or signing proxies. Allows mocking in tests.
type ProxyFactory interface {
	NewProxy(ctx context.Context, cfg ProxyConfig) (TCPProxy, error)
}

// ProxyConfig holds parameters for creating a proxy.
type ProxyConfig struct {
	ListenAddr   string
	TargetAddr   string
	Sigv4Service string // if non-empty, use a signing reverse proxy
	RealHost     string // the real AWS hostname for sigv4 signing
	AWSProfile   string
	AWSRegion    string
}
