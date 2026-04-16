package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/Kwutzke/holepunch/internal/daemon"
)

var socketPath string

var rootCmd = &cobra.Command{
	Use:   "holepunch",
	Short: "AWS port forwarding manager with DNS entries",
	Long:  "Manages SSM port-forwarding sessions to AWS services with local DNS names and auto-reconnect.",
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&socketPath, "socket", daemon.DefaultSocketPath(), "path to daemon unix socket")
}
