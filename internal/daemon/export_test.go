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
