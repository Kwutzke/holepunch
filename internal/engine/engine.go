package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Kwutzke/holepunch/internal/config"
	"github.com/Kwutzke/holepunch/internal/dns"
	"github.com/Kwutzke/holepunch/internal/reconnect"
	"github.com/Kwutzke/holepunch/internal/session"
	"github.com/Kwutzke/holepunch/internal/state"
)

// targetRotationRetryDelay replaces the exponential backoff after an ASG target rotated out: a
// rotation should cost one short pause, but a whole ASG mid-replacement must not spin Resolve
// against AWS. A pinned target cannot rotate, so it keeps the backoff.
const targetRotationRetryDelay = 500 * time.Millisecond

// ServiceStatus represents the current status of a single service.
type ServiceStatus struct {
	Profile     string
	ServiceName string
	DNSName     string
	LocalAddr   string // e.g. "127.0.0.5:443"
	State       state.ServiceState
	ConnectedAt time.Time
	Error       error
}

// Engine orchestrates port-forwarding sessions for all profiles.
type Engine struct {
	cfg       config.Config
	dns       DNSManager
	sessions  SessionManager
	bastion   BastionResolver
	ips       IPAllocator
	proxies   ProxyFactory
	events    chan Event
	mu        sync.RWMutex
	instances map[string]*serviceInstance // key: "profile/service"
}

// New creates a new Engine.
func New(cfg config.Config, dnsMgr DNSManager, sessMgr SessionManager, bastionResolver BastionResolver, ipAlloc IPAllocator, proxyFactory ProxyFactory) *Engine {
	return &Engine{
		cfg:       cfg,
		dns:       dnsMgr,
		sessions:  sessMgr,
		bastion:   bastionResolver,
		ips:       ipAlloc,
		proxies:   proxyFactory,
		events:    make(chan Event, 100),
		instances: make(map[string]*serviceInstance),
	}
}

// Events returns the event channel; events are dropped, not queued, once its 100-slot buffer is full.
func (e *Engine) Events() <-chan Event {
	return e.events
}

// Config returns a snapshot of the engine's config. Used by the daemon's
// status handler to merge configured-but-not-running services into the
// response so UIs can render a complete service inventory.
func (e *Engine) Config() config.Config {
	return e.cfg
}

// Start begins port-forwarding for the specified selectors. Each selector
// is either "profile" (all services) or "profile/service" (just that one).
// It is additive — calling Start with new selectors adds them alongside
// existing ones.
func (e *Engine) Start(ctx context.Context, selectors []string) error {
	targets, err := e.resolveTargets(selectors)
	if err != nil {
		return err
	}
	for _, t := range targets {
		profile := e.cfg.Profiles[t.profile]
		key := t.profile + "/" + t.service.Name

		e.mu.RLock()
		_, exists := e.instances[key]
		e.mu.RUnlock()
		if exists {
			e.emitLog("warn", "already running, skipping", t.profile, t.service.Name)
			continue
		}

		ip, err := e.ips.Allocate(key)
		if err != nil {
			return fmt.Errorf("allocating IP for %s: %w", key, err)
		}

		si := &serviceInstance{
			profile: t.profile,
			service: t.service,
			state:   state.Disconnected,
			backoff: e.newBackoff(),
			localIP: ip.String(),
		}

		e.mu.Lock()
		e.instances[key] = si
		e.mu.Unlock()

		svcCtx, cancel := context.WithCancel(ctx)
		si.cancel = cancel

		go e.runService(svcCtx, si, profile)
	}
	return nil
}

// Stop stops port-forwarding for the specified selectors. Each selector
// is either "profile" (all services in profile) or "profile/service"
// (just that one). If no selectors are given, all running services are
// stopped.
func (e *Engine) Stop(ctx context.Context, selectors []string) error {
	// Build the set of keys to stop. For an empty selector list, all
	// running instances are targeted.
	wantKey := make(map[string]bool)
	if len(selectors) > 0 {
		targets, err := e.resolveTargets(selectors)
		if err != nil {
			return err
		}
		for _, t := range targets {
			wantKey[t.profile+"/"+t.service.Name] = true
		}
	}

	e.mu.RLock()
	toStop := make([]*serviceInstance, 0)
	for key, si := range e.instances {
		if len(selectors) == 0 || wantKey[key] {
			toStop = append(toStop, si)
		}
	}
	e.mu.RUnlock()

	var wg sync.WaitGroup
	for _, si := range toStop {
		wg.Add(1)
		go func(inst *serviceInstance) {
			defer wg.Done()
			e.stopService(ctx, inst)
		}(si)
	}
	wg.Wait()

	// Clean up DNS entries for stopped services.
	var entriesToRemove []dns.Entry
	for _, si := range toStop {
		key := si.profile + "/" + si.service.Name
		ip, _ := e.ips.Allocate(key) // get existing allocation
		entriesToRemove = append(entriesToRemove, dns.Entry{
			IP:       ip,
			Hostname: si.service.DNSName,
		})
		e.ips.Release(key)

		e.mu.Lock()
		delete(e.instances, key)
		e.mu.Unlock()
	}

	if len(entriesToRemove) > 0 {
		if err := e.dns.RemoveEntries(ctx, entriesToRemove); err != nil {
			e.emitLog("error", fmt.Sprintf("failed to remove DNS entries: %v", err), "", "")
		}
	}

	// Emit ProfileDone for each profile whose last instance just went away.
	doneProfiles := make(map[string]bool)
	for _, si := range toStop {
		doneProfiles[si.profile] = true
	}
	e.mu.RLock()
	for _, si := range e.instances {
		delete(doneProfiles, si.profile)
	}
	e.mu.RUnlock()
	for p := range doneProfiles {
		e.emit(ProfileDone{Profile: p, Timestamp: time.Now()})
	}

	return nil
}

// DropSessions stops the session of every Connected service without stopping the service, so each
// reconnects through a new session. It returns once every dropped session has ended.
func (e *Engine) DropSessions(ctx context.Context, reason string) {
	e.mu.RLock()
	instances := make([]*serviceInstance, 0, len(e.instances))
	for _, si := range e.instances {
		instances = append(instances, si)
	}
	e.mu.RUnlock()

	var wg sync.WaitGroup
	dropped := 0
	for _, si := range instances {
		si.mu.Lock()
		sess := si.session
		connected := si.state == state.Connected
		si.mu.Unlock()
		if !connected || sess == nil {
			continue
		}
		dropped++
		wg.Go(func() {
			stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := sess.Stop(stopCtx); err != nil {
				e.emitLog("warn", fmt.Sprintf("error stopping session: %v", err), si.profile, si.service.Name)
			}
		})
	}
	wg.Wait()
	e.emitLog("info", fmt.Sprintf("%s: dropped %d sessions to reconnect", reason, dropped), "", "")
}

// Status returns the current status of all running services.
func (e *Engine) Status() []ServiceStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()

	statuses := make([]ServiceStatus, 0, len(e.instances))
	for _, si := range e.instances {
		si.mu.Lock()
		statuses = append(statuses, ServiceStatus{
			Profile:     si.profile,
			ServiceName: si.service.Name,
			DNSName:     si.service.DNSName,
			LocalAddr:   fmt.Sprintf("%s:%d", si.localIP, si.service.LocalPort),
			State:       si.state,
			ConnectedAt: si.connectedAt,
			Error:       si.lastError,
		})
		si.mu.Unlock()
	}
	return statuses
}

func (e *Engine) newBackoff() *reconnect.Backoff {
	b := reconnect.NewBackoff()
	if secs := e.cfg.Defaults.Reconnect.MaxBackoffSeconds; secs > 0 {
		b.MaxDelay = time.Duration(secs) * time.Second
	}
	return b
}

// resolveTargets expands selector strings into concrete configured
// services. Each selector is either "profile" (expands to every service
// in the profile) or "profile/service" (a single service). Returns an
// error for unknown profiles or services.
func (e *Engine) resolveTargets(selectors []string) ([]target, error) {
	var out []target
	for _, sel := range selectors {
		profileName, serviceName, hasService := strings.Cut(sel, "/")
		profile, ok := e.cfg.Profiles[profileName]
		if !ok {
			return nil, fmt.Errorf("unknown profile: %q", profileName)
		}
		if !hasService {
			for _, svc := range profile.Services {
				out = append(out, target{profile: profileName, service: svc})
			}
			continue
		}
		var match *config.Service
		for i := range profile.Services {
			if profile.Services[i].Name == serviceName {
				match = &profile.Services[i]
				break
			}
		}
		if match == nil {
			return nil, fmt.Errorf("unknown service: %q", sel)
		}
		out = append(out, target{profile: profileName, service: *match})
	}
	return out, nil
}

func (e *Engine) runService(ctx context.Context, si *serviceInstance, profile config.Profile) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		from := si.getState()
		if err := si.transition(state.Starting); err != nil {
			e.emitLog("error", fmt.Sprintf("invalid transition to Starting from %v: %v", from, err), si.profile, si.service.Name)
			return
		}
		e.emitStateChange(si, from, state.Starting, nil)

		sess, err := e.connect(ctx, si, profile)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if e.credentialsExpired(si, state.Starting, session.ClassifyCredentials(err), profile) {
				return
			}
			if !e.reconnecting(si, state.Starting, err) {
				return
			}
			e.emitLog("warn", fmt.Sprintf("start failed: %v", err), si.profile, si.service.Name)
		} else {
			waitErr := sess.Wait()
			if ctx.Err() != nil {
				return
			}

			// proxy.Stop waits out in-flight connections, seconds for long-lived clients; under si.mu
			// that wait would block Status and stopService on this instance.
			si.mu.Lock()
			proxy := si.tcpProxy
			si.tcpProxy = nil
			si.session = nil
			si.mu.Unlock()
			if proxy != nil {
				proxy.Stop()
			}

			if !e.reconnecting(si, state.Connected, waitErr) {
				return
			}

			if e.credentialsExpired(si, state.Reconnecting, waitErr, profile) {
				return
			}

			if errors.Is(waitErr, session.ErrTargetNotConnected) && profile.TargetASG != "" {
				e.bastion.Invalidate(profile)
				e.emitLog("warn", fmt.Sprintf("ssm target rotated out of %s — re-resolving (%v)", profile.TargetASG, waitErr), si.profile, si.service.Name)
				select {
				case <-ctx.Done():
					return
				case <-time.After(targetRotationRetryDelay):
				}
				continue
			}
		}

		if si.backoff.ShouldReset() {
			si.backoff.Reset()
		}

		delay := si.backoff.NextDelay()
		e.emitLog("info", fmt.Sprintf("reconnecting in %v (attempt %d)", delay, si.backoff.Attempt()), si.profile, si.service.Name)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// connect runs every start step and leaves the service Connected with its session and proxy stored;
// on error the service is still Starting and holds neither.
func (e *Engine) connect(ctx context.Context, si *serviceInstance, profile config.Profile) (session.Session, error) {
	ip, _ := e.ips.Allocate(si.profile + "/" + si.service.Name)
	if err := e.dns.Add(ctx, []dns.Entry{{IP: ip, Hostname: si.service.DNSName}}); err != nil {
		return nil, fmt.Errorf("adding DNS entry: %w", err)
	}

	ssmPort, err := freePort()
	if err != nil {
		return nil, err
	}

	target, err := e.bastion.Resolve(ctx, profile)
	if err != nil {
		return nil, fmt.Errorf("resolving SSM target: %w", err)
	}

	sess, err := e.sessions.Start(ctx, session.StartParams{
		AWSProfile: profile.AWSProfile,
		AWSRegion:  profile.AWSRegion,
		Target:     target,
		RemoteHost: si.service.RemoteHost,
		RemotePort: si.service.RemotePort,
		LocalIP:    net.IPv4(127, 0, 0, 1),
		LocalPort:  ssmPort,
	})
	if err != nil {
		return nil, err
	}
	var tcpProxy TCPProxy
	committed := false
	defer func() {
		if committed {
			return
		}
		if tcpProxy != nil {
			tcpProxy.Stop()
		}
		// ctx may be cancelled already; the session still has to be stopped.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if stopErr := sess.Stop(stopCtx); stopErr != nil {
			e.emitLog("warn", fmt.Sprintf("error stopping session: %v", stopErr), si.profile, si.service.Name)
		}
	}()

	tcpProxy, err = e.proxies.NewProxy(ctx, ProxyConfig{
		ListenAddr:   fmt.Sprintf("%s:%d", ip.String(), si.service.LocalPort),
		TargetAddr:   fmt.Sprintf("127.0.0.1:%d", ssmPort),
		Sigv4Service: si.service.Sigv4Service,
		RealHost:     si.service.RemoteHost,
		AWSProfile:   profile.AWSProfile,
		AWSRegion:    profile.AWSRegion,
	})
	if err != nil {
		tcpProxy = nil
		return nil, fmt.Errorf("starting proxy: %w", err)
	}

	// stopService cancels ctx before it reads si.session, so checking ctx under si.mu means either
	// stopService sees this session or this commit sees the stop.
	si.mu.Lock()
	if err := ctx.Err(); err != nil {
		si.mu.Unlock()
		return nil, err
	}
	if err := state.Transition(si.state, state.Connected); err != nil {
		si.mu.Unlock()
		return nil, err
	}
	si.state = state.Connected
	si.session = sess
	si.tcpProxy = tcpProxy
	si.lastError = nil
	si.connectedAt = time.Now()
	si.mu.Unlock()
	committed = true

	si.backoff.MarkConnected()
	e.emitStateChange(si, state.Starting, state.Connected, nil)
	e.emitLog("info", fmt.Sprintf("connected via %s:%d (ssm port %d)", si.localIP, si.service.LocalPort, ssmPort), si.profile, si.service.Name)
	return sess, nil
}

// credentialsExpired moves the service to Failed and emits CredentialsExpired when err wraps
// session.ErrCredentialsExpired: no retry can succeed until the user runs `aws sso login` and
// restarts the service.
func (e *Engine) credentialsExpired(si *serviceInstance, from state.ServiceState, err error, profile config.Profile) bool {
	if !errors.Is(err, session.ErrCredentialsExpired) {
		return false
	}
	si.mu.Lock()
	si.lastError = err
	si.mu.Unlock()
	if terr := si.transition(state.Failed); terr != nil {
		e.emitLog("error", fmt.Sprintf("cannot transition to Failed: %v", terr), si.profile, si.service.Name)
		return true
	}
	e.emitStateChange(si, from, state.Failed, err)
	e.emit(CredentialsExpired{
		Profile:     si.profile,
		AWSProfile:  profile.AWSProfile,
		ServiceName: si.service.Name,
		Detail:      err.Error(),
		Timestamp:   time.Now(),
	})
	e.emitLog("error", fmt.Sprintf("credentials expired for aws profile %q — run: aws sso login --profile %s", profile.AWSProfile, profile.AWSProfile), si.profile, si.service.Name)
	return true
}

// reconnecting records err as the service's last error and moves it to Reconnecting. It reports
// false when the move is refused, which ends the service loop.
func (e *Engine) reconnecting(si *serviceInstance, from state.ServiceState, err error) bool {
	si.mu.Lock()
	si.lastError = err
	si.mu.Unlock()
	if terr := si.transition(state.Reconnecting); terr != nil {
		e.emitLog("error", fmt.Sprintf("cannot transition to Reconnecting: %v", terr), si.profile, si.service.Name)
		return false
	}
	e.emitStateChange(si, from, state.Reconnecting, err)
	return true
}

func (e *Engine) stopService(ctx context.Context, si *serviceInstance) {
	si.cancel()

	si.mu.Lock()
	sess := si.session
	currentState := si.state
	si.mu.Unlock()

	if currentState != state.Disconnected && currentState != state.Stopping {
		if err := si.transition(state.Stopping); err != nil {
			e.emitLog("warn", fmt.Sprintf("cannot transition to Stopping: %v", err), si.profile, si.service.Name)
		} else {
			e.emitStateChange(si, currentState, state.Stopping, nil)
		}
	}

	if sess != nil {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := sess.Stop(stopCtx); err != nil {
			e.emitLog("warn", fmt.Sprintf("error stopping session: %v", err), si.profile, si.service.Name)
		}
	}

	si.mu.Lock()
	proxy := si.tcpProxy
	si.tcpProxy = nil
	si.mu.Unlock()
	if proxy != nil {
		proxy.Stop()
	}

	if err := si.transition(state.Disconnected); err != nil {
		e.emitLog("warn", fmt.Sprintf("cannot transition to Disconnected: %v", err), si.profile, si.service.Name)
		return
	}
	e.emitStateChange(si, state.Stopping, state.Disconnected, nil)
}

func (e *Engine) emit(evt Event) {
	select {
	case e.events <- evt:
	default:
	}
}

func (e *Engine) emitLog(level, msg, profile, service string) {
	e.emit(LogEntry{
		Level:   level,
		Message: msg,
		Profile: profile,
		Service: service,
		Time:    time.Now(),
	})
}

func (e *Engine) emitStateChange(si *serviceInstance, from, to state.ServiceState, err error) {
	e.emit(ServiceStateChanged{
		Profile:     si.profile,
		ServiceName: si.service.Name,
		DNSName:     si.service.DNSName,
		From:        from,
		To:          to,
		Error:       err,
		Timestamp:   time.Now(),
	})
}

// serviceInstance tracks a running service within the engine.
type serviceInstance struct {
	mu          sync.Mutex
	profile     string
	service     config.Service
	state       state.ServiceState
	session     session.Session
	tcpProxy    TCPProxy
	cancel      context.CancelFunc
	backoff     *reconnect.Backoff
	connectedAt time.Time
	lastError   error
	localIP     string
}

func (si *serviceInstance) getState() state.ServiceState {
	si.mu.Lock()
	defer si.mu.Unlock()
	return si.state
}

func (si *serviceInstance) transition(to state.ServiceState) error {
	si.mu.Lock()
	defer si.mu.Unlock()
	if err := state.Transition(si.state, to); err != nil {
		return err
	}
	si.state = to
	return nil
}

// target identifies a single configured service.
type target struct {
	profile string
	service config.Service
}

// freePort asks the OS for an available port on 127.0.0.1.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("finding free port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, fmt.Errorf("releasing free port: %w", err)
	}
	return port, nil
}
