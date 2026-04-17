//go:build !darwin

package daemon

import "context"

// defaultOsascript is a no-op on non-darwin platforms. The daemon still
// emits CredentialsExpired events (consumed by the tray and logs stream)
// but no native OS notification is fired.
func defaultOsascript(_ context.Context, _, _ string) error {
	return nil
}
