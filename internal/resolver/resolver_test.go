package resolver_test

import (
	"fmt"
	"net"
	"sync/atomic"
	"testing"

	mdns "github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/Kwutzke/holepunch/internal/resolver"
)

var nextPort atomic.Int32

func init() {
	nextPort.Store(15400)
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
	c := new(mdns.Client)
	msg := new(mdns.Msg)
	msg.SetQuestion(mdns.Fqdn(hostname), mdns.TypeA)

	resp, _, err := c.Exchange(msg, addr)
	require.NoError(t, err)

	var ips []net.IP
	for _, ans := range resp.Answer {
		if a, ok := ans.(*mdns.A); ok {
			ips = append(ips, a.A)
		}
	}
	return ips
}

func TestResolverBasic(t *testing.T) {
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
}

func TestResolverUnknownHost(t *testing.T) {
	t.Parallel()

	_, addr := startTestResolver(t)

	ips := queryA(t, addr, "nonexistent.dev")
	assert.Empty(t, ips, "unknown host should return no answers")
}

func TestResolverUnregister(t *testing.T) {
	t.Parallel()

	r, addr := startTestResolver(t)
	r.Register("temp.dev", net.IPv4(127, 0, 0, 10))

	ips := queryA(t, addr, "temp.dev")
	require.Len(t, ips, 1)

	r.Unregister("temp.dev")

	ips = queryA(t, addr, "temp.dev")
	assert.Empty(t, ips, "unregistered host should return no answers")
}

func TestResolverRecords(t *testing.T) {
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
