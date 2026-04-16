package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Kwutzke/holepunch/internal/config"
	"github.com/Kwutzke/holepunch/internal/ip"
	"github.com/Kwutzke/holepunch/internal/resolver"
	"github.com/spf13/cobra"
)

type serviceInfo struct {
	key     string
	dnsName string
	ip      string
}

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "One-time setup: configure DNS resolver and loopback aliases",
	Long: `Sets up two things (requires sudo):

1. DNS routing — configures your system to route queries for configured
   TLDs to holepunch's embedded DNS server.

2. Loopback aliases — creates 127.0.0.x addresses on the loopback interface
   so each service gets its own IP. Persisted across reboots.

Run this again if you add new services or TLDs to your config.`,
	RunE: runSetup,
}

var unsetupCmd = &cobra.Command{
	Use:   "unsetup",
	Short: "Remove all setup artifacts",
	RunE:  runUnsetup,
}

func init() {
	setupCmd.Flags().StringVar(&configPath, "config", config.DefaultConfigPath(), "path to config file")
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(unsetupCmd)
}

func runSetup(_ *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	var hostnames []string
	alloc := ip.New()
	var services []serviceInfo

	for profileName, profile := range cfg.Profiles {
		for _, svc := range profile.Services {
			hostnames = append(hostnames, svc.DNSName)
			key := profileName + "/" + svc.Name
			allocated, err := alloc.Allocate(key)
			if err != nil {
				return fmt.Errorf("allocating IP for %s: %w", key, err)
			}
			services = append(services, serviceInfo{key: key, dnsName: svc.DNSName, ip: allocated.String()})
		}
	}

	tlds := resolver.ExtractTLDs(hostnames)
	if len(tlds) == 0 {
		return fmt.Errorf("no DNS names configured")
	}

	uniqueIPs := make(map[string]bool)
	for _, svc := range services {
		if svc.ip != "127.0.0.1" {
			uniqueIPs[svc.ip] = true
		}
	}

	fmt.Println("holepunch setup")
	fmt.Println()
	fmt.Println("Service mapping:")
	for _, svc := range services {
		fmt.Printf("  %s → %s\n", svc.dnsName, svc.ip)
	}
	fmt.Println()
	fmt.Println("This requires sudo.")
	fmt.Println()

	if err := platformSetup(tlds, uniqueIPs); err != nil {
		return err
	}

	// Save config hash so we can detect changes later.
	if err := config.SaveSetupHash(configPath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save setup hash: %v\n", err)
	}
	return nil
}

func runUnsetup(_ *cobra.Command, _ []string) error {
	fmt.Println("Removing holepunch setup artifacts...")
	return platformUnsetup()
}

func sudoRun(args ...string) error {
	cmd := exec.Command("sudo", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sudoWriteFile(path, content string) error {
	cmd := exec.Command("sudo", "tee", path)
	cmd.Stdin = strings.NewReader(content)
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
