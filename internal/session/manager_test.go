package session_test

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"testing"

	"github.com/Kwutzke/holepunch/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestSSMManager_Wait_CredentialsExpired(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		stderrMsg string
		wantExp   bool
	}{
		{
			"sso token expired",
			"Error loading SSO Token: Token for https://foo.awsapps.com has expired",
			true,
		},
		{
			"expired token exception",
			"An error occurred (ExpiredTokenException) when calling the StartSession operation",
			true,
		},
		{
			"unrelated failure",
			"An error occurred (AccessDenied) when calling the StartSession operation",
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			factory := fakeCLIFactory(t, tt.stderrMsg, 255)
			mgr := session.NewSSMManager(factory)

			sess, err := mgr.Start(t.Context(), session.StartParams{
				Target:     "i-fake",
				RemoteHost: "host.example",
				RemotePort: 443,
				LocalPort:  18443,
			})
			require.NoError(t, err)

			waitErr := sess.Wait()
			require.Error(t, waitErr)

			if tt.wantExp {
				assert.True(t, errors.Is(waitErr, session.ErrCredentialsExpired),
					"expected ErrCredentialsExpired, got %v", waitErr)
			} else {
				assert.False(t, errors.Is(waitErr, session.ErrCredentialsExpired),
					"did not expect ErrCredentialsExpired, got %v", waitErr)
			}
		})
	}
}

// fakeCLIFactory returns a CommandFactory that ignores the requested binary
// and instead spawns `sh -c` writing the given message to stderr and exiting
// with the given code. This lets Wait-path tests exercise real exec.Cmd
// stderr wiring without depending on the AWS CLI being installed.
func fakeCLIFactory(t *testing.T, stderr string, exitCode int) session.CommandFactory {
	t.Helper()
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		script := "printf '%s\n' " + shellQuote(stderr) + " 1>&2; exit " + itoa(exitCode)
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
}

func shellQuote(s string) string {
	// sh single-quote escaping: end quote, escaped single-quote, reopen quote.
	var b []byte
	b = append(b, '\'')
	for _, r := range s {
		if r == '\'' {
			b = append(b, '\'', '\\', '\'', '\'')
			continue
		}
		b = append(b, byte(r))
	}
	b = append(b, '\'')
	return string(b)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
