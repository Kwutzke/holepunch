//go:build darwin

package cmd

import (
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/Kwutzke/holepunch/internal/tray"
)

var trayCmd = &cobra.Command{
	Use:   "tray",
	Short: "Run the macOS menu bar UI",
	Long: "Launches a menu bar tray that reflects service state and surfaces " +
		"credential-expiry as a clickable login action. Ensures the daemon " +
		"is running. Blocks until quit via the tray menu.",
	RunE: runTray,
}

func init() {
	rootCmd.AddCommand(trayCmd)
}

func runTray(_ *cobra.Command, _ []string) error {
	// systray.Run uses NSApplicationMain on darwin, which must own the
	// main OS thread. cobra's RunE runs on the main goroutine (see main.go
	// — no pre-spawned goroutines), so pinning here is sufficient.
	runtime.LockOSThread()

	client := daemon.NewClient(socketPath)
	if err := ensureDaemon(client); err != nil {
		return err
	}

	tray.Run(socketPath)
	return nil
}
