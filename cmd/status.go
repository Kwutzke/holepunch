package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show status of all active services",
	RunE:  runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(_ *cobra.Command, _ []string) error {
	client := daemon.NewClient(socketPath)

	if !client.IsRunning() {
		fmt.Println("Daemon is not running.")
		return nil
	}

	resp, err := client.SendCommand(daemon.Request{Command: daemon.CmdStatus})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("daemon error: %s", resp.Error)
	}

	if len(resp.Statuses) == 0 {
		fmt.Println("No services configured.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROFILE\tSERVICE\tDNS\tADDRESS\tSTATE\tUPTIME\tERROR")
	for _, s := range resp.Statuses {
		stateIcon := stateIcon(s.State)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s %s\t%s\t%s\n",
			s.Profile, s.ServiceName, s.DNSName, s.LocalAddr,
			stateIcon, s.State, s.ConnectedAt, s.Error)
	}
	w.Flush()
	return nil
}

func stateIcon(state string) string {
	switch state {
	case "Connected":
		return "●"
	case "Reconnecting":
		return "↻"
	case "Starting":
		return "◌"
	case "Failed":
		return "✗"
	case "Stopping":
		return "◌"
	default:
		return "○"
	}
}
