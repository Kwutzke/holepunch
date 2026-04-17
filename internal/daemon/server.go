package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kwutzke/holepunch/internal/engine"
	"github.com/Kwutzke/holepunch/internal/state"
)

// Server listens on a unix socket and dispatches commands to the engine.
type Server struct {
	eng           *engine.Engine
	bus           *Bus
	listener      net.Listener
	wg            sync.WaitGroup
	notifier      *Notifier
	hasTrayClient atomic.Int32
}

// NewServer creates a daemon server bound to the given socket path.
func NewServer(socketPath string, eng *engine.Engine) (*Server, error) {
	// Remove stale socket if it exists.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale socket: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", socketPath, err)
	}

	bus := NewBus(eng.Events())
	s := &Server{
		eng:      eng,
		bus:      bus,
		listener: listener,
	}
	s.notifier = NewNotifier(bus, s.hasTrayClientLoad)
	return s, nil
}

// hasTrayClientLoad exposes the tray-client counter to the notifier as a
// closure-friendly reader (avoids the notifier importing atomic state from
// Server directly).
func (s *Server) hasTrayClientLoad() bool {
	return s.hasTrayClient.Load() > 0
}

// Serve starts accepting connections. Blocks until the context is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.listener.Close()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				s.wg.Wait()
				return ctx.Err()
			default:
				return fmt.Errorf("accepting connection: %w", err)
			}
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			s.handleConnection(ctx, conn)
		}()
	}
}

// SocketPath returns the path the server is listening on.
func (s *Server) SocketPath() string {
	return s.listener.Addr().String()
}

func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	var req Request
	if err := readMessage(conn, &req); err != nil {
		writeMessage(conn, Response{Error: fmt.Sprintf("reading request: %v", err)})
		return
	}

	switch req.Command {
	case CmdUp:
		s.handleUp(ctx, conn, req)
	case CmdDown:
		s.handleDown(ctx, conn, req)
	case CmdStatus:
		s.handleStatus(conn)
	case CmdLogs:
		s.handleLogs(ctx, conn, req)
	case CmdSubscribe:
		s.handleSubscribe(ctx, conn)
	case CmdTrayRegister:
		s.handleTrayRegister(ctx, conn)
	default:
		writeMessage(conn, Response{Error: fmt.Sprintf("unknown command: %s", req.Command)})
	}
}

func (s *Server) handleUp(ctx context.Context, conn net.Conn, req Request) {
	if err := s.eng.Start(ctx, req.Targets); err != nil {
		writeMessage(conn, Response{Error: err.Error()})
		return
	}
	writeMessage(conn, Response{OK: true})
}

func (s *Server) handleDown(ctx context.Context, conn net.Conn, req Request) {
	if err := s.eng.Stop(ctx, req.Targets); err != nil {
		writeMessage(conn, Response{Error: err.Error()})
		return
	}
	writeMessage(conn, Response{OK: true})
}

func (s *Server) handleStatus(conn net.Conn) {
	// Start with every configured service as Disconnected so UIs can show
	// things that exist in config but aren't running yet. Overlay live
	// statuses on top by (profile, serviceName) key.
	cfg := s.eng.Config()
	byKey := make(map[string]*StatusEntry)
	order := make([]string, 0)
	for profileName, profile := range cfg.Profiles {
		for _, svc := range profile.Services {
			key := profileName + "/" + svc.Name
			byKey[key] = &StatusEntry{
				Profile:     profileName,
				ServiceName: svc.Name,
				DNSName:     svc.DNSName,
				LocalPort:   svc.LocalPort,
				State:       state.Disconnected.String(),
			}
			order = append(order, key)
		}
	}

	for _, st := range s.eng.Status() {
		key := st.Profile + "/" + st.ServiceName
		entry, ok := byKey[key]
		if !ok {
			// A running instance for a service not in config — unusual
			// but possible if config was reloaded and service removed.
			entry = &StatusEntry{Profile: st.Profile, ServiceName: st.ServiceName}
			byKey[key] = entry
			order = append(order, key)
		}
		entry.DNSName = st.DNSName
		entry.LocalAddr = st.LocalAddr
		entry.State = st.State.String()
		if st.State == state.Connected && !st.ConnectedAt.IsZero() {
			entry.ConnectedAt = time.Since(st.ConnectedAt).Truncate(time.Second).String()
		}
		if st.Error != nil {
			entry.Error = st.Error.Error()
		}
	}

	entries := make([]StatusEntry, 0, len(order))
	for _, key := range order {
		entries = append(entries, *byKey[key])
	}
	writeMessage(conn, Response{OK: true, Statuses: entries})
}

func (s *Server) handleLogs(ctx context.Context, conn net.Conn, req Request) {
	events, cancel := s.bus.Subscribe()
	defer cancel()

	// Send initial OK response.
	if err := writeMessage(conn, Response{OK: true}); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			logEntry, ok := eventToLogEntry(evt)
			if !ok {
				continue
			}
			if err := writeMessage(conn, Response{OK: true, Event: &logEntry}); err != nil {
				return
			}
			if !req.Follow {
				return
			}
		}
	}
}

// handleSubscribe streams typed envelopes for any subscriber. Used by the
// tray (and potentially other UIs) that want typed events rather than the
// legacy LogEntry projection.
func (s *Server) handleSubscribe(ctx context.Context, conn net.Conn) {
	s.streamEnvelopes(ctx, conn)
}

// handleTrayRegister behaves like subscribe but increments hasTrayClient for
// the duration of the connection so the daemon's osascript fallback
// suppresses itself (the tray owns notifications when it's connected).
func (s *Server) handleTrayRegister(ctx context.Context, conn net.Conn) {
	s.hasTrayClient.Add(1)
	defer s.hasTrayClient.Add(-1)
	s.streamEnvelopes(ctx, conn)
}

func (s *Server) streamEnvelopes(ctx context.Context, conn net.Conn) {
	events, cancel := s.bus.Subscribe()
	defer cancel()

	if err := writeMessage(conn, Response{OK: true}); err != nil {
		return
	}

	// Watch for client-side disconnect. Without this, a quiet stream (no
	// events) will block in the select below until ctx cancels at daemon
	// shutdown — so the tray counter wouldn't decrement on client close.
	disconnected := make(chan struct{})
	go func() {
		defer close(disconnected)
		buf := make([]byte, 64)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-disconnected:
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			env, ok := eventToEnvelope(evt)
			if !ok {
				continue
			}
			if err := writeMessage(conn, Response{OK: true, Envelope: env}); err != nil {
				return
			}
		}
	}
}

// eventToLogEntry projects any engine.Event onto the legacy LogEntry shape
// used by CmdLogs clients. Returns false for events that shouldn't be
// streamed as logs (e.g. ProfileDone).
func eventToLogEntry(evt engine.Event) (engine.LogEntry, bool) {
	switch e := evt.(type) {
	case engine.LogEntry:
		return e, true
	case engine.ServiceStateChanged:
		msg := fmt.Sprintf("%s/%s: %s -> %s", e.Profile, e.ServiceName, e.From, e.To)
		if e.Error != nil {
			msg += fmt.Sprintf(" (%v)", e.Error)
		}
		return engine.LogEntry{
			Level:   "info",
			Message: msg,
			Profile: e.Profile,
			Service: e.ServiceName,
			Time:    e.Timestamp,
		}, true
	case engine.CredentialsExpired:
		return engine.LogEntry{
			Level:   "error",
			Message: fmt.Sprintf("credentials expired — run: aws sso login --profile %s", e.AWSProfile),
			Profile: e.Profile,
			Service: e.ServiceName,
			Time:    e.Timestamp,
		}, true
	}
	return engine.LogEntry{}, false
}
