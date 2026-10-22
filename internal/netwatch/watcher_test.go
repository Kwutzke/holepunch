package netwatch

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const interval = 10 * time.Second

// host is a fake wall clock and interface list.
type host struct {
	now     time.Time
	addrs   []net.Addr
	addrErr error
}

func newHost(addrs []net.Addr) *host {
	return &host{now: time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC), addrs: addrs}
}

func (h *host) advance(d time.Duration) {
	h.now = h.now.Add(d)
}

func (h *host) watcher() *Watcher {
	return newWatcher(interval,
		func() time.Time { return h.now },
		func() ([]net.Addr, error) { return h.addrs, h.addrErr },
	)
}

func TestWatcher_tick(t *testing.T) {
	t.Parallel()

	wifi := []net.Addr{ipNet("127.0.0.1"), ipNet("192.168.68.60"), ipNet("fe80::1")}
	hotspot := []net.Addr{ipNet("127.0.0.1"), ipNet("172.20.10.11"), ipNet("fe80::1")}

	t.Run("steady ticks emit nothing", func(t *testing.T) {
		t.Parallel()
		h := newHost(wifi)
		w := h.watcher()

		for range 5 {
			w.tick()
			h.advance(interval)
		}

		assert.Empty(t, drain(w))
	})

	t.Run("a sleep gap emits one event", func(t *testing.T) {
		t.Parallel()
		h := newHost(wifi)
		w := h.watcher()
		w.tick()

		h.advance(10 * time.Minute)
		w.tick()
		events := drain(w)
		require.Len(t, events, 1)
		assert.Contains(t, events[0].Reason, "host resumed from sleep (10m0s")

		h.advance(interval)
		w.tick()
		assert.Empty(t, drain(w), "the tick after waking is steady again")
	})

	t.Run("a late tick within twice the interval is not a sleep", func(t *testing.T) {
		t.Parallel()
		h := newHost(wifi)
		w := h.watcher()
		w.tick()

		h.advance(2 * interval)
		w.tick()

		assert.Empty(t, drain(w))
	})

	t.Run("an IPv4 address change emits one event", func(t *testing.T) {
		t.Parallel()
		h := newHost(wifi)
		w := h.watcher()
		w.tick()

		h.addrs = hotspot
		h.advance(interval)
		w.tick()
		events := drain(w)
		require.Len(t, events, 1)
		assert.Equal(t, "local addresses changed from [192.168.68.60] to [172.20.10.11]", events[0].Reason)

		h.advance(interval)
		w.tick()
		assert.Empty(t, drain(w), "the new address set is steady")
	})

	t.Run("loopback and IPv6 changes emit nothing", func(t *testing.T) {
		t.Parallel()
		h := newHost(wifi)
		w := h.watcher()
		w.tick()

		h.addrs = []net.Addr{ipNet("127.0.0.2"), ipNet("192.168.68.60"), ipNet("2001:db8::7")}
		h.advance(interval)
		w.tick()

		assert.Empty(t, drain(w))
	})

	t.Run("a failed address listing neither fires nor hides the next change", func(t *testing.T) {
		t.Parallel()
		h := newHost(wifi)
		w := h.watcher()
		w.tick()

		h.addrErr = errors.New("route socket busy")
		h.advance(interval)
		w.tick()
		assert.Empty(t, drain(w))

		h.addrErr = nil
		h.addrs = hotspot
		h.advance(interval)
		w.tick()
		assert.Len(t, drain(w), 1)
	})

	t.Run("an unread event stands for the next one", func(t *testing.T) {
		t.Parallel()
		h := newHost(wifi)
		w := h.watcher()
		w.tick()

		h.advance(10 * time.Minute)
		w.tick()
		h.addrs = hotspot
		h.advance(interval)
		w.tick()

		assert.Len(t, drain(w), 1)
	})
}

func TestWatcher_Run(t *testing.T) {
	t.Parallel()

	w := New(5 * time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	returned := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(returned)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func drain(w *Watcher) []Event {
	var events []Event
	for {
		select {
		case evt := <-w.Events():
			events = append(events, evt)
		default:
			return events
		}
	}
}

func ipNet(ip string) net.Addr {
	return &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(24, 32)}
}
