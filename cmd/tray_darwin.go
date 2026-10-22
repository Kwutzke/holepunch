//go:build darwin

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/Kwutzke/holepunch/internal/tray"
)

var trayForeground bool

var trayCmd = &cobra.Command{
	Use:   "tray",
	Short: "Run the macOS menu bar UI",
	Long: "Launches a menu bar tray that reflects service state and surfaces " +
		"credential-expiry as a clickable login action. Ensures the daemon " +
		"is running, then detaches so the shell returns while the tray keeps running.",
	RunE: runTray,
}

func init() {
	trayCmd.Flags().BoolVar(&trayForeground, "foreground", false, "run the tray in this process instead of detaching (internal)")
	_ = trayCmd.Flags().MarkHidden("foreground")
	rootCmd.AddCommand(trayCmd)
}

func runTray(_ *cobra.Command, _ []string) error {
	if trayForeground {
		// NSApplicationMain must own the main OS thread. Locking here is enough only while
		// main.main starts no goroutine before cobra dispatches to this RunE.
		runtime.LockOSThread()
		tray.Run(socketPath)
		return nil
	}

	client := daemon.NewClient(socketPath)
	if err := ensureDaemon(client); err != nil {
		return err
	}
	return launchTray()
}

func launchTray() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving tray executable path: %w", err)
	}
	cmd := exec.Command(exe, "tray", "--foreground", "--socket", socketPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &sysProcAttr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting tray: %w", err)
	}
	fmt.Println("Tray started.")
	return nil
}
