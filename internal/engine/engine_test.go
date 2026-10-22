package engine_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Kwutzke/holepunch/internal/config"
	"github.com/Kwutzke/holepunch/internal/dns"
	"github.com/Kwutzke/holepunch/internal/engine"
	enginemocks "github.com/Kwutzke/holepunch/internal/engine/mocks"
	"github.com/Kwutzke/holepunch/internal/session"
	sessionmocks "github.com/Kwutzke/holepunch/internal/session/mocks"
	"github.com/Kwutzke/holepunch/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

var testIP = net.IPv4(127, 0, 0, 2)

// startMocks holds the engine's start-phase dependencies for tests that fail one step on purpose.
type startMocks struct {
	ctrl     *gomock.Controller
	dns      *enginemocks.MockDNSManager
	sessions *enginemocks.MockSessionManager
	bastion  *enginemocks.MockBastionResolver
	ips      *enginemocks.MockIPAllocator
	proxies  *enginemocks.MockProxyFactory
}

func newStartMocks(ctrl *gomock.Controller) startMocks {
	m := startMocks{
		ctrl:     ctrl,
		dns:      enginemocks.NewMockDNSManager(ctrl),
		sessions: enginemocks.NewMockSessionManager(ctrl),
		bastion:  enginemocks.NewMockBastionResolver(ctrl),
		ips:      enginemocks.NewMockIPAllocator(ctrl),
		proxies:  enginemocks.NewMockProxyFactory(ctrl),
	}
	m.ips.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
	return m
}

// succeedAfterwards lets every start step succeed once the expectations set before it are used up,
// and hands out sess as the session.
func (m startMocks) succeedAfterwards(sess *sessionmocks.MockSession) {
	m.dns.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	m.bastion.EXPECT().Resolve(gomock.Any(), gomock.Any()).Return("i-0abc123", nil).AnyTimes()
	m.sessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(sess, nil)
	tcpProxy := enginemocks.NewMockTCPProxy(m.ctrl)
	tcpProxy.EXPECT().Stop().AnyTimes()
	m.proxies.EXPECT().NewProxy(gomock.Any(), gomock.Any()).Return(tcpProxy, nil)
}

// standIns runs an aws stand-in per session whose child never exits, as a dead
// session-manager-plugin does not, and records both PIDs of each.
type standIns struct {
	dir     string
	started atomic.Int32
}

func newStandIns(t *testing.T) *standIns {
	t.Helper()
	s := &standIns{dir: t.TempDir()}
	t.Cleanup(func() {
		for _, pid := range s.readPIDs() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return s
}

func (s *standIns) factory(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	pidFile := filepath.Join(s.dir, strconv.Itoa(int(s.started.Add(1)))+".pid")
	return exec.CommandContext(ctx, "sh", "-c", `sleep 300 & echo $$ $! > "$0"; wait`, pidFile)
}

// pids waits until the first n stand-ins have written their PIDs and returns all of them.
func (s *standIns) pids(t *testing.T, n int) []int {
	t.Helper()
	var pids []int
	require.Eventually(t, func() bool {
		pids = s.readPIDs()
		return len(pids) >= 2*n
	}, 5*time.Second, 10*time.Millisecond, "stand-ins never wrote their PIDs")
	return pids
}

func (s *standIns) readPIDs() []int {
	files, _ := filepath.Glob(filepath.Join(s.dir, "*.pid"))
	var pids []int
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil || !strings.HasSuffix(string(b), "\n") {
			continue
		}
		for _, field := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(field); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

func TestEngine_Start(t *testing.T) {
	t.Parallel()

	t.Run("starts and connects", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockSession := sessionmocks.NewMockSession(ctrl)
		mockProxies := setupMockProxy(ctrl)

		mockIPs.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
		mockDNS.EXPECT().Add(gomock.Any(), []dns.Entry{{IP: testIP, Hostname: "opensearch.dev"}}).Return(nil)
		mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(mockSession, nil)

		sessionDone := make(chan struct{})
		waited := awaitCalled(t)
		mockSession.EXPECT().Wait().DoAndReturn(func() error {
			close(waited)
			<-sessionDone
			return nil
		})

		eng := engine.New(testConfig(), mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

		err := eng.Start(ctx, []string{"dev"})
		require.NoError(t, err)

		sc, found := waitForState(eng.Events(), state.Connected, 2*time.Second)
		require.True(t, found, "expected Connected event")
		assert.Equal(t, "dev", sc.Profile)
		assert.Equal(t, "opensearch", sc.ServiceName)
		assert.Equal(t, "opensearch.dev", sc.DNSName)

		close(sessionDone)
	})

	t.Run("unknown profile", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		eng := engine.New(testConfig(),
			enginemocks.NewMockDNSManager(ctrl),
			enginemocks.NewMockSessionManager(ctrl),
			enginemocks.NewMockBastionResolver(ctrl),
			enginemocks.NewMockIPAllocator(ctrl),
			enginemocks.NewMockProxyFactory(ctrl),
		)

		err := eng.Start(ctx, []string{"nonexistent"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown profile")
	})

	t.Run("duplicate skipped", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockSession := sessionmocks.NewMockSession(ctrl)
		mockProxies := setupMockProxy(ctrl)

		mockIPs.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil)
		mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(mockSession, nil).Times(1)

		sessionDone := make(chan struct{})
		waited := awaitCalled(t)
		mockSession.EXPECT().Wait().DoAndReturn(func() error {
			close(waited)
			<-sessionDone
			return nil
		})
		mockSession.EXPECT().Stop(gomock.Any()).DoAndReturn(func(_ context.Context) error {
			close(sessionDone)
			return nil
		}).AnyTimes()

		eng := engine.New(testConfig(), mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

		require.NoError(t, eng.Start(ctx, []string{"dev"}))
		waitForState(eng.Events(), state.Connected, 2*time.Second)

		require.NoError(t, eng.Start(ctx, []string{"dev"}))
	})

	t.Run("multiple services", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockProxies := setupMockProxy(ctrl)

		ip2 := net.IPv4(127, 0, 0, 2)
		ip3 := net.IPv4(127, 0, 0, 3)
		mockIPs.EXPECT().Allocate("dev/opensearch").Return(ip2, nil).AnyTimes()
		mockIPs.EXPECT().Allocate("dev/rds").Return(ip3, nil).AnyTimes()
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).Times(2)

		sess1 := sessionmocks.NewMockSession(ctrl)
		sess2 := sessionmocks.NewMockSession(ctrl)
		done1 := make(chan struct{})
		done2 := make(chan struct{})
		waited1, waited2 := awaitCalled(t), awaitCalled(t)
		sess1.EXPECT().Wait().DoAndReturn(func() error { close(waited1); <-done1; return nil })
		sess2.EXPECT().Wait().DoAndReturn(func() error { close(waited2); <-done2; return nil })
		mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(sess1, nil)
		mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(sess2, nil)

		eng := engine.New(testConfigMultiService(), mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

		require.NoError(t, eng.Start(ctx, []string{"dev"}))

		events := eng.Events()
		connected := 0
		deadline := time.After(3 * time.Second)
		for connected < 2 {
			select {
			case evt := <-events:
				if sc, ok := evt.(engine.ServiceStateChanged); ok && sc.To == state.Connected {
					connected++
				}
			case <-deadline:
				t.Fatalf("timed out waiting for 2 Connected events, got %d", connected)
			}
		}

		statuses := eng.Status()
		assert.Len(t, statuses, 2)

		close(done1)
		close(done2)
	})

	t.Run("single service by target", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockProxies := setupMockProxy(ctrl)

		ip2 := net.IPv4(127, 0, 0, 2)
		// Only opensearch should be allocated — rds stays untouched.
		mockIPs.EXPECT().Allocate("dev/opensearch").Return(ip2, nil).AnyTimes()
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).Times(1)

		sess := sessionmocks.NewMockSession(ctrl)
		done := make(chan struct{})
		waited := awaitCalled(t)
		sess.EXPECT().Wait().DoAndReturn(func() error { close(waited); <-done; return nil })
		mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(sess, nil).Times(1)

		eng := engine.New(testConfigMultiService(), mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

		require.NoError(t, eng.Start(ctx, []string{"dev/opensearch"}))

		_, ok := waitForState(eng.Events(), state.Connected, 3*time.Second)
		require.True(t, ok, "opensearch should connect")

		// Only one instance exists — rds was not started.
		statuses := eng.Status()
		require.Len(t, statuses, 1)
		assert.Equal(t, "opensearch", statuses[0].ServiceName)

		close(done)
	})

	t.Run("unknown service", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockProxies := setupMockProxy(ctrl)

		eng := engine.New(testConfigMultiService(), mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

		err := eng.Start(ctx, []string{"dev/nonexistent"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown service")
	})
}

func TestEngine_runService(t *testing.T) {
	t.Parallel()

	t.Run("session drop reconnects", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockProxies := setupMockProxy(ctrl)

		mockIPs.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		firstSession := sessionmocks.NewMockSession(ctrl)
		firstSession.EXPECT().Wait().Return(errors.New("session terminated"))

		secondSession := sessionmocks.NewMockSession(ctrl)
		secondDone := make(chan struct{})
		secondWaited := awaitCalled(t)
		secondSession.EXPECT().Wait().DoAndReturn(func() error {
			close(secondWaited)
			<-secondDone
			return nil
		})

		gomock.InOrder(
			mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(firstSession, nil),
			mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(secondSession, nil),
		)

		eng := engine.New(testConfig(), mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

		err := eng.Start(ctx, []string{"dev"})
		require.NoError(t, err)

		events := eng.Events()

		sc, found := waitForState(events, state.Connected, 2*time.Second)
		require.True(t, found, "first Connected")
		assert.Equal(t, "opensearch", sc.ServiceName)

		sc, found = waitForState(events, state.Reconnecting, 2*time.Second)
		require.True(t, found, "Reconnecting after drop")

		sc, found = waitForState(events, state.Connected, 10*time.Second)
		require.True(t, found, "second Connected after reconnect")

		close(secondDone)
	})

	t.Run("target not connected on a pinned target takes the backoff", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockProxies := setupMockProxy(ctrl)
		mockBastion := enginemocks.NewMockBastionResolver(ctrl)

		mockIPs.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockBastion.EXPECT().Resolve(gomock.Any(), gomock.Any()).Return("i-0abc123", nil).Times(2)

		firstSession := sessionmocks.NewMockSession(ctrl)
		firstSession.EXPECT().Wait().Return(fmt.Errorf("%w: i-0abc123 is not connected", session.ErrTargetNotConnected))

		secondSession := sessionmocks.NewMockSession(ctrl)
		secondDone := make(chan struct{})
		secondWaited := awaitCalled(t)
		secondSession.EXPECT().Wait().DoAndReturn(func() error {
			close(secondWaited)
			<-secondDone
			return nil
		})

		gomock.InOrder(
			mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(firstSession, nil),
			mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(secondSession, nil),
		)

		eng := engine.New(testConfig(), mockDNS, mockSessions, mockBastion, mockIPs, mockProxies)
		require.NoError(t, eng.Start(ctx, []string{"dev"}))

		var logs []string
		connected := 0
		deadline := time.After(5 * time.Second)
		for connected < 2 {
			select {
			case evt := <-eng.Events():
				switch e := evt.(type) {
				case engine.LogEntry:
					logs = append(logs, e.Message)
				case engine.ServiceStateChanged:
					if e.To == state.Connected {
						connected++
					}
				}
			case <-deadline:
				t.Fatalf("reconnect did not complete; logs: %q", logs)
			}
		}
		close(secondDone)

		joined := strings.Join(logs, "\n")
		assert.Contains(t, joined, "reconnecting in 1s (attempt 1)", "a pinned target has nothing to re-resolve, so it backs off")
		assert.NotContains(t, joined, "ssm target rotated")
	})

	t.Run("target not connected invalidates and retries fast", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockProxies := setupMockProxy(ctrl)
		mockBastion := enginemocks.NewMockBastionResolver(ctrl)

		mockIPs.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		cfg := testConfig()
		devProfile := cfg.Profiles["dev"]
		devProfile.Target, devProfile.TargetASG = "", "bastion-asg"
		cfg.Profiles["dev"] = devProfile

		// First resolve returns the stale ID; Wait fails with
		// ErrTargetNotConnected; engine invalidates and re-resolves; second
		// resolve returns a fresh ID and the session stays up.
		gomock.InOrder(
			mockBastion.EXPECT().Resolve(gomock.Any(), gomock.Eq(devProfile)).Return("i-stale", nil),
			mockBastion.EXPECT().Invalidate(gomock.Eq(devProfile)),
			mockBastion.EXPECT().Resolve(gomock.Any(), gomock.Eq(devProfile)).Return("i-fresh", nil),
		)

		firstSession := sessionmocks.NewMockSession(ctrl)
		firstSession.EXPECT().Wait().Return(fmt.Errorf("%w: i-stale is not connected", session.ErrTargetNotConnected))

		secondSession := sessionmocks.NewMockSession(ctrl)
		secondDone := make(chan struct{})
		secondWaited := awaitCalled(t)
		secondSession.EXPECT().Wait().DoAndReturn(func() error {
			close(secondWaited)
			<-secondDone
			return nil
		})

		// Verify the engine passes the resolved target into StartParams.
		gomock.InOrder(
			mockSessions.EXPECT().Start(gomock.Any(), gomock.AssignableToTypeOf(session.StartParams{})).
				DoAndReturn(func(_ context.Context, p session.StartParams) (session.Session, error) {
					assert.Equal(t, "i-stale", p.Target)
					return firstSession, nil
				}),
			mockSessions.EXPECT().Start(gomock.Any(), gomock.AssignableToTypeOf(session.StartParams{})).
				DoAndReturn(func(_ context.Context, p session.StartParams) (session.Session, error) {
					assert.Equal(t, "i-fresh", p.Target)
					return secondSession, nil
				}),
		)

		eng := engine.New(cfg, mockDNS, mockSessions, mockBastion, mockIPs, mockProxies)

		require.NoError(t, eng.Start(ctx, []string{"dev"}))

		events := eng.Events()

		_, found := waitForState(events, state.Connected, 2*time.Second)
		require.True(t, found, "first Connected")

		_, found = waitForState(events, state.Reconnecting, 2*time.Second)
		require.True(t, found, "Reconnecting after target-not-connected")

		_, found = waitForState(events, state.Connected, 5*time.Second)
		require.True(t, found, "second Connected after fresh resolve")

		close(secondDone)
	})

	t.Run("a transient start failure retries on the backoff until connected", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			failing func(m startMocks, err error)
		}{
			{"DNS add", func(m startMocks, err error) {
				m.dns.EXPECT().Add(gomock.Any(), gomock.Any()).Return(err).Times(2)
			}},
			{"bastion lookup", func(m startMocks, err error) {
				m.bastion.EXPECT().Resolve(gomock.Any(), gomock.Any()).Return("", err).Times(2)
			}},
			{"session start", func(m startMocks, err error) {
				m.sessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(nil, err).Times(2)
			}},
			{"proxy listen", func(m startMocks, err error) {
				failedSession := sessionmocks.NewMockSession(m.ctrl)
				failedSession.EXPECT().Stop(gomock.Any()).Return(nil).Times(2)
				m.sessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(failedSession, nil).Times(2)
				m.proxies.EXPECT().NewProxy(gomock.Any(), gomock.Any()).Return(nil, err).Times(2)
			}},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				ctrl := gomock.NewController(t)

				startErr := errors.New(tt.name + " unavailable")
				m := newStartMocks(ctrl)
				tt.failing(m, startErr)

				sessionDone := make(chan struct{})
				waited := awaitCalled(t)
				sess := sessionmocks.NewMockSession(ctrl)
				sess.EXPECT().Wait().DoAndReturn(func() error {
					close(waited)
					<-sessionDone
					return nil
				})
				m.succeedAfterwards(sess)

				eng := engine.New(testConfig(), m.dns, m.sessions, m.bastion, m.ips, m.proxies)
				require.NoError(t, eng.Start(t.Context(), []string{"dev"}))

				changes := collectStatesUntil(t, eng.Events(), state.Connected, 10*time.Second)
				close(sessionDone)

				var retries []engine.ServiceStateChanged
				for _, sc := range changes {
					assert.NotEqual(t, state.Failed, sc.To, "a transient start failure must not fail the service")
					if sc.From == state.Starting && sc.To == state.Reconnecting {
						retries = append(retries, sc)
					}
				}
				require.Len(t, retries, 2, "one retry per failed start")
				for _, sc := range retries {
					assert.ErrorIs(t, sc.Error, startErr)
				}
			})
		}
	})

	t.Run("the backoff is capped at max_backoff_seconds", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)

		m := newStartMocks(ctrl)
		m.dns.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		m.bastion.EXPECT().Resolve(gomock.Any(), gomock.Any()).Return("", errors.New("asg lookup failed")).AnyTimes()

		cfg := testConfig()
		cfg.Defaults.Reconnect.MaxBackoffSeconds = 1
		eng := engine.New(cfg, m.dns, m.sessions, m.bastion, m.ips, m.proxies)
		require.NoError(t, eng.Start(t.Context(), []string{"dev"}))

		var delays []string
		deadline := time.After(5 * time.Second)
		for len(delays) < 2 {
			select {
			case evt := <-eng.Events():
				if le, ok := evt.(engine.LogEntry); ok && strings.HasPrefix(le.Message, "reconnecting in") {
					delays = append(delays, le.Message)
				}
			case <-deadline:
				t.Fatalf("saw %q before the deadline", delays)
			}
		}
		assert.Equal(t, []string{"reconnecting in 1s (attempt 1)", "reconnecting in 1s (attempt 2)"}, delays, "uncapped, the second delay is 2s")
	})

	t.Run("down while retrying a failed start ends in Disconnected", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)

		m := newStartMocks(ctrl)
		m.bastion.EXPECT().Resolve(gomock.Any(), gomock.Any()).Return("", errors.New("asg lookup failed")).AnyTimes()
		m.dns.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		m.dns.EXPECT().RemoveEntries(gomock.Any(), gomock.Any()).Return(nil)
		m.ips.EXPECT().Release("dev/opensearch")

		eng := engine.New(testConfig(), m.dns, m.sessions, m.bastion, m.ips, m.proxies)
		require.NoError(t, eng.Start(t.Context(), []string{"dev"}))
		_, found := waitForState(eng.Events(), state.Reconnecting, 2*time.Second)
		require.True(t, found, "expected the failed start to retry")

		require.NoError(t, eng.Stop(t.Context(), []string{"dev"}))

		sc, found := waitForState(eng.Events(), state.Disconnected, 2*time.Second)
		require.True(t, found, "expected Disconnected after down")
		assert.Equal(t, state.Stopping, sc.From)
	})

	t.Run("down during a start ends in Disconnected", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)

		m := newStartMocks(ctrl)
		resolving := awaitCalled(t)
		m.bastion.EXPECT().Resolve(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, _ config.Profile) (string, error) {
			close(resolving)
			<-ctx.Done()
			return "", ctx.Err()
		})
		m.dns.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil)
		m.dns.EXPECT().RemoveEntries(gomock.Any(), gomock.Any()).Return(nil)
		m.ips.EXPECT().Release("dev/opensearch")

		eng := engine.New(testConfig(), m.dns, m.sessions, m.bastion, m.ips, m.proxies)
		require.NoError(t, eng.Start(t.Context(), []string{"dev"}))
		<-resolving

		require.NoError(t, eng.Stop(t.Context(), []string{"dev"}))

		sc, found := waitForState(eng.Events(), state.Disconnected, 2*time.Second)
		require.True(t, found, "expected Disconnected after down")
		assert.Equal(t, state.Stopping, sc.From)
	})

	t.Run("credentials expired halts reconnect", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockProxies := setupMockProxy(ctrl)

		mockIPs.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		expiredErr := fmt.Errorf("%w: The SSO session associated with this profile has expired", session.ErrCredentialsExpired)

		sess := sessionmocks.NewMockSession(ctrl)
		sess.EXPECT().Wait().Return(expiredErr)
		// Assert Start is called exactly once — halt must prevent a second attempt.
		mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(sess, nil).Times(1)

		eng := engine.New(testConfig(), mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

		require.NoError(t, eng.Start(ctx, []string{"dev"}))

		events := eng.Events()

		sc, found := waitForState(events, state.Failed, 3*time.Second)
		require.True(t, found, "expected Failed state after expired credentials")
		assert.Equal(t, "dev", sc.Profile)
		assert.Equal(t, "opensearch", sc.ServiceName)
		assert.True(t, errors.Is(sc.Error, session.ErrCredentialsExpired))

		var gotCredsEvent bool
		deadline := time.After(500 * time.Millisecond)
	collect:
		for {
			select {
			case evt := <-events:
				if ce, ok := evt.(engine.CredentialsExpired); ok {
					assert.Equal(t, "dev", ce.Profile)
					assert.Equal(t, "dev-sso", ce.AWSProfile)
					assert.Equal(t, "opensearch", ce.ServiceName)
					assert.NotEmpty(t, ce.Detail)
					gotCredsEvent = true
				}
			case <-deadline:
				break collect
			}
		}
		assert.True(t, gotCredsEvent, "expected CredentialsExpired event")

		// Confirm no retry occurred: status still Failed after a grace period
		// (shorter than the reconnect backoff InitialDelay of 1s — if the
		// goroutine didn't halt we'd see Starting again).
		time.Sleep(1500 * time.Millisecond)
		statuses := eng.Status()
		require.Len(t, statuses, 1)
		assert.Equal(t, state.Failed, statuses[0].State)
	})
}

func TestEngine_Stop(t *testing.T) {
	t.Parallel()

	t.Run("stops services", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockSession := sessionmocks.NewMockSession(ctrl)
		mockProxies := setupMockProxy(ctrl)

		mockIPs.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
		mockIPs.EXPECT().Release("dev/opensearch")
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil)
		mockDNS.EXPECT().RemoveEntries(gomock.Any(), gomock.Any()).Return(nil)
		mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(mockSession, nil)

		sessionDone := make(chan struct{})
		waited := awaitCalled(t)
		mockSession.EXPECT().Wait().DoAndReturn(func() error {
			close(waited)
			<-sessionDone
			return nil
		})
		mockSession.EXPECT().Stop(gomock.Any()).DoAndReturn(func(_ context.Context) error {
			close(sessionDone)
			return nil
		})

		eng := engine.New(testConfig(), mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

		require.NoError(t, eng.Start(ctx, []string{"dev"}))
		waitForState(eng.Events(), state.Connected, 2*time.Second)

		err := eng.Stop(ctx, []string{"dev"})
		require.NoError(t, err)

		sc, found := waitForState(eng.Events(), state.Disconnected, 2*time.Second)
		require.True(t, found, "expected Disconnected after stop")
		assert.Equal(t, "opensearch", sc.ServiceName)
	})

	t.Run("stops only the named profile", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		cfg := config.Config{
			Profiles: map[string]config.Profile{
				"dev": {
					AWSProfile: "dev", AWSRegion: "eu-west-1", Target: "i-dev",
					Services: []config.Service{{Name: "svc", DNSName: "svc.dev", RemoteHost: "h", RemotePort: 443, LocalPort: 443}},
				},
				"prod": {
					AWSProfile: "prod", AWSRegion: "eu-west-1", Target: "i-prod",
					Services: []config.Service{{Name: "svc", DNSName: "svc.prod", RemoteHost: "h", RemotePort: 443, LocalPort: 443}},
				},
			},
		}

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockProxies := setupMockProxy(ctrl)

		ip2 := net.IPv4(127, 0, 0, 2)
		ip3 := net.IPv4(127, 0, 0, 3)
		mockIPs.EXPECT().Allocate("dev/svc").Return(ip2, nil).AnyTimes()
		mockIPs.EXPECT().Allocate("prod/svc").Return(ip3, nil).AnyTimes()
		mockIPs.EXPECT().Release("dev/svc")
		mockIPs.EXPECT().Release("prod/svc")
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockDNS.EXPECT().RemoveEntries(gomock.Any(), gomock.Any()).Return(nil).Times(2)

		devSess := sessionmocks.NewMockSession(ctrl)
		prodSess := sessionmocks.NewMockSession(ctrl)
		devDone := make(chan struct{})
		prodDone := make(chan struct{})
		devWaited, prodWaited := awaitCalled(t), awaitCalled(t)
		devSess.EXPECT().Wait().DoAndReturn(func() error { close(devWaited); <-devDone; return nil })
		prodSess.EXPECT().Wait().DoAndReturn(func() error { close(prodWaited); <-prodDone; return nil })
		devSess.EXPECT().Stop(gomock.Any()).DoAndReturn(func(_ context.Context) error { close(devDone); return nil })
		prodSess.EXPECT().Stop(gomock.Any()).DoAndReturn(func(_ context.Context) error { close(prodDone); return nil })

		mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(devSess, nil)
		mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(prodSess, nil)

		eng := engine.New(cfg, mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

		require.NoError(t, eng.Start(ctx, []string{"dev", "prod"}))

		events := eng.Events()
		connected := 0
		deadline := time.After(3 * time.Second)
		for connected < 2 {
			select {
			case evt := <-events:
				if sc, ok := evt.(engine.ServiceStateChanged); ok && sc.To == state.Connected {
					connected++
				}
			case <-deadline:
				t.Fatalf("timed out, got %d connected", connected)
			}
		}

		// Stop only dev.
		require.NoError(t, eng.Stop(ctx, []string{"dev"}))

		// Prod should still be running.
		statuses := eng.Status()
		require.Len(t, statuses, 1)
		assert.Equal(t, "prod", statuses[0].Profile)

		// Clean up prod.
		require.NoError(t, eng.Stop(ctx, []string{"prod"}))
	})
}

func TestEngine_DropSessions(t *testing.T) {
	t.Parallel()

	t.Run("every Connected service reconnects on a new session and its old process group is gone", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)
		standIns := newStandIns(t)

		m := newStartMocks(ctrl)
		m.dns.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		m.dns.EXPECT().RemoveEntries(gomock.Any(), gomock.Any()).Return(nil)
		m.ips.EXPECT().Allocate(gomock.Any()).Return(testIP, nil).AnyTimes()
		m.ips.EXPECT().Release(gomock.Any()).AnyTimes()

		eng := engine.New(testConfigMultiService(), m.dns, session.NewSSMManager(standIns.factory), setupMockBastion(ctrl), m.ips, setupMockProxy(ctrl))
		require.NoError(t, eng.Start(ctx, []string{"dev"}))
		collectConnected(t, eng.Events(), 2, 5*time.Second)
		oldGroups := standIns.pids(t, 2)

		eng.DropSessions(ctx, "test network event")

		collectConnected(t, eng.Events(), 2, 10*time.Second)
		assert.EqualValues(t, 4, standIns.started.Load(), "a second session per service")
		for _, pid := range oldGroups {
			assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "process %d of a dropped session is alive", pid)
		}
		for _, st := range eng.Status() {
			assert.Equal(t, state.Connected, st.State, st.ServiceName)
		}

		require.NoError(t, eng.Stop(ctx, nil))
	})

	t.Run("journey: after a sleep with two failing lookups every service reconnects, and down leaves no stand-in", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)
		standIns := newStandIns(t)

		m := newStartMocks(ctrl)
		m.dns.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		m.dns.EXPECT().RemoveEntries(gomock.Any(), gomock.Any()).Return(nil)
		m.ips.EXPECT().Allocate(gomock.Any()).Return(testIP, nil).AnyTimes()
		m.ips.EXPECT().Release(gomock.Any()).AnyTimes()
		lookupErr := errors.New("no network yet")
		m.bastion.EXPECT().Resolve(gomock.Any(), gomock.Any()).Return("i-0abc123", nil).Times(2)
		m.bastion.EXPECT().Resolve(gomock.Any(), gomock.Any()).Return("", lookupErr).Times(2)
		m.bastion.EXPECT().Resolve(gomock.Any(), gomock.Any()).Return("i-0abc123", nil).Times(2)

		eng := engine.New(testConfigMultiService(), m.dns, session.NewSSMManager(standIns.factory), m.bastion, m.ips, setupMockProxy(ctrl))
		require.NoError(t, eng.Start(ctx, []string{"dev"}))
		collectConnected(t, eng.Events(), 2, 5*time.Second)
		oldGroups := standIns.pids(t, 2)

		eng.DropSessions(ctx, "host resumed from sleep (10m0s since the last check)")
		for _, pid := range oldGroups {
			assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "process %d of a dropped session is alive", pid)
		}

		changes := collectConnected(t, eng.Events(), 2, 15*time.Second)
		var failedStarts int
		for _, sc := range changes {
			if sc.From == state.Starting && sc.To == state.Reconnecting {
				assert.ErrorIs(t, sc.Error, lookupErr)
				failedStarts++
			}
		}
		assert.Equal(t, 2, failedStarts, "both failing lookups retried")
		assert.EqualValues(t, 4, standIns.started.Load(), "the failed lookups started no session")

		allGroups := standIns.pids(t, 4)
		require.NoError(t, eng.Stop(ctx, nil))
		for _, pid := range allGroups {
			assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "stand-in process %d outlived down", pid)
		}
	})
}

func TestEngine_Status(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ctrl := gomock.NewController(t)

	mockDNS := enginemocks.NewMockDNSManager(ctrl)
	mockSessions := enginemocks.NewMockSessionManager(ctrl)
	mockIPs := enginemocks.NewMockIPAllocator(ctrl)
	mockSession := sessionmocks.NewMockSession(ctrl)
	mockProxies := setupMockProxy(ctrl)

	mockIPs.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
	mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil)
	mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(mockSession, nil)

	sessionDone := make(chan struct{})
	waited := awaitCalled(t)
	mockSession.EXPECT().Wait().DoAndReturn(func() error {
		close(waited)
		<-sessionDone
		return nil
	})

	eng := engine.New(testConfig(), mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

	require.NoError(t, eng.Start(ctx, []string{"dev"}))
	waitForState(eng.Events(), state.Connected, 2*time.Second)

	statuses := eng.Status()
	require.Len(t, statuses, 1)
	assert.Equal(t, "dev", statuses[0].Profile)
	assert.Equal(t, "opensearch", statuses[0].ServiceName)
	assert.Equal(t, "opensearch.dev", statuses[0].DNSName)
	assert.Equal(t, "127.0.0.2:443", statuses[0].LocalAddr)
	assert.Equal(t, state.Connected, statuses[0].State)

	close(sessionDone)
}

func testConfig() config.Config {
	return config.Config{
		Profiles: map[string]config.Profile{
			"dev": {
				AWSProfile: "dev-sso",
				AWSRegion:  "eu-central-1",
				Target:     "i-0abc123",
				Services: []config.Service{
					{
						Name:       "opensearch",
						DNSName:    "opensearch.dev",
						RemoteHost: "vpc-os.es.amazonaws.com",
						RemotePort: 443,
						LocalPort:  443,
					},
				},
			},
		},
	}
}

func testConfigMultiService() config.Config {
	return config.Config{
		Profiles: map[string]config.Profile{
			"dev": {
				AWSProfile: "dev-sso",
				AWSRegion:  "eu-central-1",
				Target:     "i-0abc123",
				Services: []config.Service{
					{
						Name:       "opensearch",
						DNSName:    "opensearch.dev",
						RemoteHost: "vpc-os.es.amazonaws.com",
						RemotePort: 443,
						LocalPort:  443,
					},
					{
						Name:       "rds",
						DNSName:    "rds.dev",
						RemoteHost: "mydb.rds.amazonaws.com",
						RemotePort: 5432,
						LocalPort:  5432,
					},
				},
			},
		},
	}
}

// collectConnected returns the state changes up to the n-th move to Connected.
func collectConnected(t *testing.T, ch <-chan engine.Event, n int, timeout time.Duration) []engine.ServiceStateChanged {
	t.Helper()
	var changes []engine.ServiceStateChanged
	for range n {
		changes = append(changes, collectStatesUntil(t, ch, state.Connected, timeout)...)
	}
	return changes
}

// collectStatesUntil returns every state change up to and including the first one to target.
func collectStatesUntil(t *testing.T, ch <-chan engine.Event, target state.ServiceState, timeout time.Duration) []engine.ServiceStateChanged {
	t.Helper()
	var changes []engine.ServiceStateChanged
	deadline := time.After(timeout)
	for {
		select {
		case evt := <-ch:
			sc, ok := evt.(engine.ServiceStateChanged)
			if !ok {
				continue
			}
			changes = append(changes, sc)
			if sc.To == target {
				return changes
			}
			if sc.To == state.Failed {
				t.Fatalf("service failed before reaching %v: %v", target, sc.Error)
			}
		case <-deadline:
			t.Fatalf("no transition to %v within %v; saw %v", target, timeout, changes)
			return nil
		}
	}
}

func waitForState(ch <-chan engine.Event, targetState state.ServiceState, timeout time.Duration) (engine.ServiceStateChanged, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case evt := <-ch:
			if sc, ok := evt.(engine.ServiceStateChanged); ok && sc.To == targetState {
				return sc, true
			}
		case <-deadline:
			return engine.ServiceStateChanged{}, false
		}
	}
}

// setupMockBastion returns a MockBastionResolver that returns profile.Target
// for every Resolve call and ignores Invalidate. Suitable for tests that
// don't exercise the target-not-connected path.
func setupMockBastion(ctrl *gomock.Controller) *enginemocks.MockBastionResolver {
	m := enginemocks.NewMockBastionResolver(ctrl)
	m.EXPECT().Resolve(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, p config.Profile) (string, error) { return p.Target, nil },
	).AnyTimes()
	m.EXPECT().Invalidate(gomock.Any()).AnyTimes()
	return m
}

// setupMockProxy sets up a MockProxyFactory that always succeeds and returns a no-op MockTCPProxy.
func setupMockProxy(ctrl *gomock.Controller) *enginemocks.MockProxyFactory {
	mockProxyFactory := enginemocks.NewMockProxyFactory(ctrl)
	mockTCPProxy := enginemocks.NewMockTCPProxy(ctrl)
	mockTCPProxy.EXPECT().Stop().AnyTimes()
	mockTCPProxy.EXPECT().ListenAddr().Return("127.0.0.2:443").AnyTimes()
	mockProxyFactory.EXPECT().NewProxy(gomock.Any(), gomock.Any()).Return(mockTCPProxy, nil).AnyTimes()
	return mockProxyFactory
}

// awaitCalled returns a channel a mock closes when it runs. The cleanup blocks until then, and runs
// before gomock's missing-call check because the controller registered its cleanup first.
func awaitCalled(t *testing.T) chan struct{} {
	t.Helper()
	called := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-called:
		case <-time.After(2 * time.Second):
			t.Error("mock was not called within 2s")
		}
	})
	return called
}
