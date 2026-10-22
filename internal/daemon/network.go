package daemon

import (
	"context"
	"log"

	"github.com/Kwutzke/holepunch/internal/netwatch"
)

// SessionDropper ends every connected session so each reconnects over the current network.
type SessionDropper interface {
	DropSessions(ctx context.Context, reason string)
}

// UpstreamSetter receives the DNS servers that names holepunch does not own are forwarded to.
type UpstreamSetter interface {
	SetUpstream(servers []string)
}

// NetworkFollower keeps what depends on the network current across network events: the DNS
// upstreams and the SSM sessions.
type NetworkFollower struct {
	sessions SessionDropper
	dns      UpstreamSetter
	discover func() ([]string, error)
}

// NewNetworkFollower returns a NetworkFollower that reads the DNS upstreams with discover.
func NewNetworkFollower(sessions SessionDropper, dns UpstreamSetter, discover func() ([]string, error)) *NetworkFollower {
	return &NetworkFollower{sessions: sessions, dns: dns, discover: discover}
}

// Follow handles each network event until ctx is done: it re-reads the DNS upstreams, then drops
// every connected session.
func (f *NetworkFollower) Follow(ctx context.Context, events <-chan netwatch.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-events:
			log.Printf("network event: %s", evt.Reason)
			f.RefreshUpstream()
			f.sessions.DropSessions(ctx, evt.Reason)
		}
	}
}

// RefreshUpstream re-reads the DNS upstreams. When they cannot be read the current ones stay.
func (f *NetworkFollower) RefreshUpstream() {
	upstream, err := f.discover()
	if err != nil {
		log.Printf("DNS forwarding unchanged (discover upstream: %v)", err)
		return
	}
	f.dns.SetUpstream(upstream)
	if len(upstream) == 0 {
		log.Printf("DNS forwarding disabled (no upstream resolvers found)")
		return
	}
	log.Printf("DNS forwarding enabled (upstream: %v)", upstream)
}
