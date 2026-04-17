package daemon

import (
	"sync"

	"github.com/Kwutzke/holepunch/internal/engine"
)

// subscriberBuffer is the per-subscriber channel depth. Slow readers past
// this threshold lose events (not the whole stream) so one stuck client
// cannot block the engine or other subscribers.
const subscriberBuffer = 128

// Bus fans out engine events to multiple subscribers. The engine emits to a
// single channel; Bus drains it and dispatches non-blocking writes to each
// subscriber. Drop-on-full per subscriber preserves isolation.
type Bus struct {
	mu     sync.RWMutex
	subs   map[chan engine.Event]struct{}
	closed bool
}

// NewBus starts a goroutine draining src and dispatching events to
// subscribers. The goroutine exits when src closes.
func NewBus(src <-chan engine.Event) *Bus {
	b := &Bus{subs: make(map[chan engine.Event]struct{})}
	go b.run(src)
	return b
}

func (b *Bus) run(src <-chan engine.Event) {
	for evt := range src {
		b.mu.RLock()
		for ch := range b.subs {
			select {
			case ch <- evt:
			default:
				// Subscriber backlogged — drop for them only.
			}
		}
		b.mu.RUnlock()
	}
	b.mu.Lock()
	b.closed = true
	for ch := range b.subs {
		close(ch)
		delete(b.subs, ch)
	}
	b.mu.Unlock()
}

// Subscribe returns a new subscriber channel and a cancel function that
// unregisters and closes it. The channel is closed when either cancel is
// called or the source channel closes.
func (b *Bus) Subscribe() (<-chan engine.Event, func()) {
	ch := make(chan engine.Event, subscriberBuffer)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subs[ch]; ok {
				delete(b.subs, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}
	return ch, cancel
}
