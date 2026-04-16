package engine

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Kwutzke/holepunch/internal/config"
	"github.com/Kwutzke/holepunch/internal/dns"
	"github.com/Kwutzke/holepunch/internal/reconnect"
	"github.com/Kwutzke/holepunch/internal/session"
	"github.com/Kwutzke/holepunch/internal/state"
)

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

// Engine orchestrates port-forwarding sessions for all profiles.
type Engine struct {
	cfg        config.Config
	dns        DNSManager
	sessions   SessionManager
	ips        IPAllocator
	proxies    ProxyFactory
	events     chan Event
	mu         sync.RWMutex
	instances  map[string]*serviceInstance // key: "profile/service"
	cancelFunc context.CancelFunc
}

// New creates a new Engine.
func New(cfg config.Config, dnsMgr DNSManager, sessMgr SessionManager, ipAlloc IPAllocator, proxyFactory ProxyFactory) *Engine {
	return &Engine{
		cfg:       cfg,
		dns:       dnsMgr,
		sessions:  sessMgr,
		ips:       ipAlloc,
		proxies:   proxyFactory,
		events:    make(chan Event, 100),
		instances: make(map[string]*serviceInstance),
	}
}

// Events returns the channel on which engine events are published.
func (e *Engine) Events() <-chan Event {
	return e.events
}

func (e *Engine) emit(evt Event) {
	select {
	case e.events <- evt:
	default:
		// Drop event if channel is full — don't block the engine.
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

// Start begins port-forwarding for the specified profiles.
// It is additive — calling Start with new profiles adds them alongside existing ones.
func (e *Engine) Start(ctx context.Context, profiles []string) error {
	for _, profileName := range profiles {
		profile, ok := e.cfg.Profiles[profileName]
		if !ok {
			return fmt.Errorf("unknown profile: %q", profileName)
		}

		for _, svc := range profile.Services {
			key := profileName + "/" + svc.Name

			e.mu.RLock()
			_, exists := e.instances[key]
			e.mu.RUnlock()
			if exists {
				e.emitLog("warn", "already running, skipping", profileName, svc.Name)
				continue
			}

			ip, err := e.ips.Allocate(key)
			if err != nil {
				return fmt.Errorf("allocating IP for %s: %w", key, err)
			}

			si := &serviceInstance{
				profile: profileName,
				service: svc,
				state:   state.Disconnected,
				backoff: reconnect.NewBackoff(),
				localIP: ip.String(),
			}

			e.mu.Lock()
			e.instances[key] = si
			e.mu.Unlock()

			svcCtx, cancel := context.WithCancel(ctx)
			si.cancel = cancel

			go e.runService(svcCtx, si, profile)
		}
	}
	return nil
}

// Stop stops port-forwarding for the specified profiles.
// If no profiles are given, all running profiles are stopped.
func (e *Engine) Stop(ctx context.Context, profiles []string) error {
	e.mu.RLock()
	toStop := make([]*serviceInstance, 0)
	for key, si := range e.instances {
		if len(profiles) == 0 || matchesProfile(key, profiles) {
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

	// Emit ProfileDone for each profile that has no more instances.
	doneProfiles := make(map[string]bool)
	for _, si := range toStop {
		doneProfiles[si.profile] = true
	}
	e.mu.RLock()
	for key := range e.instances {
		for p := range doneProfiles {
			if matchesProfile(key, []string{p}) {
				delete(doneProfiles, p)
			}
		}
	}
	e.mu.RUnlock()
	for p := range doneProfiles {
		e.emit(ProfileDone{Profile: p, Timestamp: time.Now()})
	}

	return nil
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

func (e *Engine) runService(ctx context.Context, si *serviceInstance, profile config.Profile) {
	key := si.profile + "/" + si.service.Name

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Transition to Starting.
		from := si.getState()
		if err := si.transition(state.Starting); err != nil {
			e.emitLog("error", fmt.Sprintf("invalid transition to Starting from %v: %v", from, err), si.profile, si.service.Name)
			return
		}
		e.emitStateChange(si, from, state.Starting, nil)

		// Add DNS entry.
		ip, _ := e.ips.Allocate(key)
		err := e.dns.Add(ctx, []dns.Entry{{IP: ip, Hostname: si.service.DNSName}})
		if err != nil {
			e.emitLog("error", fmt.Sprintf("failed to add DNS entry: %v", err), si.profile, si.service.Name)
			si.mu.Lock()
			si.lastError = err
			si.mu.Unlock()
			_ = si.transition(state.Failed)
			e.emitStateChange(si, state.Starting, state.Failed, err)
			return
		}

		// Pick a free port for SSM to bind on 127.0.0.1.
		ssmPort, err := freePort()
		if err != nil {
			si.mu.Lock()
			si.lastError = err
			si.mu.Unlock()
			_ = si.transition(state.Failed)
			e.emitStateChange(si, state.Starting, state.Failed, err)
			return
		}

		// Start SSM session on 127.0.0.1:<random>.
		sess, err := e.sessions.Start(ctx, session.StartParams{
			AWSProfile: profile.AWSProfile,
			AWSRegion:  profile.AWSRegion,
			Target:     profile.Target,
			RemoteHost: si.service.RemoteHost,
			RemotePort: si.service.RemotePort,
			LocalIP:    net.IPv4(127, 0, 0, 1),
			LocalPort:  ssmPort,
		})
		if err != nil {
			si.mu.Lock()
			si.lastError = err
			si.mu.Unlock()
			_ = si.transition(state.Failed)
			e.emitStateChange(si, state.Starting, state.Failed, err)
			return
		}

		// Start proxy: 127.0.0.x:<real_port> → 127.0.0.1:<ssm_port>.
		listenAddr := fmt.Sprintf("%s:%d", ip.String(), si.service.LocalPort)
		targetAddr := fmt.Sprintf("127.0.0.1:%d", ssmPort)
		tcpProxy, err := e.proxies.NewProxy(ctx, ProxyConfig{
			ListenAddr:   listenAddr,
			TargetAddr:   targetAddr,
			Sigv4Service: si.service.Sigv4Service,
			RealHost:     si.service.RemoteHost,
			AWSProfile:   profile.AWSProfile,
			AWSRegion:    profile.AWSRegion,
		})
		if err != nil {
			sess.Stop(ctx)
			si.mu.Lock()
			si.lastError = err
			si.mu.Unlock()
			_ = si.transition(state.Failed)
			e.emitStateChange(si, state.Starting, state.Failed, err)
			return
		}

		si.mu.Lock()
		si.session = sess
		si.tcpProxy = tcpProxy
		si.mu.Unlock()

		// Transition to Connected.
		_ = si.transition(state.Connected)
		si.mu.Lock()
		si.connectedAt = time.Now()
		si.mu.Unlock()
		si.backoff.MarkConnected()
		e.emitStateChange(si, state.Starting, state.Connected, nil)
		e.emitLog("info", fmt.Sprintf("connected via %s:%d (ssm port %d)", si.localIP, si.service.LocalPort, ssmPort), si.profile, si.service.Name)

		// Wait for session to exit.
		waitErr := sess.Wait()

		// Check if we were asked to stop.
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Session dropped — stop proxy and transition to Reconnecting.
		si.mu.Lock()
		if si.tcpProxy != nil {
			si.tcpProxy.Stop()
			si.tcpProxy = nil
		}
		si.lastError = waitErr
		si.session = nil
		si.mu.Unlock()

		if err := si.transition(state.Reconnecting); err != nil {
			e.emitLog("error", fmt.Sprintf("cannot transition to Reconnecting: %v", err), si.profile, si.service.Name)
			return
		}
		e.emitStateChange(si, state.Connected, state.Reconnecting, waitErr)

		// Check if connection was stable enough to reset backoff.
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

		// Transition back to Starting for the reconnect loop.
		_ = si.transition(state.Starting)
		e.emitStateChange(si, state.Reconnecting, state.Starting, nil)

		// Reset state so the loop re-enters from Starting -> Connected.
		si.mu.Lock()
		si.state = state.Disconnected
		si.mu.Unlock()
	}
}

func (e *Engine) stopService(ctx context.Context, si *serviceInstance) {
	si.cancel()

	si.mu.Lock()
	sess := si.session
	currentState := si.state
	si.mu.Unlock()

	if currentState != state.Disconnected && currentState != state.Stopping {
		_ = si.transition(state.Stopping)
		e.emitStateChange(si, currentState, state.Stopping, nil)
	}

	if sess != nil {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := sess.Stop(stopCtx); err != nil {
			e.emitLog("warn", fmt.Sprintf("error stopping session: %v", err), si.profile, si.service.Name)
		}
	}

	si.mu.Lock()
	if si.tcpProxy != nil {
		si.tcpProxy.Stop()
		si.tcpProxy = nil
	}
	si.mu.Unlock()

	_ = si.transition(state.Disconnected)
	e.emitStateChange(si, state.Stopping, state.Disconnected, nil)
}

func matchesProfile(key string, profiles []string) bool {
	for _, p := range profiles {
		if len(key) > len(p) && key[:len(p)+1] == p+"/" {
			return true
		}
	}
	return false
}

// freePort asks the OS for an available port on 127.0.0.1.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("finding free port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}
