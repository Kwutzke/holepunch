//go:build darwin

package daemon

import (
	"context"
	"os/exec"
	"strings"
)

// defaultOsascript fires a native macOS notification via `osascript`.
// Escapes both title and message so they cannot break out of the
// AppleScript string literal.
func defaultOsascript(ctx context.Context, title, message string) error {
	script := `display notification "` + escapeAppleScript(message) +
		`" with title "` + escapeAppleScript(title) + `"`
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	return cmd.Run()
}

// escapeAppleScript escapes double quotes and backslashes for AppleScript
// string literals.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
