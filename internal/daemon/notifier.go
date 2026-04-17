package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/Kwutzke/holepunch/internal/engine"
)

// notifyDedupeWindow suppresses repeat notifications for the same
// (profile, aws profile) pair within this window — a profile with many
// services would otherwise fire one OS-level notification per service on
// the same credential expiry.
const notifyDedupeWindow = 30 * time.Second

// osascriptFunc is injected for testing. It is called with the message text.
// Production uses the platform-specific implementation in notifier_darwin.go;
// other platforms use a no-op.
type osascriptFunc func(ctx context.Context, title, message string) error

// trayPresentFunc reports whether a tray client is currently registered
// with the daemon. When true, the notifier suppresses its own fallback
// notification since the tray will surface the event itself.
type trayPresentFunc func() bool

// Notifier subscribes to engine events via the bus and fires a native
// OS-level notification on CredentialsExpired. It exists so the user sees
// something actionable without having to open `holepunch logs -f`.
type Notifier struct {
	bus         *Bus
	runFn       osascriptFunc
	trayPresent trayPresentFunc
	cancel      context.CancelFunc

	mu       sync.Mutex
	lastFire map[string]time.Time
}

// NewNotifier starts a notifier goroutine subscribed to the given bus.
// trayPresent is consulted on each CredentialsExpired: if it returns true,
// the fallback osascript is skipped (the tray handles it). May be nil for
// "always fire" behavior (used in tests).
func NewNotifier(bus *Bus, trayPresent trayPresentFunc) *Notifier {
	n := &Notifier{
		bus:         bus,
		runFn:       defaultOsascript,
		trayPresent: trayPresent,
		lastFire:    make(map[string]time.Time),
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	go n.run(ctx)
	return n
}

// Stop terminates the notifier's event loop.
func (n *Notifier) Stop() {
	if n.cancel != nil {
		n.cancel()
	}
}

func (n *Notifier) run(ctx context.Context) {
	events, unsub := n.bus.Subscribe()
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
