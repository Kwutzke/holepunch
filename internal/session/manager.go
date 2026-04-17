package session

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// stderrCaptureSize bounds the ring buffer holding recent aws-cli stderr.
// 8 KiB keeps the full SSO-token error text (typically <2 KiB) without
// risking unbounded memory growth on a noisy child.
const stderrCaptureSize = 8 * 1024

// StartParams contains the parameters for starting an SSM port-forwarding session.
type StartParams struct {
	AWSProfile string
	AWSRegion  string
	Target     string // SSM target instance ID
	RemoteHost string
	RemotePort int
	LocalIP    net.IP
	LocalPort  int
}

// Session represents a running SSM port-forwarding session.
type Session interface {
	PID() int
	Wait() error
	Stop(ctx context.Context) error
}

// Manager starts SSM port-forwarding sessions.
type Manager interface {
	Start(ctx context.Context, params StartParams) (Session, error)
}

// CommandFactory creates exec.Cmd instances. Injectable for testing.
type CommandFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// SSMManager implements Manager by shelling out to the AWS CLI.
type SSMManager struct {
	cmdFactory CommandFactory
}

// NewSSMManager creates a new SSMManager.
// If cmdFactory is nil, it defaults to exec.CommandContext.
func NewSSMManager(cmdFactory CommandFactory) *SSMManager {
	if cmdFactory == nil {
		cmdFactory = exec.CommandContext
	}
	return &SSMManager{cmdFactory: cmdFactory}
}

func (m *SSMManager) Start(ctx context.Context, params StartParams) (Session, error) {
	args := buildSSMArgs(params)
	cmd := m.cmdFactory(ctx, "aws", args...)
	cmd.Stdout = os.Stdout

	stderrBuf := newRingBuffer(stderrCaptureSize)
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrBuf)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting SSM session: %w", err)
	}

	return &ssmSession{cmd: cmd, stderr: stderrBuf}, nil
}

// BuildSSMArgs constructs the AWS CLI arguments for an SSM port-forwarding session.
// Exported for testing command construction without starting a process.
func BuildSSMArgs(params StartParams) []string {
	return buildSSMArgs(params)
}

func buildSSMArgs(params StartParams) []string {
	paramJSON := fmt.Sprintf(
		`{"host":["%s"],"portNumber":["%d"],"localPortNumber":["%d"]}`,
		params.RemoteHost, params.RemotePort, params.LocalPort,
	)

	args := []string{
		"ssm", "start-session",
		"--target", params.Target,
		"--document-name", "AWS-StartPortForwardingSessionToRemoteHost",
		"--parameters", paramJSON,
	}

	if params.AWSProfile != "" {
		args = append(args, "--profile", params.AWSProfile)
	}
	if params.AWSRegion != "" {
		args = append(args, "--region", params.AWSRegion)
	}

	return args
}

type ssmSession struct {
	cmd    *exec.Cmd
	stderr *ringBuffer
}

func (s *ssmSession) PID() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

func (s *ssmSession) Wait() error {
	err := s.cmd.Wait()
	if err == nil {
		return nil
	}
	if s.stderr != nil && matchesCredentialsExpired(s.stderr.Bytes()) {
		snippet := expiredSnippet(s.stderr.Bytes())
		if snippet == "" {
			return fmt.Errorf("%w: %v", ErrCredentialsExpired, err)
		}
		return fmt.Errorf("%w: %s", ErrCredentialsExpired, snippet)
	}
	return err
}

func (s *ssmSession) Stop(ctx context.Context) error {
	if s.cmd.Process == nil {
		return nil
	}

	// Send SIGTERM first for graceful shutdown.
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// Process might already be dead.
		if !isProcessFinished(err) {
			return fmt.Errorf("sending SIGTERM: %w", err)
		}
		return nil
	}

	// Wait for process to exit or context to cancel.
	done := make(chan error, 1)
	go func() {
		done <- s.cmd.Wait()
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Context cancelled, force kill.
		if err := s.cmd.Process.Signal(syscall.SIGKILL); err != nil && !isProcessFinished(err) {
			return fmt.Errorf("sending SIGKILL: %w", err)
		}
		// Wait briefly for the kill to take effect.
		killCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		select {
		case <-done:
		case <-killCtx.Done():
		}
		return ctx.Err()
	}
}

func isProcessFinished(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}
