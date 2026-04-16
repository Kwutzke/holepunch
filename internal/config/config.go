package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Defaults Defaults           `yaml:"defaults"`
	Profiles map[string]Profile `yaml:"profiles"`
}

type Defaults struct {
	Reconnect          ReconnectConfig `yaml:"reconnect"`
	HealthCheckSeconds int             `yaml:"health_check_interval_seconds"`
}

type ReconnectConfig struct {
	MaxBackoffSeconds int `yaml:"max_backoff_seconds"`
	MaxAttempts       int `yaml:"max_attempts"` // 0 = unlimited
}

type Profile struct {
	AWSProfile string    `yaml:"aws_profile"`
	AWSRegion  string    `yaml:"aws_region"`
	Target     string    `yaml:"target"`
	Services   []Service `yaml:"services"`
}

type Service struct {
	Name         string `yaml:"name"`
	DNSName      string `yaml:"dns_name"`
	RemoteHost   string `yaml:"remote_host"`
	RemotePort   int    `yaml:"remote_port"`
	LocalPort    int    `yaml:"local_port"`
	Sigv4Service string `yaml:"sigv4_service,omitempty"` // e.g. "es" for OpenSearch — enables signing proxy
}

// DefaultConfigPath returns the default config file path.
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".holepunch", "config.yaml")
}

// Load reads and parses the config from the given path.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}
	return Parse(data)
}

// Parse parses config from raw YAML bytes.
func Parse(data []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing config: %w", err)
	}
	applyDefaults(&cfg)
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Defaults.Reconnect.MaxBackoffSeconds == 0 {
		cfg.Defaults.Reconnect.MaxBackoffSeconds = 30
	}
	if cfg.Defaults.HealthCheckSeconds == 0 {
		cfg.Defaults.HealthCheckSeconds = 10
	}
	for profileName, profile := range cfg.Profiles {
		for i := range profile.Services {
			svc := &profile.Services[i]
			if svc.LocalPort == 0 {
				svc.LocalPort = svc.RemotePort
			}
		}
		cfg.Profiles[profileName] = profile
	}
}

func validate(cfg Config) error {
	if len(cfg.Profiles) == 0 {
		return fmt.Errorf("config validation: no profiles defined")
	}

	dnsNames := make(map[string]string) // dns_name -> "profile/service" for error messages

	for profileName, profile := range cfg.Profiles {
		if profile.AWSProfile == "" {
			return fmt.Errorf("config validation: profile %q: aws_profile is required", profileName)
		}
		if profile.AWSRegion == "" {
			return fmt.Errorf("config validation: profile %q: aws_region is required", profileName)
		}
		if profile.Target == "" {
			return fmt.Errorf("config validation: profile %q: target is required", profileName)
		}
		if len(profile.Services) == 0 {
			return fmt.Errorf("config validation: profile %q: no services defined", profileName)
		}

		serviceNames := make(map[string]bool)
		for _, svc := range profile.Services {
			if svc.Name == "" {
				return fmt.Errorf("config validation: profile %q: service name is required", profileName)
			}
			if serviceNames[svc.Name] {
				return fmt.Errorf("config validation: profile %q: duplicate service name %q", profileName, svc.Name)
			}
			serviceNames[svc.Name] = true

			if svc.DNSName == "" {
				return fmt.Errorf("config validation: profile %q, service %q: dns_name is required", profileName, svc.Name)
			}
			qualifiedName := profileName + "/" + svc.Name
			if existing, ok := dnsNames[svc.DNSName]; ok {
				return fmt.Errorf("config validation: duplicate dns_name %q used by %q and %q", svc.DNSName, existing, qualifiedName)
			}
			dnsNames[svc.DNSName] = qualifiedName

			if svc.RemoteHost == "" {
				return fmt.Errorf("config validation: profile %q, service %q: remote_host is required", profileName, svc.Name)
			}
			if svc.RemotePort == 0 {
				return fmt.Errorf("config validation: profile %q, service %q: remote_port is required", profileName, svc.Name)
			}
		}
	}
	return nil
}
