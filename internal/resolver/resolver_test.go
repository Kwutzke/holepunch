package resolver_test

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Kwutzke/holepunch/internal/resolver"
	mdns "github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var nextPort atomic.Int32

func init() {
	nextPort.Store(15400)
}

func TestResolver_Register(t *testing.T) {
	t.Parallel()

	t.Run("answers a registered name", func(t *testing.T) {
		t.Parallel()

		r, addr := startTestResolver(t)
		r.Register("opensearch.dev", net.IPv4(127, 0, 0, 2))
		r.Register("rds.dev", net.IPv4(127, 0, 0, 3))

		ips := queryA(t, addr, "opensearch.dev")
		require.Len(t, ips, 1)
		assert.True(t, ips[0].Equal(net.IPv4(127, 0, 0, 2)), "got %v", ips[0])

		ips = queryA(t, addr, "rds.dev")
		require.Len(t, ips, 1)
		assert.True(t, ips[0].Equal(net.IPv4(127, 0, 0, 3)), "got %v", ips[0])
	})

	t.Run("unknown host", func(t *testing.T) {
		t.Parallel()

		_, addr := startTestResolver(t)

		ips := queryA(t, addr, "nonexistent.dev")
		assert.Empty(t, ips, "unknown host should return no answers")
	})
}

func TestResolver_Unregister(t *testing.T) {
	t.Parallel()

	r, addr := startTestResolver(t)
	r.Register("temp.dev", net.IPv4(127, 0, 0, 10))

	ips := queryA(t, addr, "temp.dev")
	require.Len(t, ips, 1)

	r.Unregister("temp.dev")

	ips = queryA(t, addr, "temp.dev")
	assert.Empty(t, ips, "unregistered host should return no answers")
}

func TestResolver_Records(t *testing.T) {
	t.Parallel()

	r := resolver.New("127.0.0.1:0") // not started, just testing registration
	r.Register("a.dev", net.IPv4(127, 0, 0, 2))
	r.Register("b.dev", net.IPv4(127, 0, 0, 3))

	records := r.Records()
	assert.Len(t, records, 2)
	assert.Contains(t, records, "a.dev.")
	assert.Contains(t, records, "b.dev.")
}

func TestExtractTLDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		hostnames []string
		expected  []string
	}{
		{"single TLD", []string{"a.dev", "b.dev"}, []string{"dev"}},
		{"multiple TLDs", []string{"a.dev", "b.prod", "c.staging"}, []string{"dev", "prod", "staging"}},
		{"deduplication", []string{"a.dev", "b.dev", "c.prod", "d.prod"}, []string{"dev", "prod"}},
		{"empty", nil, nil},
		{"single hostname", []string{"opensearch.dev"}, []string{"dev"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolver.ExtractTLDs(tt.hostnames)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestResolver_SetUpstream(t *testing.T) {
	t.Parallel()

	t.Run("forwards unregistered names", func(t *testing.T) {
		t.Parallel()

		var mu sync.Mutex
		var forwarded []string
		upstream := startFakeUpstream(t,
			map[string]net.IP{"real.dev.": net.IPv4(93, 184, 216, 34)},
			func(q mdns.Question) {
				mu.Lock()
				defer mu.Unlock()
				forwarded = append(forwarded, q.Name)
			},
		)

		r, addr := startTestResolver(t)
		r.SetUpstream([]string{upstream})

		resp := exchange(t, addr, "real.dev", mdns.TypeA)
		require.Len(t, resp.Answer, 1)
		a, ok := resp.Answer[0].(*mdns.A)
		require.True(t, ok)
		assert.True(t, a.A.Equal(net.IPv4(93, 184, 216, 34)))
		assert.False(t, resp.Authoritative, "forwarded reply must not claim authority")
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, []string{"real.dev."}, forwarded)
	})

	t.Run("does not forward an owned name", func(t *testing.T) {
		t.Parallel()

		var mu sync.Mutex
		var forwarded []string
		upstream := startFakeUpstream(t, nil, func(q mdns.Question) {
			mu.Lock()
			defer mu.Unlock()
			forwarded = append(forwarded, q.Name)
		})

		r, addr := startTestResolver(t)
		r.SetUpstream([]string{upstream})
		r.Register("svc.dev", net.IPv4(127, 0, 0, 2))

		resp := exchange(t, addr, "svc.dev", mdns.TypeAAAA)
		assert.Empty(t, resp.Answer, "AAAA on owned A-only record is NODATA")
		assert.True(t, resp.Authoritative, "owned names answer authoritatively")
		assert.Equal(t, mdns.RcodeSuccess, resp.Rcode)
		mu.Lock()
		defer mu.Unlock()
		assert.Empty(t, forwarded, "owned names must never be forwarded upstream")
	})

	t.Run("upstream failure", func(t *testing.T) {
		t.Parallel()

		r, addr := startTestResolver(t)
		// 127.0.0.1:1 is closed — exchange will fail.
		r.SetUpstream([]string{"127.0.0.1:1"})

		resp := exchange(t, addr, "unreachable.dev", mdns.TypeA)
		assert.Equal(t, mdns.RcodeServerFailure, resp.Rcode)
	})
}

func startTestResolver(t *testing.T) (*resolver.Resolver, string) {
	t.Helper()
	port := int(nextPort.Add(1))
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	r := resolver.New(addr)
	require.NoError(t, r.Start())
	t.Cleanup(r.Stop)
	return r, addr
}

func queryA(t *testing.T, addr, hostname string) []net.IP {
	t.Helper()
	resp := exchange(t, addr, hostname, mdns.TypeA)
	var ips []net.IP
	for _, ans := range resp.Answer {
		if a, ok := ans.(*mdns.A); ok {
			ips = append(ips, a.A)
		}
	}
	return ips
}

func exchange(t *testing.T, addr, hostname string, qtype uint16) *mdns.Msg {
	t.Helper()
	c := new(mdns.Client)
	msg := new(mdns.Msg)
	msg.SetQuestion(mdns.Fqdn(hostname), qtype)

	resp, _, err := c.Exchange(msg, addr)
	require.NoError(t, err)
	return resp
}

// startFakeUpstream runs a DNS server that answers only for the provided
// name->IP map. Queries for other names return NXDOMAIN. onQuery, if non-nil,
// is invoked for every incoming query (useful to assert forwarding occurred
// or didn't).
func startFakeUpstream(t *testing.T, answers map[string]net.IP, onQuery func(q mdns.Question)) string {
	t.Helper()
	port := int(nextPort.Add(1))
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	mux := mdns.NewServeMux()
	mux.HandleFunc(".", func(w mdns.ResponseWriter, req *mdns.Msg) {
		reply := new(mdns.Msg)
		reply.SetReply(req)
		if len(req.Question) > 0 && onQuery != nil {
			onQuery(req.Question[0])
		}
		for _, q := range req.Question {
			ip, ok := answers[q.Name]
			if !ok {
				reply.Rcode = mdns.RcodeNameError
				continue
			}
			if q.Qtype == mdns.TypeA {
				reply.Answer = append(reply.Answer, &mdns.A{
					Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 60},
					A:   ip.To4(),
				})
			}
		}
		_ = w.WriteMsg(reply)
	})

	ready := make(chan struct{})
	srv := &mdns.Server{Addr: addr, Net: "udp", Handler: mux, NotifyStartedFunc: func() { close(ready) }}
	go func() { _ = srv.ListenAndServe() }()
	<-ready
	t.Cleanup(func() { _ = srv.Shutdown() })
	return addr
}
