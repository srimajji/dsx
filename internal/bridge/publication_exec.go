package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/srimajji/dsx/internal/ownership"
	"github.com/srimajji/dsx/internal/runtime"
)

const maxContainerInspectBytes = 1 << 20

func dialOwnedWorkspaceLoopback(ctx context.Context, containerExecutable executableIdentity, lease LeaseIdentity, target PublicationTarget, port uint16) (net.Conn, error) {
	if ctx == nil || port == 0 {
		return nil, errors.New("invalid publication guest execution request")
	}
	if err := validateOwnedPublicationWorkspace(ctx, containerExecutable, lease, target); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, containerExecutable.Path,
		"exec", "--interactive", "--user", target.GuestUser, target.WorkspaceID,
		target.GuestHelperPath, "relay-loopback", "--port", strconv.Itoa(int(port)),
	)
	command.Env = []string{"LANG=C", "LC_ALL=C"}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open publication guest stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open publication guest stdout: %w", err)
	}
	var stderr boundedProcessBuffer
	stderr.limit = 4096
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start publication guest relay: %w", err)
	}
	connection := &execStreamConn{
		command: command,
		stdin:   stdin,
		stdout:  stdout,
		done:    make(chan error, 1),
		local:   publicationAddr("host-publication"),
		remote:  publicationAddr("guest-loopback:" + strconv.Itoa(int(port))),
	}
	go func() { connection.done <- command.Wait() }()
	return connection, nil
}

func validateOwnedLeaseWorkspace(ctx context.Context, containerExecutable executableIdentity, lease LeaseIdentity) error {
	identity, err := ownership.NewIdentity(lease.ProjectID, lease.Sandbox, lease.RunID, runtime.ResourceWorkspace, "workspace")
	if err != nil {
		return errors.New("derive exact lease workspace identity")
	}
	return validateOwnedPublicationWorkspace(ctx, containerExecutable, lease, PublicationTarget{WorkspaceID: identity.Name()})
}

func validateOwnedPublicationWorkspace(ctx context.Context, containerExecutable executableIdentity, lease LeaseIdentity, target PublicationTarget) error {
	current, err := canonicalExecutableIdentity(containerExecutable.Path)
	if err != nil || current != containerExecutable {
		return errors.New("Apple container executable identity changed")
	}
	identity, err := ownership.NewIdentity(lease.ProjectID, lease.Sandbox, lease.RunID, runtime.ResourceWorkspace, "workspace")
	if err != nil || identity.Name() != target.WorkspaceID {
		return errors.New("publication workspace identity is not the exact owned workspace")
	}
	command := exec.CommandContext(ctx, containerExecutable.Path, "inspect", target.WorkspaceID)
	command.Env = []string{"LANG=C", "LC_ALL=C"}
	var stdout, stderr boundedProcessBuffer
	stdout.limit = maxContainerInspectBytes
	stderr.limit = 4096
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil || stdout.exceeded || stderr.exceeded {
		return errors.New("inspect publication workspace ownership")
	}
	var inspected []struct {
		Configuration struct {
			ID     string            `json:"id"`
			Labels map[string]string `json:"labels"`
		} `json:"configuration"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.bytes()))
	if err := decoder.Decode(&inspected); err != nil || len(inspected) != 1 {
		return errors.New("decode publication workspace ownership")
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("publication workspace inspect has trailing data")
	}
	observed := inspected[0]
	if observed.Configuration.ID != target.WorkspaceID || observed.Status.State != "running" {
		return errors.New("publication workspace is not the exact running workspace")
	}
	expected := identity.Labels()
	if len(observed.Configuration.Labels) != len(expected) {
		return errors.New("publication workspace ownership labels differ")
	}
	for _, label := range expected {
		if observed.Configuration.Labels[label.Key] != label.Value {
			return errors.New("publication workspace ownership labels differ")
		}
	}
	return nil
}

type boundedProcessBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *boundedProcessBuffer) Write(value []byte) (int, error) {
	accepted := len(value)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining < len(value) {
		buffer.exceeded = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	_, _ = buffer.buffer.Write(value)
	return accepted, nil
}

func (buffer *boundedProcessBuffer) bytes() []byte { return buffer.buffer.Bytes() }

type execStreamConn struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	done    chan error
	local   net.Addr
	remote  net.Addr
	once    sync.Once
}

func (connection *execStreamConn) Read(value []byte) (int, error) {
	return connection.stdout.Read(value)
}
func (connection *execStreamConn) Write(value []byte) (int, error) {
	return connection.stdin.Write(value)
}
func (connection *execStreamConn) LocalAddr() net.Addr              { return connection.local }
func (connection *execStreamConn) RemoteAddr() net.Addr             { return connection.remote }
func (connection *execStreamConn) SetDeadline(time.Time) error      { return nil }
func (connection *execStreamConn) SetReadDeadline(time.Time) error  { return nil }
func (connection *execStreamConn) SetWriteDeadline(time.Time) error { return nil }
func (connection *execStreamConn) CloseWrite() error                { return connection.stdin.Close() }
func (connection *execStreamConn) CloseRead() error                 { return connection.stdout.Close() }

func (connection *execStreamConn) Close() error {
	connection.once.Do(func() {
		_ = connection.stdin.Close()
		_ = connection.stdout.Close()
		if connection.command.Process != nil {
			_ = connection.command.Process.Kill()
		}
		<-connection.done
	})
	return nil
}

type publicationAddr string

func (address publicationAddr) Network() string { return "dsx-publication" }
func (address publicationAddr) String() string  { return string(address) }
