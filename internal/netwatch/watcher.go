// Package netwatch reports network events: moments after which every connection the host held to
// a remote peer must be presumed dead, because the host slept or its addresses changed.
package netwatch

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
)

// Event is one network event.
type Event struct {
	Reason string
	Time   time.Time
}

// Watcher ticks on a fixed interval and emits an Event when the wall clock jumped past twice the
// interval since the previous tick (the host slept) or the set of local non-loopback IPv4
// addresses changed. IPv6 is left out: temporary addresses rotate on their own, and each rotation
// would read as a network change.
type Watcher struct {
	interval time.Duration
	now      func() time.Time
	addrs    func() ([]net.Addr, error)
	events   chan Event

	lastTick  time.Time
	lastAddrs []string
}

// New returns a Watcher of the real wall clock and interfaces.
func New(interval time.Duration) *Watcher {
	return newWatcher(interval, time.Now, net.InterfaceAddrs)
}

func newWatcher(interval time.Duration, now func() time.Time, addrs func() ([]net.Addr, error)) *Watcher {
	return &Watcher{
		interval: interval,
		now:      now,
		addrs:    addrs,
		events:   make(chan Event, 1),
	}
}

// Events delivers network events. An event still unread when the next one happens stands for both.
func (w *Watcher) Events() <-chan Event {
	return w.events
}

// Run ticks until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	w.tick()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick()
		}
	}
}

func (w *Watcher) tick() {
	// Round(0) drops the monotonic reading, which stops while the host sleeps.
	now := w.now().Round(0)
	addrs, addrErr := w.addrs()

	var reasons []string
	if !w.lastTick.IsZero() {
		if gap := now.Sub(w.lastTick); gap > 2*w.interval {
			reasons = append(reasons, fmt.Sprintf("host resumed from sleep (%v since the last check)", gap.Round(time.Second)))
		}
	}
	w.lastTick = now

	// A failed listing keeps the previous set, so it neither fires nor masks the next change.
	if addrErr == nil {
		current := ipv4Set(addrs)
		if w.lastAddrs != nil && !slices.Equal(current, w.lastAddrs) {
			reasons = append(reasons, fmt.Sprintf("local addresses changed from [%s] to [%s]", strings.Join(w.lastAddrs, " "), strings.Join(current, " ")))
		}
		w.lastAddrs = current
	}

	if len(reasons) == 0 {
		return
	}
	select {
	case w.events <- Event{Reason: strings.Join(reasons, "; "), Time: now}:
	default:
	}
}

// ipv4Set returns the sorted non-loopback IPv4 addresses, never nil.
func ipv4Set(addrs []net.Addr) []string {
	set := []string{}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.To4() == nil || ipNet.IP.IsLoopback() {
			continue
		}
		set = append(set, ipNet.IP.String())
	}
	slices.Sort(set)
	return slices.Compact(set)
}
