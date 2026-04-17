//go:build !darwin

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

var trayCmd = &cobra.Command{
	Use:   "tray",
	Short: "Run the macOS menu bar UI (macOS only)",
	RunE: func(_ *cobra.Command, _ []string) error {
		return errors.New("holepunch tray is macOS-only")
	},
}

func init() {
	rootCmd.AddCommand(trayCmd)
}
