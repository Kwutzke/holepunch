package dns

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const (
	beginMarker = "# BEGIN holepunch"
	endMarker   = "# END holepunch"
)

// Entry represents a single DNS entry mapping an IP to a hostname.
type Entry struct {
	IP       net.IP
	Hostname string
}

func (e Entry) String() string {
	return fmt.Sprintf("%s %s", e.IP.String(), e.Hostname)
}

// WriteFunc defines how the hosts file content is written.
// The default uses sudo tee to write to /etc/hosts.
type WriteFunc func(ctx context.Context, path string, content []byte) error

// Manager handles DNS entries in a hosts file.
type Manager struct {
	mu        sync.Mutex
	hostsPath string
	writeFunc WriteFunc
}

// New creates a Manager for the given hosts file path.
// If writeFunc is nil, it defaults to writing via sudo tee.
func New(hostsPath string, writeFunc WriteFunc) *Manager {
	if writeFunc == nil {
		writeFunc = sudoWrite
	}
	return &Manager{
		hostsPath: hostsPath,
		writeFunc: writeFunc,
	}
}

// Add ensures the given entries exist in the managed block of the hosts file.
// Existing entries in the managed block are preserved; new ones are appended.
func (m *Manager) Add(ctx context.Context, entries []Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	content, err := os.ReadFile(m.hostsPath)
	if err != nil {
		return fmt.Errorf("reading hosts file: %w", err)
	}

	existing := parseManagedBlock(content)
	merged := mergeEntries(existing, entries)
	newContent := replaceManagedBlock(content, merged)

	return m.writeFunc(ctx, m.hostsPath, newContent)
}

// Remove removes the entire managed block from the hosts file.
func (m *Manager) Remove(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	content, err := os.ReadFile(m.hostsPath)
	if err != nil {
		return fmt.Errorf("reading hosts file: %w", err)
	}

	newContent := replaceManagedBlock(content, nil)

	if bytes.Equal(content, newContent) {
		return nil // nothing to remove
	}

	return m.writeFunc(ctx, m.hostsPath, newContent)
}

// RemoveEntries removes specific entries from the managed block.
func (m *Manager) RemoveEntries(ctx context.Context, entries []Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	content, err := os.ReadFile(m.hostsPath)
	if err != nil {
		return fmt.Errorf("reading hosts file: %w", err)
	}

	existing := parseManagedBlock(content)
	filtered := subtractEntries(existing, entries)
	newContent := replaceManagedBlock(content, filtered)

	return m.writeFunc(ctx, m.hostsPath, newContent)
}

func parseManagedBlock(content []byte) []Entry {
	var entries []Entry
	inBlock := false

	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == beginMarker {
			inBlock = true
			continue
		}
		if line == endMarker {
			break
		}
		if !inBlock || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			ip := net.ParseIP(parts[0])
			if ip != nil {
				entries = append(entries, Entry{IP: ip, Hostname: parts[1]})
			}
		}
	}
	return entries
}

func mergeEntries(existing, additions []Entry) []Entry {
	seen := make(map[string]bool)
	var result []Entry

	for _, e := range existing {
		key := e.Hostname
		if !seen[key] {
			seen[key] = true
			result = append(result, e)
		}
	}
	for _, e := range additions {
		key := e.Hostname
		if !seen[key] {
			seen[key] = true
			result = append(result, e)
		}
	}
	return result
}

func subtractEntries(existing, removals []Entry) []Entry {
	removeSet := make(map[string]bool)
	for _, e := range removals {
		removeSet[e.Hostname] = true
	}
	var result []Entry
	for _, e := range existing {
		if !removeSet[e.Hostname] {
			result = append(result, e)
		}
	}
	return result
}

func replaceManagedBlock(content []byte, entries []Entry) []byte {
	var before, after []byte
	inBlock := false
	foundBlock := false

	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == beginMarker {
			inBlock = true
			foundBlock = true
			continue
		}
		if trimmed == endMarker {
			inBlock = false
			continue
		}
		if inBlock {
			continue
		}

		if foundBlock {
			after = append(after, []byte(line+"\n")...)
		} else {
			before = append(before, []byte(line+"\n")...)
		}
	}

	if len(entries) == 0 {
		result := append(before, after...)
		// Remove trailing extra newlines from block removal.
		return bytes.TrimRight(result, "\n")
	}

	var block bytes.Buffer
	block.WriteString(beginMarker + "\n")
	for _, e := range entries {
		block.WriteString(e.String() + "\n")
	}
	block.WriteString(endMarker + "\n")

	var result bytes.Buffer
	result.Write(before)
	result.Write(block.Bytes())
	result.Write(after)

	return result.Bytes()
}

func sudoWrite(ctx context.Context, path string, content []byte) error {
	cmd := exec.CommandContext(ctx, "sudo", "tee", path)
	cmd.Stdin = bytes.NewReader(content)
	cmd.Stdout = nil // suppress tee's stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("writing hosts file via sudo: %w", err)
	}
	return nil
}
