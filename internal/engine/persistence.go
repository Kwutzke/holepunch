package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Kwutzke/holepunch/internal/state"
)

// PersistedState represents the serializable state for crash recovery.
type PersistedState struct {
	Services  []PersistedService `json:"services"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// PersistedService represents a single service's persisted state.
type PersistedService struct {
	Profile     string `json:"profile"`
	ServiceName string `json:"service_name"`
	DNSName     string `json:"dns_name"`
	LocalAddr   string `json:"local_addr"`
	LocalIP     string `json:"local_ip"`
	State       string `json:"state"`
	PID         int    `json:"pid,omitempty"`
	ConnectedAt string `json:"connected_at,omitempty"`
	Error       string `json:"error,omitempty"`
}

// StatePersister writes engine state to disk on every change.
type StatePersister struct {
	mu   sync.Mutex
	path string
}

// NewStatePersister creates a persister that writes to the given path.
func NewStatePersister(path string) *StatePersister {
	return &StatePersister{path: path}
}

// DefaultStatePath returns the default state file path.
func DefaultStatePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".holepunch", "state.json")
}

// Save writes the current engine state to disk.
func (p *StatePersister) Save(statuses []ServiceStatus) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	persisted := PersistedState{
		Services:  make([]PersistedService, 0, len(statuses)),
		UpdatedAt: time.Now(),
	}

	for _, s := range statuses {
		ps := PersistedService{
			Profile:     s.Profile,
			ServiceName: s.ServiceName,
			DNSName:     s.DNSName,
			LocalAddr:   s.LocalAddr,
			State:       s.State.String(),
		}
		if s.State == state.Connected && !s.ConnectedAt.IsZero() {
			ps.ConnectedAt = s.ConnectedAt.Format(time.RFC3339)
		}
		if s.Error != nil {
			ps.Error = s.Error.Error()
		}
		persisted.Services = append(persisted.Services, ps)
	}

	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	// Write atomically via temp file + rename.
	tmpPath := p.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}
	if err := os.Rename(tmpPath, p.path); err != nil {
		return fmt.Errorf("renaming state file: %w", err)
	}

	return nil
}

// Load reads persisted state from disk.
func (p *StatePersister) Load() (PersistedState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	data, err := os.ReadFile(p.path)
	if err != nil {
		return PersistedState{}, fmt.Errorf("reading state: %w", err)
	}

	var persisted PersistedState
	if err := json.Unmarshal(data, &persisted); err != nil {
		return PersistedState{}, fmt.Errorf("parsing state: %w", err)
	}

	return persisted, nil
}

// Remove deletes the state file.
func (p *StatePersister) Remove() error {
	if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
