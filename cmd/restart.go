package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/Kwutzke/holepunch/internal/config"
	"github.com/Kwutzke/holepunch/internal/daemon"
)

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the daemon and reconnect all profiles",
	RunE:  runRestart,
}

func init() {
	restartCmd.Flags().StringVar(&configPath, "config", config.DefaultConfigPath(), "path to config file")
	rootCmd.AddCommand(restartCmd)
}

func runRestart(_ *cobra.Command, _ []string) error {
	client := daemon.NewClient(socketPath)

	// Stop existing daemon if running.
	if client.IsRunning() {
		fmt.Println("Stopping daemon...")

		// Stop all profiles.
		client.SendCommand(daemon.Request{Command: daemon.CmdDown})

		// Kill daemon process.
		pidPath := daemon.DefaultPIDPath()
		if pid, err := daemon.ReadPIDFile(pidPath); err == nil {
			if proc, err := os.FindProcess(pid); err == nil {
				proc.Signal(os.Interrupt)
			}
		}

		// Wait for daemon to exit.
		for range 30 {
			time.Sleep(100 * time.Millisecond)
			if !client.IsRunning() {
				break
			}
		}
	}

	// Start fresh with all profiles.
	fmt.Println("Starting daemon...")
	return runUp(nil, nil)
}
