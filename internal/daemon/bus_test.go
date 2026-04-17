package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/Kwutzke/holepunch/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBus_FanoutDeliversToEverySubscriber(t *testing.T) {
	t.Parallel()

	src := make(chan engine.Event)
	bus := NewBus(src)

	const numSubs = 3
	const numEvents = 50 // well under subscriberBuffer so no drops happen

	subs := make([]<-chan engine.Event, numSubs)
	cancels := make([]func(), numSubs)
	for i := range subs {
		subs[i], cancels[i] = bus.Subscribe()
	}
	t.Cleanup(func() {
		for _, c := range cancels {
			c()
		}
	})

	var wg sync.WaitGroup
	counts := make([]int, numSubs)
	for i := range subs {
		wg.Add(1)
		go func(idx int, ch <-chan engine.Event) {
			defer wg.Done()
			for range ch {
				counts[idx]++
			}
		}(i, subs[i])
	}

	for range numEvents {
		src <- engine.LogEntry{Message: "evt"}
	}
	close(src)
	wg.Wait()

	for i, c := range counts {
		assert.Equal(t, numEvents, c, "subscriber %d event count", i)
	}
}

// A slow subscriber must not block the bus. We measure that by confirming
// the producer returns promptly and that a concurrent fast reader keeps
// receiving events while the slow one is wedged.
func TestBus_SlowSubscriberDoesNotBlockBus(t *testing.T) {
	t.Parallel()

	src := make(chan engine.Event)
	bus := NewBus(src)

	fastCh, fastCancel := bus.Subscribe()
	slowCh, slowCancel := bus.Subscribe()
	t.Cleanup(fastCancel)
	t.Cleanup(slowCancel)

	var fastCount int
	done := make(chan struct{})
	go func() {
		for range fastCh {
			fastCount++
		}
		close(done)
	}()

	// slowCh is intentionally never drained — its buffer will fill and
	// subsequent dispatches to it will drop. That must not stall the bus.
	_ = slowCh

	const burst = 500
	start := time.Now()
	for range burst {
		src <- engine.LogEntry{Message: "evt"}
	}
	elapsed := time.Since(start)
	close(src)
	<-done

	assert.Less(t, elapsed, time.Second, "producer should not be blocked by slow subscriber")
	assert.Greater(t, fastCount, subscriberBuffer, "fast subscriber should outpace slow one's buffer cap")
	assert.LessOrEqual(t, fastCount, burst)
}

// A subscriber that never drains must cap at roughly subscriberBuffer
// events. We allow a small slack because bus.run's close-on-source-close
// finishes asynchronously relative to the test receiver.
func TestBus_NonDrainingSubscriberCapsNearBuffer(t *testing.T) {
	t.Parallel()

	src := make(chan engine.Event)
	bus := NewBus(src)
	ch, cancel := bus.Subscribe()
	t.Cleanup(cancel)

	const burst = subscriberBuffer * 4
	for range burst {
		src <- engine.LogEntry{Message: "evt"}
	}
	close(src)

	// Allow bus.run to finish dispatching and close the subscriber.
	time.Sleep(50 * time.Millisecond)

	var count int
	for range ch {
		count++
	}
	// Spec: dispatch is non-blocking; a non-draining subscriber never
	// pushes the bus into backpressure. 2× slack covers scheduling jitter.
	assert.LessOrEqual(t, count, subscriberBuffer*2,
		"non-draining subscriber received way more than its buffer (got %d)", count)
	assert.Less(t, count, burst,
		"non-draining subscriber should have dropped some events")
}

func TestBus_CancelUnregisters(t *testing.T) {
	t.Parallel()

	src := make(chan engine.Event)
	bus := NewBus(src)

	ch, cancel := bus.Subscribe()
	cancel()
	cancel() // idempotent

	select {
	case _, ok := <-ch:
		require.False(t, ok, "channel should be closed after cancel")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for closed channel")
	}

	ch2, cancel2 := bus.Subscribe()
	t.Cleanup(cancel2)

	done := make(chan struct{})
	go func() {
		evt := <-ch2
		assert.Equal(t, "hi", evt.(engine.LogEntry).Message)
		close(done)
	}()
	src <- engine.LogEntry{Message: "hi"}
	<-done
	close(src)
}

func TestBus_SourceCloseClosesSubscribers(t *testing.T) {
	t.Parallel()

	src := make(chan engine.Event)
	bus := NewBus(src)
	ch, _ := bus.Subscribe()

	close(src)

	select {
	case _, ok := <-ch:
		require.False(t, ok, "channel should be closed after source closes")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscriber close")
	}
}
