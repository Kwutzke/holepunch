package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/Kwutzke/holepunch/internal/daemon"
)

var followLogs bool

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show daemon log output",
	RunE:  runLogs,
}

func init() {
	logsCmd.Flags().BoolVarP(&followLogs, "follow", "f", false, "follow log output")
	rootCmd.AddCommand(logsCmd)
}

func runLogs(_ *cobra.Command, _ []string) error {
	client := daemon.NewClient(socketPath)

	if !client.IsRunning() {
		fmt.Println("Daemon is not running.")
		return nil
	}

	// Handle Ctrl-C gracefully.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		os.Exit(0)
	}()

	return client.StreamLogs(
		daemon.Request{Command: daemon.CmdLogs, Follow: followLogs},
		func(resp daemon.Response) bool {
			if resp.Event != nil {
				fmt.Printf("[%s] [%s] [%s/%s] %s\n",
					resp.Event.Time.Format("15:04:05"),
					resp.Event.Level,
					resp.Event.Profile,
					resp.Event.Service,
					resp.Event.Message,
				)
			}
			return true
		},
	)
}
