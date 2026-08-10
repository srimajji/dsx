package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/ownership"
	"github.com/srimajji/dsx/internal/runtime"
)

func TestDialOwnedWorkspaceLoopbackUsesPinnedExecStream(t *testing.T) {
	projectID, err := model.NewProjectID(filepath.Join(t.TempDir(), "project"))
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := model.ParseSandboxName("main")
	if err != nil {
		t.Fatal(err)
	}
	runID, err := model.NewRunID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	lease := LeaseIdentity{ProjectID: projectID, Sandbox: sandbox, RunID: runID}
	owned, err := ownership.NewIdentity(projectID, sandbox, runID, runtime.ResourceWorkspace, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	labels := make(map[string]string)
	for _, label := range owned.Labels() {
		labels[label.Key] = label.Value
	}
	inspection, err := json.Marshal([]any{map[string]any{
		"configuration": map[string]any{"id": owned.Name(), "labels": labels},
		"status":        map[string]any{"state": "running"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "container")
	script := "#!/bin/sh\nif [ \"$1\" = inspect ]; then\n  /bin/echo '" + string(inspection) + "'\nelif [ \"$1\" = exec ]; then\n  /bin/cat\nelse\n  exit 2\nfi\n"
	if err := os.WriteFile(executable, []byte(script), 0o500); err != nil {
		t.Fatal(err)
	}
	executableID, err := canonicalExecutableIdentity(executable)
	if err != nil {
		t.Fatal(err)
	}
	target := PublicationTarget{WorkspaceID: owned.Name(), GuestUser: "1000:1000", GuestHelperPath: "/usr/local/libexec/dsx/dsx-guest"}
	connection, err := dialOwnedWorkspaceLoopback(context.Background(), executableID, lease, target, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte("publication-stream")); err != nil {
		t.Fatal(err)
	}
	if closer, ok := connection.(interface{ CloseWrite() error }); !ok {
		t.Fatal("publication connection does not support half-close")
	} else if err := closer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(connection)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "publication-stream" {
		t.Fatalf("stream output = %q", output)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRelaySpecsRequiresOwnedPublicationTarget(t *testing.T) {
	_, _, err := validateRelaySpecs([]RelaySpec{{
		Name: "web", Mode: RelayModePublication,
		ListenerIP: netip.MustParseAddr("127.0.0.1"), ListenerPort: 49152, OwnerIP: netip.MustParseAddr("127.0.0.1"),
		Destination: netip.MustParseAddrPort("192.168.64.2:3000"), DestinationLiteral: true, Lease: time.Minute,
	}})
	if err == nil {
		t.Fatal("publication without guest execution target was accepted")
	}
}
