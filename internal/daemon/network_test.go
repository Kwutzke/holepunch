package daemon_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/Kwutzke/holepunch/internal/netwatch"
	"github.com/Kwutzke/holepunch/internal/resolver"
	mdns "github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const selfAddr = "127.0.0.1:15353"

type recordingUpstream struct {
	mu   sync.Mutex
	sets [][]string
}

func (r *recordingUpstream) SetUpstream(servers []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sets = append(r.sets, slices.Clone(servers))
}

func (r *recordingUpstream) all() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sets)
}

type dropperFunc func(ctx context.Context, reason string)

func (f dropperFunc) DropSessions(ctx context.Context, reason string) { f(ctx, reason) }

func TestNetworkFollower_Follow(t *testing.T) {
	t.Parallel()

	t.Run("after a network event an unowned name is forwarded to the upstream resolv.conf now names, before the sessions reconnect", func(t *testing.T) {
		t.Parallel()
		oldNet := startFakeUpstream(t, net.IPv4(192, 168, 68, 20))
		newNet := startFakeUpstream(t, net.IPv4(172, 20, 10, 20))
		// resolv.conf cannot name a port, so each fixture nameserver stands for one fake upstream.
		listening := map[string]string{"192.0.2.1:53": oldNet, "192.0.2.2:53": newNet}
		resolvConf := writeResolvConf(t, filepath.Join(t.TempDir(), "resolv.conf"), "nameserver 192.0.2.1\n")
		discover := func() ([]string, error) {
			servers, err := resolver.DiscoverFromConfig(resolvConf, selfAddr)
			for i, s := range servers {
				servers[i] = listening[s]
			}
			return servers, err
		}

		dnsAddr := freeUDPAddr(t)
		dnsServer := resolver.New(dnsAddr)
		require.NoError(t, dnsServer.Start())
		t.Cleanup(dnsServer.Stop)

		var answerAtDrop net.IP
		dropped := make(chan string, 1)
		sessions := dropperFunc(func(_ context.Context, reason string) {
			answerAtDrop = lookupA(t, dnsAddr, "web.dev")
			dropped <- reason
		})
		f := daemon.NewNetworkFollower(sessions, dnsServer, discover)
		f.RefreshUpstream()
		require.True(t, lookupA(t, dnsAddr, "web.dev").Equal(net.IPv4(192, 168, 68, 20)), "forwarded to the startup upstream")

		writeResolvConf(t, resolvConf, "nameserver 192.0.2.2\n")
		events := make(chan netwatch.Event, 1)
		ctx, cancel := context.WithCancel(t.Context())
		followed := make(chan struct{})
		go func() {
			f.Follow(ctx, events)
			close(followed)
		}()
		events <- netwatch.Event{Reason: "local addresses changed from [192.168.68.60] to [172.20.10.11]"}

		select {
		case reason := <-dropped:
			assert.Equal(t, "local addresses changed from [192.168.68.60] to [172.20.10.11]", reason)
		case <-time.After(2 * time.Second):
			t.Fatal("sessions were not dropped")
		}
		assert.True(t, answerAtDrop.Equal(net.IPv4(172, 20, 10, 20)), "answer at drop time came from %v, not the new upstream", answerAtDrop)

		cancel()
		select {
		case <-followed:
		case <-time.After(time.Second):
			t.Fatal("Follow did not return after cancel")
		}
	})
}

func TestNetworkFollower_RefreshUpstream(t *testing.T) {
	t.Parallel()

	t.Run("an unreadable resolv.conf keeps the current upstreams", func(t *testing.T) {
		t.Parallel()
		upstream := &recordingUpstream{}
		discoverErr := error(nil)
		f := daemon.NewNetworkFollower(dropperFunc(func(context.Context, string) {}), upstream, func() ([]string, error) {
			if discoverErr != nil {
				return nil, discoverErr
			}
			return []string{"192.168.68.1:53"}, nil
		})
		f.RefreshUpstream()

		discoverErr = errors.New("resolv.conf: no such file")
		f.RefreshUpstream()

		assert.Equal(t, [][]string{{"192.168.68.1:53"}}, upstream.all())
	})
}

func writeResolvConf(t *testing.T, path, content string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// startFakeUpstream runs a DNS server answering every A query with ip and returns its address.
func startFakeUpstream(t *testing.T, ip net.IP) string {
	t.Helper()
	addr := freeUDPAddr(t)
	mux := mdns.NewServeMux()
	mux.HandleFunc(".", func(w mdns.ResponseWriter, req *mdns.Msg) {
		reply := new(mdns.Msg)
		reply.SetReply(req)
		for _, q := range req.Question {
			reply.Answer = append(reply.Answer, &mdns.A{Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 60}, A: ip.To4()})
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

func lookupA(t *testing.T, addr, name string) net.IP {
	t.Helper()
	msg := new(mdns.Msg)
	msg.SetQuestion(mdns.Fqdn(name), mdns.TypeA)
	reply, _, err := new(mdns.Client).Exchange(msg, addr)
	require.NoError(t, err)
	for _, rr := range reply.Answer {
		if a, ok := rr.(*mdns.A); ok {
			return a.A
		}
	}
	return nil
}

func freeUDPAddr(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := conn.LocalAddr().String()
	require.NoError(t, conn.Close())
	return addr
}
