package resolver

import (
	"fmt"
	"net"
	"strings"
	"sync"

	mdns "github.com/miekg/dns"
)

// Resolver is an embedded DNS server that resolves configured hostnames
// to their assigned loopback IPs. It listens on a high port (no root needed)
// and macOS /etc/resolver/ files route queries to it.
type Resolver struct {
	mu      sync.RWMutex
	records map[string]net.IP // FQDN (with trailing dot) -> IP
	server  *mdns.Server
	addr    string
}

// New creates a Resolver that will listen on the given address (e.g. "127.0.0.1:15353").
func New(addr string) *Resolver {
	r := &Resolver{
		records: make(map[string]net.IP),
		addr:    addr,
	}
	return r
}

// Start begins serving DNS queries. Non-blocking.
func (r *Resolver) Start() error {
	mux := mdns.NewServeMux()
	mux.HandleFunc(".", r.handleDNS)

	ready := make(chan struct{})
	r.server = &mdns.Server{
		Addr:              r.addr,
		Net:               "udp",
		Handler:           mux,
		NotifyStartedFunc: func() { close(ready) },
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.server.ListenAndServe()
	}()

	// Wait until the server is ready or fails.
	select {
	case err := <-errCh:
		return fmt.Errorf("DNS server failed to start: %w", err)
	case <-ready:
		return nil
	}
}

// Stop shuts down the DNS server.
func (r *Resolver) Stop() {
	if r.server != nil {
		r.server.Shutdown()
	}
}

// Register adds a hostname -> IP mapping. The hostname should NOT have a trailing dot.
func (r *Resolver) Register(hostname string, ip net.IP) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fqdn := mdns.Fqdn(hostname)
	r.records[fqdn] = ip
}

// Unregister removes a hostname mapping.
func (r *Resolver) Unregister(hostname string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fqdn := mdns.Fqdn(hostname)
	delete(r.records, fqdn)
}

// Records returns a copy of all registered hostname -> IP mappings.
func (r *Resolver) Records() map[string]net.IP {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]net.IP, len(r.records))
	for k, v := range r.records {
		result[k] = v
	}
	return result
}

func (r *Resolver) handleDNS(w mdns.ResponseWriter, req *mdns.Msg) {
	msg := new(mdns.Msg)
	msg.SetReply(req)
	msg.Authoritative = true

	for _, q := range req.Question {
		if q.Qtype != mdns.TypeA && q.Qtype != mdns.TypeAAAA {
			continue
		}

		r.mu.RLock()
		ip, ok := r.records[q.Name]
		r.mu.RUnlock()

		if ok && q.Qtype == mdns.TypeA {
			msg.Answer = append(msg.Answer, &mdns.A{
				Hdr: mdns.RR_Header{
					Name:   q.Name,
					Rrtype: mdns.TypeA,
					Class:  mdns.ClassINET,
					Ttl:    1, // short TTL since mappings can change
				},
				A: ip.To4(),
			})
		}
	}

	w.WriteMsg(msg)
}

// ExtractTLDs returns the unique top-level domains from a list of hostnames.
// e.g. ["opensearch.dev", "rds.dev", "opensearch.prod"] -> ["dev", "prod"]
func ExtractTLDs(hostnames []string) []string {
	seen := make(map[string]bool)
	var tlds []string
	for _, h := range hostnames {
		parts := strings.Split(h, ".")
		if len(parts) >= 2 {
			tld := parts[len(parts)-1]
			if !seen[tld] {
				seen[tld] = true
				tlds = append(tlds, tld)
			}
		}
	}
	return tlds
}
