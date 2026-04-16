package session_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/Kwutzke/holepunch/internal/session"
)

func TestBuildSSMArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		params         session.StartParams
		expectedArgs   []string
		unexpectedArgs []string
	}{
		{
			"full params",
			session.StartParams{
				AWSProfile: "dev-sso",
				AWSRegion:  "eu-central-1",
				Target:     "i-0abc123",
				RemoteHost: "vpc-os.eu-central-1.es.amazonaws.com",
				RemotePort: 443,
				LocalIP:    net.IPv4(127, 0, 0, 2),
				LocalPort:  443,
			},
			[]string{
				"ssm", "start-session",
				"--target", "i-0abc123",
				"--document-name", "AWS-StartPortForwardingSessionToRemoteHost",
				"--profile", "dev-sso",
				"--region", "eu-central-1",
			},
			nil,
		},
		{
			"without profile and region",
			session.StartParams{
				Target:     "i-0abc123",
				RemoteHost: "host.amazonaws.com",
				RemotePort: 5432,
				LocalIP:    net.IPv4(127, 0, 0, 3),
				LocalPort:  5432,
			},
			[]string{
				"ssm", "start-session",
				"--target", "i-0abc123",
			},
			[]string{"--profile", "--region"},
		},
		{
			"different ports",
			session.StartParams{
				AWSProfile: "prod",
				AWSRegion:  "us-east-1",
				Target:     "i-0def456",
				RemoteHost: "redis.cache.amazonaws.com",
				RemotePort: 6379,
				LocalIP:    net.IPv4(127, 0, 0, 4),
				LocalPort:  16379,
			},
			[]string{
				"--target", "i-0def456",
				"--profile", "prod",
				"--region", "us-east-1",
			},
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := session.BuildSSMArgs(tt.params)

			for _, expected := range tt.expectedArgs {
				assert.Contains(t, args, expected)
			}
			for _, unexpected := range tt.unexpectedArgs {
				assert.NotContains(t, args, unexpected)
			}
		})
	}
}

func TestBuildSSMArgsParameterJSON(t *testing.T) {
	t.Parallel()

	params := session.StartParams{
		Target:     "i-0abc123",
		RemoteHost: "vpc-os.es.amazonaws.com",
		RemotePort: 443,
		LocalPort:  8443,
	}
	args := session.BuildSSMArgs(params)

	// Find the --parameters value.
	var paramJSON string
	for i, arg := range args {
		if arg == "--parameters" && i+1 < len(args) {
			paramJSON = args[i+1]
			break
		}
	}

	assert.Contains(t, paramJSON, `"host":["vpc-os.es.amazonaws.com"]`)
	assert.Contains(t, paramJSON, `"portNumber":["443"]`)
	assert.Contains(t, paramJSON, `"localPortNumber":["8443"]`)
}

func TestNewSSMManager(t *testing.T) {
	t.Parallel()

	// Verify it doesn't panic with nil factory.
	mgr := session.NewSSMManager(nil)
	assert.NotNil(t, mgr)
}
