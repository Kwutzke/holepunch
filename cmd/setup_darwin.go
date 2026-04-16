package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	resolverDir    = "/etc/resolver"
	launchDaemonID = "com.holepunch.loopback"
	launchPlistDir = "/Library/LaunchDaemons"
)

func platformSetup(tlds []string, uniqueIPs map[string]bool) error {
	// 1. Create /etc/resolver/ files for DNS routing.
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

	// 2. Create loopback aliases on lo0.
	for ipAddr := range uniqueIPs {
		if err := sudoRun("ifconfig", "lo0", "alias", ipAddr); err != nil {
			return fmt.Errorf("creating loopback alias %s: %w", ipAddr, err)
		}
		fmt.Printf("  Created loopback alias %s\n", ipAddr)
	}

	// 3. Install LaunchDaemon so aliases survive reboots.
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

func platformUnsetup() error {
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

	fmt.Println()
	fmt.Println("Done. Loopback aliases will be removed on next reboot.")
	return nil
}

func buildLaunchDaemonPlist(ips map[string]bool) string {
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	for ipAddr := range ips {
		fmt.Fprintf(&script, "/sbin/ifconfig lo0 alias %s\n", ipAddr)
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
