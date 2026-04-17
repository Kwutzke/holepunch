package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Kwutzke/holepunch/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOsascript records calls to the notifier runFn. Used as a replacement
// for the real osascript shell-out in unit tests.
type fakeOsascript struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeOsascript) fn(_ context.Context, _, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, message)
	return nil
}

func (f *fakeOsascript) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newTestNotifier wires a Notifier to a given bus with an injected runFn.
// Parallels NewNotifier but lets tests observe calls without fork/exec.
func newTestNotifier(bus *Bus, run osascriptFunc) *Notifier {
	n := &Notifier{
		bus:      bus,
		runFn:    run,
		lastFire: make(map[string]time.Time),
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	go n.run(ctx)
	return n
}

func TestNotifier_FiresOnCredentialsExpired(t *testing.T) {
	t.Parallel()

	src := make(chan engine.Event)
	bus := NewBus(src)

	fake := &fakeOsascript{}
	n := newTestNotifier(bus, fake.fn)
	t.Cleanup(n.Stop)

	src <- engine.CredentialsExpired{
		Profile:    "dev",
		AWSProfile: "dev-sso",
	}

	require.Eventually(t, func() bool { return fake.count() == 1 },
		time.Second, 10*time.Millisecond)
	assert.Contains(t, fake.calls[0], "dev")
	assert.Contains(t, fake.calls[0], "dev-sso")
}

func TestNotifier_IgnoresOtherEvents(t *testing.T) {
	t.Parallel()

	src := make(chan engine.Event)
	bus := NewBus(src)

	fake := &fakeOsascript{}
	n := newTestNotifier(bus, fake.fn)
	t.Cleanup(n.Stop)

	src <- engine.LogEntry{Message: "noise"}
	src <- engine.ProfileDone{Profile: "dev"}

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, fake.count())
}

func TestNotifier_DedupesWithinWindow(t *testing.T) {
	t.Parallel()

	src := make(chan engine.Event)
	bus := NewBus(src)

	fake := &fakeOsascript{}
	n := newTestNotifier(bus, fake.fn)
	t.Cleanup(n.Stop)

	// Two services in the same profile expire at once.
	src <- engine.CredentialsExpired{Profile: "dev", AWSProfile: "dev-sso", ServiceName: "opensearch"}
	src <- engine.CredentialsExpired{Profile: "dev", AWSProfile: "dev-sso", ServiceName: "rds"}

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, fake.count(), "same (profile, aws-profile) should fire once within dedupe window")
}

func TestNotifier_DifferentProfilesNotDeduped(t *testing.T) {
	t.Parallel()

	src := make(chan engine.Event)
	bus := NewBus(src)

	fake := &fakeOsascript{}
	n := newTestNotifier(bus, fake.fn)
	t.Cleanup(n.Stop)

	src <- engine.CredentialsExpired{Profile: "dev", AWSProfile: "dev-sso"}
	src <- engine.CredentialsExpired{Profile: "prod", AWSProfile: "prod-sso"}

	require.Eventually(t, func() bool { return fake.count() == 2 },
		time.Second, 10*time.Millisecond)
}
