package resolver

import (
	"context"

	"github.com/Kwutzke/holepunch/internal/dns"
)

// DNSAdapter implements engine.DNSManager by registering/unregistering
// hostnames with the embedded DNS resolver instead of writing /etc/hosts.
type DNSAdapter struct {
	resolver *Resolver
}

// NewDNSAdapter creates an adapter that bridges engine DNS operations to the resolver.
func NewDNSAdapter(r *Resolver) *DNSAdapter {
	return &DNSAdapter{resolver: r}
}

func (a *DNSAdapter) Add(_ context.Context, entries []dns.Entry) error {
	for _, e := range entries {
		a.resolver.Register(e.Hostname, e.IP)
	}
	return nil
}

func (a *DNSAdapter) Remove(_ context.Context) error {
	for hostname := range a.resolver.Records() {
		a.resolver.Unregister(hostname)
	}
	return nil
}

func (a *DNSAdapter) RemoveEntries(_ context.Context, entries []dns.Entry) error {
	for _, e := range entries {
		a.resolver.Unregister(e.Hostname)
	}
	return nil
}
