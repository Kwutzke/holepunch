package resolver

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	mdns "github.com/miekg/dns"
)

// Resolver answers for registered names on a non-privileged port, where the
// /etc/resolver/ files written by setup route each configured TLD. Unregistered
// names are forwarded upstream because claiming a TLD would otherwise
// black-hole the real domains under it (web.dev once foo.dev is registered).
type Resolver struct {
	mu       sync.RWMutex
	records  map[string]net.IP // FQDN (with trailing dot) -> IP
	server   *mdns.Server
	addr     string
	upstream []string     // "ip:port" servers; empty disables forwarding
	client   *mdns.Client // lazily initialized, guarded by mu
}

// New creates a Resolver that will listen on the given address (e.g. "127.0.0.1:15353").
func New(addr string) *Resolver {
	r := &Resolver{
		records: make(map[string]net.IP),
		addr:    addr,
	}
	return r
}

// SetUpstream configures the upstream nameservers used to forward queries for
// names not registered with the resolver. Servers are addresses of the form
// "ip:port". Passing nil or an empty slice disables forwarding. Call before Start.
func (r *Resolver) SetUpstream(servers []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upstream = append([]string(nil), servers...)
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
	if len(req.Question) == 0 {
		msg := new(mdns.Msg)
		msg.SetReply(req)
		_ = w.WriteMsg(msg)
		return
	}

	q := req.Question[0]
	r.mu.RLock()
	ip, owned := r.records[q.Name]
	upstream := r.upstream
	r.mu.RUnlock()

	if owned {
		msg := new(mdns.Msg)
		msg.SetReply(req)
		msg.Authoritative = true
		if q.Qtype == mdns.TypeA {
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
		_ = w.WriteMsg(msg)
		return
	}

	if len(upstream) == 0 {
		msg := new(mdns.Msg)
		msg.SetReply(req)
		msg.Authoritative = true
		_ = w.WriteMsg(msg)
		return
	}

	reply, err := r.forward(req, upstream)
	if err != nil {
		msg := new(mdns.Msg)
		msg.SetReply(req)
		msg.Rcode = mdns.RcodeServerFailure
		_ = w.WriteMsg(msg)
		return
	}
	reply.Id = req.Id
	reply.Authoritative = false
	reply.RecursionAvailable = true
	_ = w.WriteMsg(reply)
}

// forward sends req to each upstream in order and returns the first successful
// reply. Returns an error only when all upstreams fail.
func (r *Resolver) forward(req *mdns.Msg, upstream []string) (*mdns.Msg, error) {
	client := r.forwardClient()
	var lastErr error
	for _, server := range upstream {
		reply, _, err := client.Exchange(req, server)
		if err == nil {
			return reply, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (r *Resolver) forwardClient() *mdns.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client == nil {
		r.client = &mdns.Client{Timeout: 2 * time.Second}
	}
	return r.client
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
