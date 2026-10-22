package engine_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
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

	t.Run("bastion resolver error fails service", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockProxies := setupMockProxy(ctrl)
		mockBastion := enginemocks.NewMockBastionResolver(ctrl)

		mockIPs.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil)
		mockBastion.EXPECT().Resolve(gomock.Any(), gomock.Eq(testConfig().Profiles["dev"])).Return("", errors.New("asg lookup failed"))

		eng := engine.New(testConfig(), mockDNS, mockSessions, mockBastion, mockIPs, mockProxies)
		require.NoError(t, eng.Start(ctx, []string{"dev"}))

		sc, found := waitForState(eng.Events(), state.Failed, 2*time.Second)
		require.True(t, found, "expected Failed event")
		assert.Contains(t, sc.Error.Error(), "asg lookup failed")
	})

	t.Run("session start failure", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockProxies := setupMockProxy(ctrl)

		mockIPs.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil)
		mockSessions.EXPECT().Start(gomock.Any(), gomock.Any()).Return(nil, errors.New("aws cli not found"))

		eng := engine.New(testConfig(), mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

		err := eng.Start(ctx, []string{"dev"})
		require.NoError(t, err)

		sc, found := waitForState(eng.Events(), state.Failed, 2*time.Second)
		require.True(t, found, "expected Failed event")
		assert.Contains(t, sc.Error.Error(), "aws cli not found")
	})

	t.Run("DNS add failure", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ctrl := gomock.NewController(t)

		mockDNS := enginemocks.NewMockDNSManager(ctrl)
		mockSessions := enginemocks.NewMockSessionManager(ctrl)
		mockIPs := enginemocks.NewMockIPAllocator(ctrl)
		mockProxies := setupMockProxy(ctrl)

		mockIPs.EXPECT().Allocate("dev/opensearch").Return(testIP, nil).AnyTimes()
		mockDNS.EXPECT().Add(gomock.Any(), gomock.Any()).Return(errors.New("permission denied"))

		eng := engine.New(testConfig(), mockDNS, mockSessions, setupMockBastion(ctrl), mockIPs, mockProxies)

		err := eng.Start(ctx, []string{"dev"})
		require.NoError(t, err)

		sc, found := waitForState(eng.Events(), state.Failed, 2*time.Second)
		require.True(t, found, "expected Failed event on DNS failure")
		assert.Contains(t, sc.Error.Error(), "permission denied")
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
