package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/Kwutzke/holepunch/internal/config"
)

func TestParseValidConfig(t *testing.T) {
	t.Parallel()

	yaml := `
profiles:
  dev:
    aws_profile: dev-sso
    aws_region: eu-central-1
    target: i-0abc123
    services:
      - name: opensearch
        dns_name: opensearch.dev
        remote_host: vpc-os.eu-central-1.es.amazonaws.com
        remote_port: 443
      - name: rds
        dns_name: rds.dev
        remote_host: mydb.cluster-xyz.eu-central-1.rds.amazonaws.com
        remote_port: 5432
        local_port: 15432
  prod:
    aws_profile: prod-sso
    aws_region: eu-central-1
    target: i-0def456
    services:
      - name: opensearch
        dns_name: opensearch.prod
        remote_host: vpc-os-prod.eu-central-1.es.amazonaws.com
        remote_port: 443
`

	cfg, err := config.Parse([]byte(yaml))
	require.NoError(t, err)

	assert.Len(t, cfg.Profiles, 2)

	dev := cfg.Profiles["dev"]
	assert.Equal(t, "dev-sso", dev.AWSProfile)
	assert.Equal(t, "eu-central-1", dev.AWSRegion)
	assert.Equal(t, "i-0abc123", dev.Target)
	assert.Len(t, dev.Services, 2)

	os := dev.Services[0]
	assert.Equal(t, "opensearch", os.Name)
	assert.Equal(t, "opensearch.dev", os.DNSName)
	assert.Equal(t, 443, os.RemotePort)
	assert.Equal(t, 443, os.LocalPort, "local_port should default to remote_port")

	rds := dev.Services[1]
	assert.Equal(t, 15432, rds.LocalPort, "explicit local_port should be preserved")
}

func TestParseDefaults(t *testing.T) {
	t.Parallel()

	yaml := `
profiles:
  dev:
    aws_profile: dev
    aws_region: us-east-1
    target: i-123
    services:
      - name: redis
        dns_name: redis.dev
        remote_host: redis.cache.amazonaws.com
        remote_port: 6379
`

	cfg, err := config.Parse([]byte(yaml))
	require.NoError(t, err)

	assert.Equal(t, 30, cfg.Defaults.Reconnect.MaxBackoffSeconds)
	assert.Equal(t, 10, cfg.Defaults.HealthCheckSeconds)
	assert.Equal(t, 0, cfg.Defaults.Reconnect.MaxAttempts, "default max_attempts should be 0 (unlimited)")
}

func TestParseCustomDefaults(t *testing.T) {
	t.Parallel()

	yaml := `
defaults:
  reconnect:
    max_backoff_seconds: 60
    max_attempts: 10
  health_check_interval_seconds: 5
profiles:
  dev:
    aws_profile: dev
    aws_region: us-east-1
    target: i-123
    services:
      - name: svc
        dns_name: svc.dev
        remote_host: host.amazonaws.com
        remote_port: 443
`

	cfg, err := config.Parse([]byte(yaml))
	require.NoError(t, err)

	assert.Equal(t, 60, cfg.Defaults.Reconnect.MaxBackoffSeconds)
	assert.Equal(t, 10, cfg.Defaults.Reconnect.MaxAttempts)
	assert.Equal(t, 5, cfg.Defaults.HealthCheckSeconds)
}

func TestParseValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		yaml        string
		errContains string
	}{
		{
			"no profiles",
			`profiles: {}`,
			"no profiles defined",
		},
		{
			"missing aws_profile",
			`
profiles:
  dev:
    aws_region: us-east-1
    target: i-123
    services:
      - name: svc
        dns_name: svc.dev
        remote_host: host
        remote_port: 443`,
			"aws_profile is required",
		},
		{
			"missing aws_region",
			`
profiles:
  dev:
    aws_profile: dev
    target: i-123
    services:
      - name: svc
        dns_name: svc.dev
        remote_host: host
        remote_port: 443`,
			"aws_region is required",
		},
		{
			"missing target",
			`
profiles:
  dev:
    aws_profile: dev
    aws_region: us-east-1
    services:
      - name: svc
        dns_name: svc.dev
        remote_host: host
        remote_port: 443`,
			"target is required",
		},
		{
			"no services",
			`
profiles:
  dev:
    aws_profile: dev
    aws_region: us-east-1
    target: i-123
    services: []`,
			"no services defined",
		},
		{
			"missing service name",
			`
profiles:
  dev:
    aws_profile: dev
    aws_region: us-east-1
    target: i-123
    services:
      - dns_name: svc.dev
        remote_host: host
        remote_port: 443`,
			"service name is required",
		},
		{
			"missing dns_name",
			`
profiles:
  dev:
    aws_profile: dev
    aws_region: us-east-1
    target: i-123
    services:
      - name: svc
        remote_host: host
        remote_port: 443`,
			"dns_name is required",
		},
		{
			"missing remote_host",
			`
profiles:
  dev:
    aws_profile: dev
    aws_region: us-east-1
    target: i-123
    services:
      - name: svc
        dns_name: svc.dev
        remote_port: 443`,
			"remote_host is required",
		},
		{
			"missing remote_port",
			`
profiles:
  dev:
    aws_profile: dev
    aws_region: us-east-1
    target: i-123
    services:
      - name: svc
        dns_name: svc.dev
        remote_host: host`,
			"remote_port is required",
		},
		{
			"duplicate service name",
			`
profiles:
  dev:
    aws_profile: dev
    aws_region: us-east-1
    target: i-123
    services:
      - name: svc
        dns_name: svc1.dev
        remote_host: host1
        remote_port: 443
      - name: svc
        dns_name: svc2.dev
        remote_host: host2
        remote_port: 443`,
			"duplicate service name",
		},
		{
			"duplicate dns_name across profiles",
			`
profiles:
  dev:
    aws_profile: dev
    aws_region: us-east-1
    target: i-123
    services:
      - name: svc1
        dns_name: same.dns
        remote_host: host1
        remote_port: 443
  prod:
    aws_profile: prod
    aws_region: us-east-1
    target: i-456
    services:
      - name: svc2
        dns_name: same.dns
        remote_host: host2
        remote_port: 443`,
			"duplicate dns_name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse([]byte(tt.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContains)
		})
	}
}

func TestParseInvalidYAML(t *testing.T) {
	t.Parallel()

	_, err := config.Parse([]byte(`{invalid yaml`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing config")
}

func TestLoad(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yaml := `
profiles:
  dev:
    aws_profile: dev
    aws_region: us-east-1
    target: i-123
    services:
      - name: svc
        dns_name: svc.dev
        remote_host: host
        remote_port: 443
`
	err := os.WriteFile(path, []byte(yaml), 0o644)
	require.NoError(t, err)

	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Len(t, cfg.Profiles, 1)
}

func TestLoadFileNotFound(t *testing.T) {
	t.Parallel()

	_, err := config.Load("/nonexistent/config.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading config")
}
