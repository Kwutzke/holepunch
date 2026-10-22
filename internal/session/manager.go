package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

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

// stderrCaptureSize bounds the ring buffer holding recent aws-cli stderr.
// 8 KiB keeps the full SSO-token error text (typically <2 KiB) without
// risking unbounded memory growth on a noisy child.
const stderrCaptureSize = 8 * 1024

// groupGrace is how long a session's process group gets to exit after SIGTERM before SIGKILL.
const groupGrace = 2 * time.Second

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

	// aws leads its own process group, so session-manager-plugin can be signalled with it.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0

	s := &ssmSession{cmd: cmd, stderr: stderrBuf, done: make(chan struct{})}
	cmd.Cancel = func() error { return s.signalGroup(syscall.SIGTERM) }
	cmd.WaitDelay = groupGrace

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting SSM session: %w", err)
	}
	go s.wait()

	return s, nil
}

type ssmSession struct {
	cmd    *exec.Cmd
	stderr *ringBuffer

	done chan struct{}
	err  error // set before done closes
}

func (s *ssmSession) PID() int {
	return s.cmd.Process.Pid
}

func (s *ssmSession) Wait() error {
	<-s.done
	return s.err
}

func (s *ssmSession) Stop(ctx context.Context) error {
	// Once the group is gone its ID may be reused, so it must not be signalled.
	select {
	case <-s.done:
		return nil
	default:
	}

	if err := s.signalGroup(syscall.SIGTERM); err != nil {
		return fmt.Errorf("sending SIGTERM: %w", err)
	}

	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
	}

	if err := s.signalGroup(syscall.SIGKILL); err != nil {
		return fmt.Errorf("sending SIGKILL: %w", err)
	}
	select {
	case <-s.done:
	case <-time.After(groupGrace):
	}
	return ctx.Err()
}

// wait is the only caller of cmd.Wait. The session ends only once its whole process group is
// gone, so a plugin that outlives aws is terminated with it.
func (s *ssmSession) wait() {
	err := s.cmd.Wait()
	s.terminateGroup()
	s.err = s.classify(err)
	close(s.done)
}

func (s *ssmSession) classify(err error) error {
	if err == nil {
		return nil
	}
	stderrBytes := s.stderr.Bytes()
	if matchesCredentialsExpired(stderrBytes) {
		return wrapSessionErr(ErrCredentialsExpired, expiredSnippet(stderrBytes), err)
	}
	if matchesTargetNotConnected(stderrBytes) {
		return wrapSessionErr(ErrTargetNotConnected, targetNotConnectedSnippet(stderrBytes), err)
	}
	return err
}

// terminateGroup sends SIGTERM to what is left of the process group, then SIGKILL once groupGrace
// passes with a member still alive.
func (s *ssmSession) terminateGroup() {
	if err := s.signalGroup(syscall.SIGTERM); err != nil || !s.groupAlive() {
		return
	}
	deadline := time.Now().Add(groupGrace)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if !s.groupAlive() {
			return
		}
	}
	_ = s.signalGroup(syscall.SIGKILL)
}

func (s *ssmSession) groupAlive() bool {
	return syscall.Kill(-s.cmd.Process.Pid, 0) == nil
}

// signalGroup treats a group with no members left as success.
func (s *ssmSession) signalGroup(sig syscall.Signal) error {
	err := syscall.Kill(-s.cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
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

func wrapSessionErr(sentinel error, snippet string, cause error) error {
	if snippet == "" {
		return fmt.Errorf("%w: %v", sentinel, cause)
	}
	return fmt.Errorf("%w: %s", sentinel, snippet)
}
