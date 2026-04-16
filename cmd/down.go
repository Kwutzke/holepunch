package cmd

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/Kwutzke/holepunch/internal/daemon"
)

var forceKill bool

var downCmd = &cobra.Command{
	Use:   "down [profile...]",
	Short: "Stop port forwarding for the given profiles, or stop all and kill daemon",
	RunE:  runDown,
}

func init() {
	downCmd.Flags().BoolVarP(&forceKill, "force", "f", false, "force kill the daemon (SIGKILL)")
	rootCmd.AddCommand(downCmd)
}

func runDown(_ *cobra.Command, args []string) error {
	client := daemon.NewClient(socketPath)
	pidPath := daemon.DefaultPIDPath()

	if forceKill {
		return killDaemon(pidPath, true)
	}

	if !client.IsRunning() {
		// Check for orphaned process.
		if pid, err := daemon.ReadPIDFile(pidPath); err == nil {
			fmt.Printf("Daemon unresponsive (PID %d). Use -f to force kill.\n", pid)
			return nil
		}
		fmt.Println("Daemon is not running.")
		return nil
	}

	// Stop the requested profiles (or all).
	resp, err := client.SendCommand(daemon.Request{
		Command:  daemon.CmdDown,
		Profiles: args,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("daemon error: %s", resp.Error)
	}

	if len(args) > 0 {
		fmt.Printf("Stopped profiles: %v\n", args)
		return nil
	}

	// No args = stop everything and kill the daemon.
	fmt.Println("All profiles stopped. Shutting down daemon...")
	return killDaemon(pidPath, false)
}

func killDaemon(pidPath string, force bool) error {
	pid, err := daemon.ReadPIDFile(pidPath)
	if err != nil {
		daemon.Cleanup(socketPath, pidPath)
		fmt.Println("Daemon stopped.")
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		daemon.Cleanup(socketPath, pidPath)
		fmt.Println("Daemon stopped.")
		return nil
	}

	if force {
		fmt.Printf("Force killing daemon (PID %d)...\n", pid)
		proc.Signal(syscall.SIGKILL)
		daemon.Cleanup(socketPath, pidPath)
		fmt.Println("Daemon killed.")
		return nil
	}

	proc.Signal(os.Interrupt)
	for range 30 {
		time.Sleep(100 * time.Millisecond)
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			daemon.Cleanup(socketPath, pidPath)
			fmt.Println("Daemon stopped.")
			return nil
		}
	}

	fmt.Println("Daemon did not stop in time. Use -f to force kill.")
	return nil
}
