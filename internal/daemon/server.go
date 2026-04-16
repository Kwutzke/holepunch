package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/Kwutzke/holepunch/internal/engine"
	"github.com/Kwutzke/holepunch/internal/state"
)

// Server listens on a unix socket and dispatches commands to the engine.
type Server struct {
	eng      *engine.Engine
	listener net.Listener
	wg       sync.WaitGroup
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

	return &Server{
		eng:      eng,
		listener: listener,
	}, nil
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
	default:
		writeMessage(conn, Response{Error: fmt.Sprintf("unknown command: %s", req.Command)})
	}
}

func (s *Server) handleUp(ctx context.Context, conn net.Conn, req Request) {
	if err := s.eng.Start(ctx, req.Profiles); err != nil {
		writeMessage(conn, Response{Error: err.Error()})
		return
	}
	writeMessage(conn, Response{OK: true})
}

func (s *Server) handleDown(ctx context.Context, conn net.Conn, req Request) {
	if err := s.eng.Stop(ctx, req.Profiles); err != nil {
		writeMessage(conn, Response{Error: err.Error()})
		return
	}
	writeMessage(conn, Response{OK: true})
}

func (s *Server) handleStatus(conn net.Conn) {
	statuses := s.eng.Status()
	entries := make([]StatusEntry, 0, len(statuses))
	for _, st := range statuses {
		entry := StatusEntry{
			Profile:     st.Profile,
			ServiceName: st.ServiceName,
			DNSName:     st.DNSName,
			LocalAddr:   st.LocalAddr,
			State:       st.State.String(),
		}
		if st.State == state.Connected && !st.ConnectedAt.IsZero() {
			entry.ConnectedAt = time.Since(st.ConnectedAt).Truncate(time.Second).String()
		}
		if st.Error != nil {
			entry.Error = st.Error.Error()
		}
		entries = append(entries, entry)
	}
	writeMessage(conn, Response{OK: true, Statuses: entries})
}

func (s *Server) handleLogs(ctx context.Context, conn net.Conn, req Request) {
	events := s.eng.Events()

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
			logEntry, isLog := evt.(engine.LogEntry)
			if !isLog {
				// Convert state changes to log entries too.
				if sc, isSC := evt.(engine.ServiceStateChanged); isSC {
					msg := fmt.Sprintf("%s/%s: %s -> %s", sc.Profile, sc.ServiceName, sc.From, sc.To)
					if sc.Error != nil {
						msg += fmt.Sprintf(" (%v)", sc.Error)
					}
					logEntry = engine.LogEntry{
						Level:   "info",
						Message: msg,
						Profile: sc.Profile,
						Service: sc.ServiceName,
						Time:    sc.Timestamp,
					}
				} else {
					continue
				}
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
