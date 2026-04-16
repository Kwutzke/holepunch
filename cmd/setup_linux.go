package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const systemdUnitName = "holepunch-loopback.service"

func platformSetup(tlds []string, uniqueIPs map[string]bool) error {
	// 1. Configure systemd-resolved for per-TLD DNS routing.
	if hasSystemdResolved() {
		for _, tld := range tlds {
			if err := sudoRun("resolvectl", "dns", "lo", "127.0.0.1"); err != nil {
				return fmt.Errorf("configuring resolvectl dns: %w", err)
			}
			if err := sudoRun("resolvectl", "domain", "lo", "~"+tld); err != nil {
				return fmt.Errorf("configuring resolvectl domain for %s: %w", tld, err)
			}
			fmt.Printf("  Configured systemd-resolved for ~%s → 127.0.0.1:15353\n", tld)
		}

		// Make DNS port config persistent via a drop-in.
		dropIn := "[Resolve]\nDNS=127.0.0.1:15353\n"
		domains := make([]string, len(tlds))
		for i, tld := range tlds {
			domains[i] = "~" + tld
		}
		dropIn += "Domains=" + strings.Join(domains, " ") + "\n"

		dropInDir := "/etc/systemd/resolved.conf.d"
		if err := sudoRun("mkdir", "-p", dropInDir); err != nil {
			return fmt.Errorf("creating %s: %w", dropInDir, err)
		}
		dropInPath := dropInDir + "/holepunch.conf"
		if err := sudoWriteFile(dropInPath, dropIn); err != nil {
			return fmt.Errorf("writing resolved drop-in: %w", err)
		}
		sudoRun("systemctl", "restart", "systemd-resolved")
		fmt.Printf("  Installed resolved drop-in %s\n", dropInPath)
	} else {
		return fmt.Errorf("systemd-resolved not found. Manual DNS configuration required.\nAdd 'nameserver 127.0.0.1' and 'port 15353' to your DNS config for TLDs: %s", strings.Join(tlds, ", "))
	}

	// 2. Create loopback aliases on lo.
	for ipAddr := range uniqueIPs {
		// Check if already assigned.
		if err := exec.Command("ip", "addr", "show", "dev", "lo").Run(); err == nil {
			out, _ := exec.Command("ip", "addr", "show", "dev", "lo").Output()
			if strings.Contains(string(out), ipAddr) {
				fmt.Printf("  Loopback alias %s already exists\n", ipAddr)
				continue
			}
		}
		if err := sudoRun("ip", "addr", "add", ipAddr+"/8", "dev", "lo"); err != nil {
			return fmt.Errorf("creating loopback alias %s: %w", ipAddr, err)
		}
		fmt.Printf("  Created loopback alias %s\n", ipAddr)
	}

	// 3. Install systemd service for boot persistence.
	unitContent := buildSystemdUnit(uniqueIPs)
	unitPath := "/etc/systemd/system/" + systemdUnitName
	if err := sudoWriteFile(unitPath, unitContent); err != nil {
		return fmt.Errorf("writing systemd unit: %w", err)
	}
	sudoRun("systemctl", "daemon-reload")
	sudoRun("systemctl", "enable", systemdUnitName)
	fmt.Printf("  Installed systemd unit %s\n", unitPath)

	fmt.Println()
	fmt.Println("Setup complete. Run: holepunch up <profile>")
	return nil
}

func platformUnsetup() error {
	// Remove systemd-resolved drop-in.
	dropInPath := "/etc/systemd/resolved.conf.d/holepunch.conf"
	if _, err := os.Stat(dropInPath); err == nil {
		sudoRun("rm", dropInPath)
		sudoRun("systemctl", "restart", "systemd-resolved")
		fmt.Printf("  Removed %s\n", dropInPath)
	}

	// Remove systemd unit.
	unitPath := "/etc/systemd/system/" + systemdUnitName
	if _, err := os.Stat(unitPath); err == nil {
		sudoRun("systemctl", "disable", systemdUnitName)
		sudoRun("rm", unitPath)
		sudoRun("systemctl", "daemon-reload")
		fmt.Printf("  Removed %s\n", unitPath)
	}

	fmt.Println()
	fmt.Println("Done. Loopback aliases will be removed on next reboot.")
	return nil
}

func buildSystemdUnit(ips map[string]bool) string {
	var commands []string
	for ipAddr := range ips {
		commands = append(commands, fmt.Sprintf("/sbin/ip addr add %s/8 dev lo || true", ipAddr))
	}

	return fmt.Sprintf(`[Unit]
Description=holepunch loopback aliases
After=network.target

[Service]
Type=oneshot
RemainAfterExit=yes
%s

[Install]
WantedBy=multi-user.target
`, formatExecStartLines(commands))
}

func formatExecStartLines(commands []string) string {
	var lines []string
	for _, cmd := range commands {
		lines = append(lines, "ExecStart="+cmd)
	}
	return strings.Join(lines, "\n")
}

func hasSystemdResolved() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "systemd-resolved").Run() == nil
}
