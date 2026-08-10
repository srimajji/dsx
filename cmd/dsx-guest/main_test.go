package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/guest"
	"github.com/srimajji/dsx/internal/guestproto"
)

func TestCtlProcess(t *testing.T) {
	if os.Getenv("GO_WANT_DSX_CTL_HELPER") != "1" {
		return
	}
	os.Exit(run([]string{"ctl", "--socket", os.Getenv("DSX_CTL_SOCKET")}))
}

func TestCtlModeRoundTrip(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "dg-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "dsx", "control.sock")
	supervisor, err := guest.NewSupervisor(guest.Options{Version: "test", InstanceID: "40000000-0000-4000-8000-000000000004", ChildUID: uint32(os.Geteuid()), ChildGID: uint32(os.Getegid())})
	if err != nil {
		t.Fatal(err)
	}
	server, err := guest.NewServer(supervisor, guest.ServerOptions{Path: socket})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancelServer := context.WithCancel(context.Background())
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(ctx) }()
	select {
	case <-server.Ready():
	case err := <-serveResult:
		t.Fatalf("Serve() = %v", err)
	case <-time.After(time.Second):
		t.Fatal("server was not ready")
	}
	defer func() {
		cancelServer()
		<-serveResult
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = supervisor.Shutdown(shutdownContext)
	}()
	request := guestproto.Request{Protocol: guestproto.ProtocolV1, RequestID: "50000000-0000-4000-8000-000000000005", Operation: guestproto.OperationPing, DeadlineMS: 1000, Params: json.RawMessage(`{}`)}
	requestJSON, err := guestproto.EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestCtlProcess$")
	command.Env = append(os.Environ(), "GO_WANT_DSX_CTL_HELPER=1", "DSX_CTL_SOCKET="+socket)
	command.Stdin = bytes.NewReader(requestJSON)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("ctl command = %v, stderr %q", err, stderr.String())
	}
	if bytes.Count(stdout.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("ctl stdout = %q", stdout.String())
	}
	response, err := guestproto.DecodeResponse(bytes.TrimSpace(stdout.Bytes()))
	if err != nil || !response.OK {
		t.Fatalf("ctl response = %+v, %v", response, err)
	}
}

func TestParseChildIdentityRejectsMalformedOrRootUID(t *testing.T) {
	for _, values := range [][2]string{{"", "20"}, {"0", "20"}, {"-1", "20"}, {"1x", "20"}, {"4294967296", "20"}, {"501", ""}, {"501", "0"}, {"501", "-1"}, {"501", "4294967296"}} {
		if _, _, err := parseChildIdentity(values[0], values[1]); err == nil {
			t.Fatalf("parseChildIdentity(%q, %q) succeeded", values[0], values[1])
		}
	}
	uid, gid, err := parseChildIdentity("501", "20")
	if err != nil || uid != 501 || gid != 20 {
		t.Fatalf("parseChildIdentity(valid) = (%d, %d, %v)", uid, gid, err)
	}
}

func TestParseExecArgumentsAcceptsOnlyOptionalEnvironmentFileBeforeDelimiter(t *testing.T) {
	path, argv, err := parseExecArguments([]string{"--env-file", "/tmp/dsx-run/00000000-0000-7000-8000-000000000000/env-00000000000000000000000000000000", "--", "printf", "%s", "a b"})
	if err != nil || path == "" || len(argv) != 3 || argv[2] != "a b" {
		t.Fatalf("parseExecArguments(valid) = (%q, %#v, %v)", path, argv, err)
	}
	path, argv, err = parseExecArguments([]string{"--", "sh"})
	if err != nil || path != "" || len(argv) != 1 || argv[0] != "sh" {
		t.Fatalf("parseExecArguments(plain) = (%q, %#v, %v)", path, argv, err)
	}
	for _, arguments := range [][]string{
		nil,
		{"--env-file"},
		{"--env-file", "/tmp/file"},
		{"--env-file", "/tmp/one", "--env-file", "/tmp/two", "--", "sh"},
		{"--", ""},
		{"sh"},
	} {
		if _, _, err := parseExecArguments(arguments); err == nil {
			t.Fatalf("parseExecArguments(%#v) succeeded", arguments)
		}
	}
}
