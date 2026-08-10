package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/guestproto"
)

func TestServerSocketPermissionsControlRoundTripAndCleanup(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	socket := shortSocketPath(t)
	server, cancel, result := startTestServer(t, supervisor, socket)
	select {
	case <-server.Ready():
	case <-time.After(time.Second):
		t.Fatal("server was not ready")
	}
	parentInfo, err := os.Stat(filepath.Dir(socket))
	if err != nil || parentInfo.Mode().Perm() != controlDirectoryMode {
		t.Fatalf("control directory = info %v, err %v", parentInfo, err)
	}
	socketInfo, err := os.Stat(socket)
	if err != nil || socketInfo.Mode().Perm() != controlSocketMode || socketInfo.Mode()&os.ModeSocket == 0 {
		t.Fatalf("control socket = info %v, err %v", socketInfo, err)
	}
	request := guestproto.Request{Protocol: guestproto.ProtocolV1, RequestID: testRequestID, Operation: guestproto.OperationPing, DeadlineMS: 1000, Params: json.RawMessage(`{}`)}
	raw, _ := guestproto.EncodeRequest(request)
	var output bytes.Buffer
	ok, err := Control(context.Background(), socket, bytes.NewReader(raw), &output)
	if err != nil || !ok {
		t.Fatalf("Control() = (%t, %v), output %q", ok, err, output.String())
	}
	if output.Bytes()[len(output.Bytes())-1] != '\n' || bytes.Count(output.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("ctl output is not one compact line: %q", output.String())
	}
	response, err := guestproto.DecodeResponse(bytes.TrimSpace(output.Bytes()))
	if err != nil || !response.OK {
		t.Fatalf("ctl response = %+v, %v", response, err)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Serve() = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket remains after server exit: %v", err)
	}
	shutdownSupervisor(t, supervisor)
}

func TestServerMalformedStartDoesNotMutateGeneration(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	socket := shortSocketPath(t)
	server, cancel, result := startTestServer(t, supervisor, socket)
	defer func() {
		cancel()
		<-result
		shutdownSupervisor(t, supervisor)
	}()
	<-server.Ready()
	generation := uint64(0)
	request := guestproto.Request{Protocol: guestproto.ProtocolV1, RequestID: testRequestID, Operation: guestproto.OperationStart, IfGeneration: &generation, IdempotencyKey: testKey, DeadlineMS: 1000, Params: json.RawMessage(`{"processes":[],"log_limit_bytes":1,"unknown":true}`)}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := guestproto.WriteFrame(connection, raw); err != nil {
		t.Fatal(err)
	}
	response, err := guestproto.ReadResponse(connection)
	if err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != guestproto.CodeInvalidRequest {
		t.Fatalf("malformed response = %+v", response)
	}
	if status := supervisor.Status(); status.Generation != 0 || len(status.Processes) != 0 {
		t.Fatalf("malformed request mutated state: %+v", status)
	}
}

func TestControlReturnsApplicationErrorResponse(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	socket := shortSocketPath(t)
	server, cancel, result := startTestServer(t, supervisor, socket)
	defer func() {
		cancel()
		<-result
		shutdownSupervisor(t, supervisor)
	}()
	<-server.Ready()
	request := guestproto.Request{Protocol: guestproto.ProtocolV1, RequestID: testRequestID, Operation: guestproto.OperationWait, Target: "missing", DeadlineMS: 1000, Params: json.RawMessage(`{}`)}
	raw, _ := guestproto.EncodeRequest(request)
	var output bytes.Buffer
	ok, err := Control(context.Background(), socket, bytes.NewReader(raw), &output)
	if err != nil || ok {
		t.Fatalf("Control() = (%t, %v), output %q", ok, err, output.String())
	}
	response, err := guestproto.DecodeResponse(bytes.TrimSpace(output.Bytes()))
	if err != nil || response.Error == nil || response.Error.Code != guestproto.CodeWrongState {
		t.Fatalf("application error response = %+v, %v", response, err)
	}
}

func TestServerRejectsExistingSocketPath(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	parent := filepath.Dir(shortSocketPath(t))
	if err := os.MkdirAll(parent, controlDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, controlDirectoryMode); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "control.sock")
	if err := os.WriteFile(path, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(supervisor, ServerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(context.Background()); err == nil {
		t.Fatal("Serve() accepted an existing socket path")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "sentinel" {
		t.Fatalf("unsafe path was changed: %q, %v", contents, err)
	}
	shutdownSupervisor(t, supervisor)
}

func TestServerRecoversVerifiedStaleSocket(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	path := shortSocketPath(t)
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, controlDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, controlDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(parent, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, controlSocketMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	server, cancel, result := startTestServer(t, supervisor, path)
	<-server.Ready()
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("Serve() after stale socket = %v", err)
	}
	shutdownSupervisor(t, supervisor)
}

func startTestServer(t *testing.T, supervisor *Supervisor, socket string) (*Server, context.CancelFunc, <-chan error) {
	t.Helper()
	server, err := NewServer(supervisor, ServerOptions{Path: socket})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Serve(ctx) }()
	select {
	case <-server.Ready():
	case err := <-result:
		t.Fatalf("Serve() before ready = %v", err)
	case <-time.After(time.Second):
		t.Fatal("Serve() did not become ready")
	}
	return server, cancel, result
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "dg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return filepath.Join(root, "dsx", "control.sock")
}
