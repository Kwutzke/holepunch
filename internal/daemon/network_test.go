package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/Kwutzke/holepunch/internal/netwatch"
	"github.com/Kwutzke/holepunch/internal/resolver"
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

func (r *recordingUpstream) last() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sets) == 0 {
		return nil
	}
	return r.sets[len(r.sets)-1]
}

type recordingDropper struct {
	dropped        chan string
	upstream       *recordingUpstream
	upstreamAtDrop []string
}

func (r *recordingDropper) DropSessions(_ context.Context, reason string) {
	if r.upstream != nil {
		r.upstreamAtDrop = r.upstream.last()
	}
	r.dropped <- reason
}

func TestNetworkFollower_Follow(t *testing.T) {
	t.Parallel()

	t.Run("a network event re-reads resolv.conf, then drops the sessions", func(t *testing.T) {
		t.Parallel()
		resolvConf := writeResolvConf(t, filepath.Join(t.TempDir(), "resolv.conf"), "nameserver 192.168.68.1\n")
		upstream := &recordingUpstream{}
		sessions := &recordingDropper{dropped: make(chan string, 1), upstream: upstream}
		f := daemon.NewNetworkFollower(sessions, upstream, func() ([]string, error) { return resolver.DiscoverFromConfig(resolvConf, selfAddr) })
		f.RefreshUpstream()

		writeResolvConf(t, resolvConf, "nameserver 172.20.10.1\n")
		events := make(chan netwatch.Event, 1)
		ctx, cancel := context.WithCancel(t.Context())
		followed := make(chan struct{})
		go func() {
			f.Follow(ctx, events)
			close(followed)
		}()
		events <- netwatch.Event{Reason: "local addresses changed from [192.168.68.60] to [172.20.10.11]"}

		select {
		case reason := <-sessions.dropped:
			assert.Equal(t, "local addresses changed from [192.168.68.60] to [172.20.10.11]", reason)
		case <-time.After(2 * time.Second):
			t.Fatal("sessions were not dropped")
		}
		assert.Equal(t, [][]string{{"192.168.68.1:53"}, {"172.20.10.1:53"}}, upstream.all())
		assert.Equal(t, []string{"172.20.10.1:53"}, sessions.upstreamAtDrop, "upstreams are current before the sessions reconnect")

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
		f := daemon.NewNetworkFollower(&recordingDropper{}, upstream, func() ([]string, error) {
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
