package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/Kwutzke/holepunch/internal/engine"
)

// A profile with many services reports the same credential expiry once per service.
const notifyDedupeWindow = 30 * time.Second

// Notifier posts a desktop notification on CredentialsExpired. It stays silent while a tray
// client is connected, because the tray shows the expiry and a login item in its menu.
type Notifier struct {
	bus         *Bus
	runFn       osascriptFunc
	trayPresent trayPresentFunc
	cancel      context.CancelFunc

	mu       sync.Mutex
	lastFire map[string]time.Time
}

// NewNotifier notifies unconditionally when trayPresent is nil.
func NewNotifier(bus *Bus, trayPresent trayPresentFunc) *Notifier {
	n := &Notifier{
		bus:         bus,
		runFn:       defaultOsascript,
		trayPresent: trayPresent,
		lastFire:    make(map[string]time.Time),
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	events, unsub := bus.Subscribe()
	go n.run(ctx, events, unsub)
	return n
}

func (n *Notifier) Stop() {
	if n.cancel != nil {
		n.cancel()
	}
}

// run takes a subscription made before the goroutine starts, so no event published after
// NewNotifier returns is missed.
func (n *Notifier) run(ctx context.Context, events <-chan engine.Event, unsub func()) {
	defer unsub()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			ce, isCE := evt.(engine.CredentialsExpired)
			if !isCE {
				continue
			}
			if n.trayPresent != nil && n.trayPresent() {
				continue
			}
			if !n.shouldFire(ce) {
				continue
			}
			msg := "SSO expired for " + ce.Profile + " — run: aws sso login --profile " + ce.AWSProfile
			_ = n.runFn(ctx, "holepunch", msg)
		}
	}
}

func (n *Notifier) shouldFire(ce engine.CredentialsExpired) bool {
	key := ce.Profile + "\x00" + ce.AWSProfile
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	if last, ok := n.lastFire[key]; ok && now.Sub(last) < notifyDedupeWindow {
		return false
	}
	n.lastFire[key] = now
	return true
}

type osascriptFunc func(ctx context.Context, title, message string) error

type trayPresentFunc func() bool
