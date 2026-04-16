package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/Kwutzke/holepunch/internal/config"
	"github.com/Kwutzke/holepunch/internal/ip"
	"github.com/Kwutzke/holepunch/internal/resolver"
)

const (
	resolverDir    = "/etc/resolver"
	launchDaemonID = "com.holepunch.loopback"
	launchPlistDir = "/Library/LaunchDaemons"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "One-time setup: configure DNS resolver and loopback aliases",
	Long: `Sets up two things (requires sudo):

1. /etc/resolver/ files — routes DNS queries for your configured TLDs
   to the holepunch daemon's embedded DNS server.

2. Loopback aliases — creates 127.0.0.x addresses on lo0 so each service
   gets its own IP. A LaunchDaemon is installed so aliases survive reboots.

Run this again if you add new services or TLDs to your config.`,
	RunE: runSetup,
}

var unsetupCmd = &cobra.Command{
	Use:   "unsetup",
	Short: "Remove all setup artifacts (resolver files, loopback aliases, LaunchDaemon)",
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

	// Collect DNS names and compute IPs.
	var hostnames []string
	alloc := ip.New()
	type serviceInfo struct {
		key     string
		dnsName string
		ip      string
	}
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

	// Collect unique IPs for loopback aliases.
	uniqueIPs := make(map[string]bool)
	for _, svc := range services {
		if svc.ip != "127.0.0.1" {
			uniqueIPs[svc.ip] = true
		}
	}

	fmt.Println("holepunch setup")
	fmt.Println()
	fmt.Println("DNS resolver files:")
	for _, tld := range tlds {
		fmt.Printf("  /etc/resolver/%s → 127.0.0.1:15353\n", tld)
	}
	fmt.Println()
	fmt.Println("Loopback aliases:")
	for ipAddr := range uniqueIPs {
		fmt.Printf("  %s on lo0\n", ipAddr)
	}
	fmt.Println()
	fmt.Println("Service mapping:")
	for _, svc := range services {
		fmt.Printf("  %s → %s\n", svc.dnsName, svc.ip)
	}
	fmt.Println()
	fmt.Println("This requires sudo.")
	fmt.Println()

	// 1. Create resolver files.
	resolverContent := fmt.Sprintf("# Managed by holepunch\nnameserver 127.0.0.1\nport %d\n", 15353)

	if err := sudoRun("mkdir", "-p", resolverDir); err != nil {
		return fmt.Errorf("creating %s: %w", resolverDir, err)
	}

	for _, tld := range tlds {
		path := filepath.Join(resolverDir, tld)
		if err := sudoWriteFile(path, resolverContent); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		fmt.Printf("  Created %s\n", path)
	}

	// 2. Create loopback aliases now.
	for ipAddr := range uniqueIPs {
		if err := sudoRun("ifconfig", "lo0", "alias", ipAddr); err != nil {
			return fmt.Errorf("creating loopback alias %s: %w", ipAddr, err)
		}
		fmt.Printf("  Created loopback alias %s\n", ipAddr)
	}

	// 3. Install LaunchDaemon for persistence across reboots.
	plistContent := buildLaunchDaemonPlist(uniqueIPs)
	plistPath := filepath.Join(launchPlistDir, launchDaemonID+".plist")
	if err := sudoWriteFile(plistPath, plistContent); err != nil {
		return fmt.Errorf("writing LaunchDaemon: %w", err)
	}
	sudoRun("launchctl", "load", plistPath)
	fmt.Printf("  Installed LaunchDaemon %s\n", plistPath)

	fmt.Println()
	fmt.Println("Setup complete. Run: holepunch up <profile>")
	return nil
}

func runUnsetup(_ *cobra.Command, _ []string) error {
	fmt.Println("Removing holepunch setup artifacts...")

	// Remove resolver files.
	entries, _ := os.ReadDir(resolverDir)
	for _, entry := range entries {
		path := filepath.Join(resolverDir, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(content), "holepunch") {
			sudoRun("rm", path)
			fmt.Printf("  Removed %s\n", path)
		}
	}

	// Remove LaunchDaemon.
	plistPath := filepath.Join(launchPlistDir, launchDaemonID+".plist")
	if _, err := os.Stat(plistPath); err == nil {
		sudoRun("launchctl", "unload", plistPath)
		sudoRun("rm", plistPath)
		fmt.Printf("  Removed %s\n", plistPath)
	}

	// Remove loopback aliases (they'll be gone on next reboot anyway).
	// We don't track which IPs were created, so skip this.
	fmt.Println()
	fmt.Println("Done. Loopback aliases will be removed on next reboot.")
	return nil
}

func buildLaunchDaemonPlist(ips map[string]bool) string {
	var args []string
	for ipAddr := range ips {
		args = append(args, fmt.Sprintf(
			"    <string>/sbin/ifconfig</string>\n    <string>lo0</string>\n    <string>alias</string>\n    <string>%s</string>",
			ipAddr))
	}

	// Use a shell script approach since we need multiple ifconfig calls.
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	for ipAddr := range ips {
		script.WriteString(fmt.Sprintf("/sbin/ifconfig lo0 alias %s\n", ipAddr))
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/sh</string>
        <string>-c</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>
`, launchDaemonID, escapeXML(script.String()))
}

func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
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
