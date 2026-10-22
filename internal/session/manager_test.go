package session_test

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Kwutzke/holepunch/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The aws stand-ins spawn a never-exiting child, the way aws spawns session-manager-plugin, and
// write the child's PID to the file named by $0.
const (
	neverExitingStandIn     = `sleep 300 & echo $! > "$0"; wait`
	termIgnoringStandIn     = `trap '' TERM; sleep 300 & echo $! > "$0"; wait`
	exitsLeavingChildBehind = `trap '' TERM; sleep 300 & echo $! > "$0"; exit 0`
)

func TestBuildSSMArgs(t *testing.T) {
	t.Parallel()

	t.Run("arguments", func(t *testing.T) {
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
	})

	t.Run("parameter JSON", func(t *testing.T) {
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
	})
}

func TestNewSSMManager(t *testing.T) {
	t.Parallel()

	// Verify it doesn't panic with nil factory.
	mgr := session.NewSSMManager(nil)
	assert.NotNil(t, mgr)
}

func TestSSMManager_Start(t *testing.T) {
	t.Parallel()

	t.Run("Wait classifies expired credentials", func(t *testing.T) {
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
	})

	t.Run("Wait classifies target not connected", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name      string
			stderrMsg string
			wantTNC   bool
		}{
			{
				"target not connected",
				"An error occurred (TargetNotConnected) when calling the StartSession operation: i-019242a1bc22c3a59 is not connected.",
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

				factory := fakeCLIFactory(t, tt.stderrMsg, 254)
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

				if tt.wantTNC {
					assert.True(t, errors.Is(waitErr, session.ErrTargetNotConnected),
						"expected ErrTargetNotConnected, got %v", waitErr)
				} else {
					assert.False(t, errors.Is(waitErr, session.ErrTargetNotConnected),
						"did not expect ErrTargetNotConnected, got %v", waitErr)
				}
			})
		}
	})

	t.Run("the session ends with its whole process group", func(t *testing.T) {
		t.Parallel()

		params := session.StartParams{Target: "i-fake", RemoteHost: "host.example", RemotePort: 443, LocalPort: 18443}

		t.Run("Stop leaves no process of the session's group alive", func(t *testing.T) {
			t.Parallel()

			pidFile := filepath.Join(t.TempDir(), "child.pid")
			sess, err := session.NewSSMManager(pluginStandInFactory(pidFile, neverExitingStandIn)).Start(t.Context(), params)
			require.NoError(t, err)
			child := readChildPID(t, pidFile)

			stopCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, sess.Stop(stopCtx))

			assertProcessGone(t, child)
		})

		t.Run("context cancel leaves no process of the session's group alive", func(t *testing.T) {
			t.Parallel()

			pidFile := filepath.Join(t.TempDir(), "child.pid")
			ctx, cancel := context.WithCancel(t.Context())
			sess, err := session.NewSSMManager(pluginStandInFactory(pidFile, neverExitingStandIn)).Start(ctx, params)
			require.NoError(t, err)
			child := readChildPID(t, pidFile)

			cancel()
			waited := make(chan struct{})
			go func() {
				_ = sess.Wait()
				close(waited)
			}()
			select {
			case <-waited:
			case <-time.After(5 * time.Second):
				t.Fatal("Wait did not return after context cancel")
			}

			assertProcessGone(t, child)
		})

		t.Run("Stop kills a group that ignores SIGTERM once its deadline passes", func(t *testing.T) {
			t.Parallel()

			pidFile := filepath.Join(t.TempDir(), "child.pid")
			sess, err := session.NewSSMManager(pluginStandInFactory(pidFile, termIgnoringStandIn)).Start(t.Context(), params)
			require.NoError(t, err)
			child := readChildPID(t, pidFile)

			stopCtx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			require.ErrorIs(t, sess.Stop(stopCtx), context.DeadlineExceeded)

			assertProcessGone(t, child)
		})

		t.Run("a child that outlives aws and ignores SIGTERM is killed before Wait returns", func(t *testing.T) {
			t.Parallel()

			pidFile := filepath.Join(t.TempDir(), "child.pid")
			sess, err := session.NewSSMManager(pluginStandInFactory(pidFile, exitsLeavingChildBehind)).Start(t.Context(), params)
			require.NoError(t, err)
			child := readChildPID(t, pidFile)

			require.ErrorIs(t, sess.Wait(), exec.ErrWaitDelay, "the child held aws's stderr past its exit")
			assertProcessGone(t, child)
			require.NoError(t, sess.Stop(t.Context()), "Stop after the session ended")
		})
	})
}

func pluginStandInFactory(pidFile, script string) session.CommandFactory {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", script, pidFile)
	}
}

func readChildPID(t *testing.T, pidFile string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil || !strings.HasSuffix(string(b), "\n") {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "stand-in never wrote its child's PID")
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	assert.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, 3*time.Second, 20*time.Millisecond, "child process %d outlived its session", pid)
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
