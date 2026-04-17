package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/Kwutzke/holepunch/internal/config"
	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up [target...]",
	Short: "Start port forwarding for the given targets (profile or profile/service), or all",
	Long: "Start port forwarding. Each target is either a profile name (starts all " +
		"services in that profile) or \"profile/service\" (starts just that service). " +
		"With no targets, starts every profile in the config.",
	RunE: runUp,
}

func init() {
	upCmd.Flags().StringVar(&configPath, "config", config.DefaultConfigPath(), "path to config file")
	rootCmd.AddCommand(upCmd)
}

func runUp(_ *cobra.Command, args []string) error {
	// If no profiles given, start all.
	if len(args) == 0 {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		for name := range cfg.Profiles {
			args = append(args, name)
		}
		if len(args) == 0 {
			return fmt.Errorf("no profiles defined in config")
		}
	}

	// Warn if config changed since last setup.
	if config.SetupStale(configPath) {
		fmt.Println("Warning: config has changed since last setup.")
		fmt.Println("Run 'sudo holepunch setup' then 'holepunch restart' to apply changes.")
		fmt.Println()
	}

	client := daemon.NewClient(socketPath)

	if err := ensureDaemon(client); err != nil {
		return err
	}

	resp, err := client.SendCommand(daemon.Request{
		Command: daemon.CmdUp,
		Targets: args,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("daemon error: %s", resp.Error)
	}

	fmt.Printf("Started: %v\n", args)
	fmt.Println("Use 'holepunch status' to check service states.")
	return nil
}

func ensureDaemon(client *daemon.Client) error {
	if client.IsRunning() {
		return nil
	}

	fmt.Println("Starting daemon...")
	if err := launchDaemon(); err != nil {
		return fmt.Errorf("starting daemon: %w", err)
	}

	for range 50 {
		time.Sleep(100 * time.Millisecond)
		if client.IsRunning() {
			return nil
		}
	}
	return fmt.Errorf("daemon failed to start within 5 seconds")
}

func launchDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	cmd := exec.Command(exe, "daemon", "--socket", socketPath, "--config", configPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &sysProcAttr
	return cmd.Start()
}
