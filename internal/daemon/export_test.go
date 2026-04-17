package daemon

import "net"

// ExportedWriteMessage exposes writeMessage for testing.
func ExportedWriteMessage(conn net.Conn, msg any) error {
	return writeMessage(conn, msg)
}

// ExportedReadMessage exposes readMessage for testing.
func ExportedReadMessage(conn net.Conn, msg any) error {
	return readMessage(conn, msg)
}

// HasTrayClient reports whether the server currently has any tray
// registrations — used in tests to verify the counter increments/decrements
// around CmdTrayRegister connections.
func (s *Server) HasTrayClient() bool {
	return s.hasTrayClient.Load() > 0
}
