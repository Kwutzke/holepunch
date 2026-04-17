// Package tray implements the macOS menu bar UI for holepunch. It reads the
// daemon's typed event stream, reflects service state in a flat clickable
// menu, and surfaces credential-expiry as a clickable "Login" item.
//
// The tray runs in-process as a cobra subcommand (holepunch tray) and
// requires the OS main thread (systray uses NSApplicationMain on darwin).
package tray

import (
	_ "embed"
	"fmt"
	"log"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/Kwutzke/holepunch/internal/daemon"
)

// --- embedded icons -------------------------------------------------------

//go:embed icons/idle.png
var iconIdle []byte

//go:embed icons/ok.png
var iconOK []byte

//go:embed icons/partial.png
var iconPartial []byte

//go:embed icons/warn.png
var iconWarn []byte

//go:embed icons/err.png
var iconErr []byte

// iconState is the aggregate-state selector for the tray icon. Ordered by
// severity so a worst-of reduction can use a simple max().
type iconState int

const (
	stateIdle iconState = iota
	stateOK
	statePartial // some connected, some off — green ring (unfilled)
	stateWarn
	stateErr
)

// iconFor returns the PNG bytes for the given aggregate state.
func iconFor(s iconState) []byte {
	switch s {
	case stateOK:
		return iconOK
	case statePartial:
		return iconPartial
	case stateWarn:
		return iconWarn
	case stateErr:
		return iconErr
	default:
		return iconIdle
	}
}

// --- entry point ----------------------------------------------------------

// Run blocks the calling goroutine on systray.Run. Caller is responsible
// for pinning the OS thread via runtime.LockOSThread before invoking this,
// otherwise NSApplicationMain will be on a non-main thread on darwin.
func Run(socketPath string) {
	t := newTray(socketPath)
	systray.Run(t.onReady, t.onExit)
}

// --- internal state -------------------------------------------------------

type serviceEntry struct {
	profile     string
	serviceName string
	dnsName     string
	localPort   int
	localAddr   string
	state       string
	err         string
	menuItem    *systray.MenuItem
}

type profileEntry struct {
	name         string
	credsExpired bool
	awsProfile   string
	services     map[string]*serviceEntry

	headerItem *systray.MenuItem // disabled, shows profile name
	loginItem  *systray.MenuItem // hidden unless credsExpired
}

type tray struct {
	socketPath string

	mu       sync.Mutex
	profiles map[string]*profileEntry
	built    bool // true once the profile section of the menu has been rendered

	// eventSeq increments every time apply() mutates service state. Seeds
	// capture this before the RTT; if it changed by the time the seed
	// applies, state fields are left alone (events are authoritative and
	// more recent than the snapshot). This both avoids the seed-vs-event
	// race and lets a seed recover from an event that was dropped by the
	// bus (eventSeq didn't tick, so the seed is safe to apply fully).
	eventSeq uint64

	// bottom-level menu handles
	startAllItem   *systray.MenuItem
	stopAllItem    *systray.MenuItem
	restartAllItem *systray.MenuItem
	logsItem       *systray.MenuItem
	quitItem       *systray.MenuItem

	// event pump
	events  chan *daemon.EventEnvelope
	refresh chan struct{}
	stopCh  chan struct{}
}

func newTray(socketPath string) *tray {
	return &tray{
		socketPath: socketPath,
		profiles:   make(map[string]*profileEntry),
		events:     make(chan *daemon.EventEnvelope, 128),
		refresh:    make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
	}
}

// --- systray lifecycle ----------------------------------------------------

func (t *tray) onReady() {
	systray.SetIcon(iconFor(stateIdle))
	systray.SetTitle("")
	systray.SetTooltip("holepunch")

	// Seed synchronously BEFORE building the menu so profile+service items
	// appear above the static actions. systray has no insert-before, so
	// anything discovered later in this session is a no-op (user must
	// restart the tray after config changes).
	t.seedStatuses()
	t.buildProfilesMenu()
	t.buildStaticMenu()

	go t.dispatchEvents()
	go t.debouncedRefresh()
	go t.streamDaemon()
	go t.periodicReseed()
	t.triggerRefresh()
}

func (t *tray) onExit() {
	close(t.stopCh)
}

// buildProfilesMenu creates one row per profile/service based on whatever
// the initial seed returned. Runs once per tray session.
func (t *tray) buildProfilesMenu() {
	t.mu.Lock()
	defer t.mu.Unlock()

	names := make([]string, 0, len(t.profiles))
	for n := range t.profiles {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, pname := range names {
		p := t.profiles[pname]
		p.headerItem = systray.AddMenuItem(pname, "")
		p.headerItem.Disable()

		p.loginItem = systray.AddMenuItem("↻ Login — aws sso login", "Run aws sso login in Terminal")
		p.loginItem.Hide()
		go t.watchLoginClick(p)

		svcNames := make([]string, 0, len(p.services))
		for n := range p.services {
			svcNames = append(svcNames, n)
		}
		sort.Strings(svcNames)
		for _, sname := range svcNames {
			svc := p.services[sname]
			svc.menuItem = systray.AddMenuItem(serviceLabel(svc), "Click to toggle")
			go t.watchServiceClick(svc)
		}

		systray.AddSeparator()
	}
	t.built = true
}

// buildStaticMenu adds the always-present action items below the profiles.
func (t *tray) buildStaticMenu() {
	t.startAllItem = systray.AddMenuItem("Start All", "Start every profile")
	t.stopAllItem = systray.AddMenuItem("Stop All", "Stop every profile")
	t.restartAllItem = systray.AddMenuItem("Restart All", "Restart every profile")
	systray.AddSeparator()
	t.logsItem = systray.AddMenuItem("Open Logs", "Follow holepunch logs in Terminal")
	systray.AddSeparator()
	t.quitItem = systray.AddMenuItem("Quit Tray", "Exit the holepunch tray UI")

	go t.watchStaticClicks()
}

func (t *tray) watchStaticClicks() {
	for {
		select {
		case <-t.stopCh:
			return
		case <-t.startAllItem.ClickedCh:
			go t.sendDaemon(daemon.CmdUp, t.allProfileTargets())
		case <-t.stopAllItem.ClickedCh:
			// Enumerate profiles so the daemon doesn't shut itself down —
			// empty targets on CmdDown would stop everything AND the
			// daemon, killing the tray's stream.
			go t.sendDaemon(daemon.CmdDown, t.allProfileTargets())
		case <-t.restartAllItem.ClickedCh:
			go t.restartAll()
		case <-t.logsItem.ClickedCh:
			go t.openLogsTerminal()
		case <-t.quitItem.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// allProfileTargets returns every known profile name, so CmdUp/CmdDown
// address them explicitly rather than using the empty-means-everything
// sentinel (which triggers daemon shutdown on CmdDown).
func (t *tray) allProfileTargets() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.profiles))
	for name := range t.profiles {
		out = append(out, name)
	}
	return out
}

// restartAll sends Down then Up for every profile. We don't use the CLI
// 'holepunch restart' here because it kills the daemon, which severs the
// tray's event stream. This keeps the tray connected throughout.
func (t *tray) restartAll() {
	targets := t.allProfileTargets()
	t.sendDaemon(daemon.CmdDown, targets)
	t.sendDaemon(daemon.CmdUp, targets)
}

// watchServiceClick toggles the service based on its current observed state.
// Connected/Starting/Reconnecting → down. Everything else → up.
func (t *tray) watchServiceClick(svc *serviceEntry) {
	for {
		select {
		case <-t.stopCh:
			return
		case <-svc.menuItem.ClickedCh:
			target := svc.profile + "/" + svc.serviceName
			t.mu.Lock()
			cur := svc.state
			t.mu.Unlock()
			switch cur {
			case "Connected", "Starting", "Reconnecting":
				go t.sendDaemon(daemon.CmdDown, []string{target})
			default:
				go t.sendDaemon(daemon.CmdUp, []string{target})
			}
		}
	}
}

// sendDaemon dispatches a control request directly to the daemon socket.
// Skipping the `holepunch` CLI fork saves ~100-300ms of process spawn and
// makes clicks feel responsive.
func (t *tray) sendDaemon(cmd string, targets []string) {
	client := daemon.NewClient(t.socketPath)
	resp, err := client.SendCommand(daemon.Request{Command: cmd, Targets: targets})
	if err != nil {
		log.Printf("tray: %s %v: %v", cmd, targets, err)
		return
	}
	if !resp.OK {
		log.Printf("tray: %s %v: %s", cmd, targets, resp.Error)
	}
}

func (t *tray) watchLoginClick(p *profileEntry) {
	for {
		select {
		case <-t.stopCh:
			return
		case <-p.loginItem.ClickedCh:
			go t.openLoginTerminal(p.awsProfile)
		}
	}
}

// seedStatuses pulls the current status snapshot.
//
// Captures eventSeq before the daemon RTT. On return, if eventSeq is
// unchanged, no events landed during the RTT so the snapshot is
// authoritative and state/err are adopted. If events did land, the
// snapshot predates them; we only adopt DNSName/LocalAddr (which aren't
// carried on state events) and leave state alone.
//
// Returns false when the daemon is unreachable or errors.
func (t *tray) seedStatuses() bool {
	client := daemon.NewClient(t.socketPath)

	t.mu.Lock()
	baseline := t.eventSeq
	t.mu.Unlock()

	resp, err := client.SendCommand(daemon.Request{Command: daemon.CmdStatus})
	if err != nil || !resp.OK {
		return false
	}

	seen := make(map[string]bool)
	t.mu.Lock()
	safe := t.eventSeq == baseline
	for _, st := range resp.Statuses {
		key := st.Profile + "/" + st.ServiceName
		seen[key] = true

		p := t.getOrCreateProfileLocked(st.Profile)
		svc, existed := p.services[st.ServiceName]
		if !existed {
			svc = &serviceEntry{profile: st.Profile, serviceName: st.ServiceName}
			p.services[st.ServiceName] = svc
		}
		svc.dnsName = st.DNSName
		svc.localPort = st.LocalPort
		svc.localAddr = st.LocalAddr
		if safe || !existed {
			svc.state = st.State
			svc.err = st.Error
		}
	}
	// Items that existed before but aren't in the snapshot: mark
	// Disconnected. Only safe when no events interleaved — otherwise the
	// event that created this entry may be newer than the snapshot.
	if safe {
		for pname, p := range t.profiles {
			for sname, svc := range p.services {
				if !seen[pname+"/"+sname] {
					svc.state = "Disconnected"
					svc.err = ""
					svc.localAddr = ""
				}
			}
		}
	}
	t.mu.Unlock()
	t.triggerRefresh()
	return true
}

// --- daemon stream --------------------------------------------------------

func (t *tray) streamDaemon() {
	client := daemon.NewClient(t.socketPath)
	backoff := time.Second
	for {
		select {
		case <-t.stopCh:
			return
		default:
		}

		// Probe + re-seed via CmdStatus. Doubles as a liveness check so
		// we can reset backoff on the happy path and pick up any state
		// we missed while disconnected.
		if !t.seedStatuses() {
			select {
			case <-t.stopCh:
				return
			case <-time.After(backoff):
			}
			if backoff < 10*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		err := client.StreamEvents(daemon.Request{Command: daemon.CmdTrayRegister}, func(env *daemon.EventEnvelope) bool {
			select {
			case t.events <- env:
			case <-t.stopCh:
				return false
			}
			return true
		})
		if err != nil {
			log.Printf("tray: daemon stream: %v", err)
		}

		select {
		case <-t.stopCh:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// periodicReseed re-pulls the status snapshot at a steady cadence. This is
// the safety net for any state drift — dropped events, missed Connected
// transitions, or service state changes triggered by external `holepunch`
// CLI calls we didn't subscribe for.
func (t *tray) periodicReseed() {
	const interval = 5 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.seedStatuses()
		}
	}
}

func (t *tray) dispatchEvents() {
	for {
		select {
		case <-t.stopCh:
			return
		case env := <-t.events:
			t.apply(env)
			t.triggerRefresh()
		}
	}
}

func (t *tray) apply(env *daemon.EventEnvelope) {
	t.mu.Lock()
	reseed := false
	switch env.Kind {
	case daemon.EnvelopeKindState:
		if env.State == nil {
			t.mu.Unlock()
			return
		}
		p := t.getOrCreateProfileLocked(env.State.Profile)
		svc, ok := p.services[env.State.ServiceName]
		if !ok {
			svc = &serviceEntry{
				profile:     env.State.Profile,
				serviceName: env.State.ServiceName,
				dnsName:     env.State.DNSName,
			}
			p.services[env.State.ServiceName] = svc
		}
		svc.state = env.State.To
		svc.err = env.State.Error
		if env.State.To != "Failed" {
			p.credsExpired = false
		}
		if env.State.To == "Disconnected" {
			svc.localAddr = ""
		}
		// localAddr isn't on the state event; reseed on Starting and on
		// Connected-without-addr so new assignments propagate.
		if env.State.To == "Starting" || (env.State.To == "Connected" && svc.localAddr == "") {
			reseed = true
		}
		t.eventSeq++
	case daemon.EnvelopeKindCredsExpired:
		if env.Creds == nil {
			t.mu.Unlock()
			return
		}
		p := t.getOrCreateProfileLocked(env.Creds.Profile)
		p.credsExpired = true
		p.awsProfile = env.Creds.AWSProfile
		t.eventSeq++
	case daemon.EnvelopeKindProfileDone:
		reseed = true
	}
	t.mu.Unlock()

	if reseed {
		go t.seedStatuses()
	}
}

func (t *tray) getOrCreateProfileLocked(name string) *profileEntry {
	p, ok := t.profiles[name]
	if !ok {
		p = &profileEntry{
			name:     name,
			services: make(map[string]*serviceEntry),
		}
		t.profiles[name] = p
	}
	return p
}

// --- debounced menu refresh ----------------------------------------------

func (t *tray) triggerRefresh() {
	select {
	case t.refresh <- struct{}{}:
	default:
	}
}

func (t *tray) debouncedRefresh() {
	const debounce = 50 * time.Millisecond
	var pending <-chan time.Time
	for {
		select {
		case <-t.stopCh:
			return
		case <-t.refresh:
			if pending == nil {
				pending = time.After(debounce)
			}
		case <-pending:
			pending = nil
			t.rebuild()
		}
	}
}

// rebuild updates labels and visibility on existing menu items and sets the
// aggregate icon. It does NOT create new items — that happens once at
// onReady. A config change discovered mid-session is currently not
// reflected until the user restarts the tray.
func (t *tray) rebuild() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.built {
		// onReady hasn't built the menu yet — just update the icon.
		systray.SetIcon(iconFor(t.aggregateStateLocked()))
		return
	}

	for _, p := range t.profiles {
		if p.headerItem == nil {
			continue // profile appeared after initial build; ignored
		}
		if p.credsExpired {
			p.headerItem.SetTitle(p.name + "  ⚠ credentials expired")
			p.loginItem.Show()
		} else {
			p.headerItem.SetTitle(p.name)
			p.loginItem.Hide()
		}
		for _, svc := range p.services {
			if svc.menuItem == nil {
				continue
			}
			svc.menuItem.SetTitle(serviceLabel(svc))
		}
	}

	systray.SetIcon(iconFor(t.aggregateStateLocked()))
}

// serviceLabel renders a single-line status for a service row as
// "marker  dns:port". Failed services append the error text after an em
// dash. The port comes from config (seeded via StatusEntry.LocalPort) so
// it's shown even when the service is not running.
func serviceLabel(svc *serviceEntry) string {
	marker := stateMarker(svc.state)
	addr := svc.dnsName
	if svc.localPort != 0 {
		addr = fmt.Sprintf("%s:%d", svc.dnsName, svc.localPort)
	}
	parts := []string{marker, addr}
	if svc.state == "Failed" {
		if svc.err != "" {
			parts = append(parts, "— "+svc.err)
		} else {
			parts = append(parts, "— failed")
		}
	}
	return strings.Join(parts, "  ")
}

func stateMarker(s string) string {
	switch s {
	case "Connected":
		return "●"
	case "Starting", "Reconnecting":
		return "◐"
	case "Failed":
		return "✗"
	case "Stopping":
		return "◌"
	default:
		return "○"
	}
}

// aggregateStateLocked computes the icon state across every service.
// Rules:
//   - any creds_expired or Failed → err
//   - any Starting/Reconnecting/Stopping (in-flight) → warn
//   - some Connected AND some Disconnected → partial (green ring)
//   - all Connected → ok (green filled)
//   - nothing connected and nothing in flight → idle
func (t *tray) aggregateStateLocked() iconState {
	var anyConnected, anyDisconnected, anyInFlight bool

	for _, p := range t.profiles {
		if p.credsExpired {
			return stateErr
		}
		for _, svc := range p.services {
			switch svc.state {
			case "Failed":
				return stateErr
			case "Starting", "Reconnecting", "Stopping":
				anyInFlight = true
			case "Connected":
				anyConnected = true
			case "", "Disconnected":
				anyDisconnected = true
			}
		}
	}
	if anyInFlight {
		return stateWarn
	}
	if anyConnected && anyDisconnected {
		return statePartial
	}
	if anyConnected {
		return stateOK
	}
	return stateIdle
}

// --- shell-out helpers ----------------------------------------------------

// openLogsTerminal opens a new Terminal window running `holepunch logs -f`.
func (t *tray) openLogsTerminal() {
	script := fmt.Sprintf(`tell application "Terminal" to do script "holepunch --socket %s logs -f"`, t.socketPath)
	if err := exec.Command("osascript", "-e", script).Run(); err != nil {
		log.Printf("tray: open logs: %v", err)
	}
}

// openLoginTerminal opens a Terminal window running `aws sso login` for the
// given AWS profile. This is the non-clickable-notification fallback path;
// PR3 will replace it with terminal-notifier -execute for in-notification
// action.
func (t *tray) openLoginTerminal(awsProfile string) {
	if awsProfile == "" {
		return
	}
	script := fmt.Sprintf(`tell application "Terminal" to do script "aws sso login --profile %s"`, awsProfile)
	if err := exec.Command("osascript", "-e", script).Run(); err != nil {
		log.Printf("tray: open login: %v", err)
	}
}
