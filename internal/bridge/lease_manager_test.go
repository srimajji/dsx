package bridge

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/model"
)

func leaseTestIdentity(t *testing.T, project, run string) LeaseIdentity {
	t.Helper()
	projectID, err := model.ParseProjectID(project)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := model.ParseRunID(run)
	if err != nil {
		t.Fatal(err)
	}
	return LeaseIdentity{ProjectID: projectID, Sandbox: "main", RunID: runID}
}

func leaseTestManager(t *testing.T) *ProductionLeaseManager {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProductionLeaseManager(root, executable)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func leaseTestSpecs() []RelaySpec {
	return []RelaySpec{{
		Name: "team-db", Mode: RelayModePrivateHost, ListenerIP: netip.MustParseAddr("192.168.64.1"), OwnerIP: netip.MustParseAddr("192.168.64.12"),
		Destination: netip.MustParseAddrPort("10.40.0.9:5432"), DestinationLiteral: true, Lease: time.Hour,
	}}
}

func TestRelaySpecDigestIsCanonicalAndEnvironmentNamesAreBounded(t *testing.T) {
	forward, first, err := validateRelaySpecs([]RelaySpec{
		{Name: "z", Mode: RelayModePrivateHost, ListenerIP: netip.MustParseAddr("192.168.64.1"), OwnerIP: netip.MustParseAddr("192.168.64.12"), Destination: netip.MustParseAddrPort("10.0.0.2:2"), DestinationLiteral: true, Lease: time.Minute},
		{Name: "a-b", Mode: RelayModePrivateHost, ListenerIP: netip.MustParseAddr("192.168.64.1"), OwnerIP: netip.MustParseAddr("192.168.64.12"), Destination: netip.MustParseAddrPort("10.0.0.1:1"), DestinationLiteral: true, Lease: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	reverse, second, err := validateRelaySpecs([]RelaySpec{forward[1], forward[0]})
	if err != nil {
		t.Fatal(err)
	}
	if first != second || reverse[0].Name != "a-b" {
		t.Fatalf("canonical digest/order = %q %q %#v", first, second, reverse)
	}
	if base, err := relayEnvironmentBase("a-b"); err != nil || base != "DSX_BRIDGE_A_B" {
		t.Fatalf("environment base = %q, %v", base, err)
	}
}

func TestEnsurePreservesContradictoryStaleLedger(t *testing.T) {
	manager := leaseTestManager(t)
	identity := leaseTestIdentity(t, "abcdefghijklmnopqrst", "01890f5c-7b00-7000-8000-000000000101")
	paths, err := manager.ensurePaths(identity)
	if err != nil {
		t.Fatal(err)
	}
	ledger := leaseLedger{Version: 1, Identity: identity, SpecDigest: "different-plan", PID: 42, ProcessStartedAt: time.Now().UTC(), Executable: manager.executable, Result: LeaseResult{Bindings: []ListenerBinding{{Name: "team-db", Mode: RelayModePrivateHost, Addr: netip.MustParseAddr("192.168.64.1"), Port: 49152}}, Environment: map[string]string{"DSX_BRIDGE_TEAM_DB_HOST": "192.168.64.1", "DSX_BRIDGE_TEAM_DB_PORT": "49152"}}}
	encoded, _ := json.Marshal(ledger)
	if err := atomicWritePrivate(paths.ledger, encoded); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Ensure(context.Background(), identity, leaseTestSpecs()); model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("Ensure error = %v", err)
	}
	after, err := os.ReadFile(paths.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("contradictory ledger was modified")
	}
}

func TestPublicationLeaseResultContainsOnlyNamedListenerTruth(t *testing.T) {
	spec := RelaySpec{
		Name: "web", Mode: RelayModePublication,
		ListenerIP: netip.MustParseAddr("127.0.0.1"), ListenerPort: 49152,
		OwnerIP:            netip.MustParseAddr("127.0.0.1"),
		Destination:        netip.MustParseAddrPort("192.168.64.12:3000"),
		DestinationLiteral: true, Lease: time.Hour,
	}
	result := LeaseResult{Bindings: []ListenerBinding{{Name: "web", Mode: RelayModePublication, Addr: spec.ListenerIP, Port: spec.ListenerPort}}}
	if !validLeaseResult([]RelaySpec{spec}, result) {
		t.Fatal("exact publication result rejected")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(encoded), spec.Destination.String()) || contains(string(encoded), "token") {
		t.Fatalf("lease result disclosed private authority: %s", encoded)
	}
}

func TestStatusReportsExactFailureAndDoesNotExposeDestination(t *testing.T) {
	manager := leaseTestManager(t)
	identity := leaseTestIdentity(t, "bcdefghijklmnopqrstu", "01890f5c-7b00-7000-8000-000000000102")
	paths, err := manager.ensurePaths(identity)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := validateRelaySpecs(leaseTestSpecs())
	if err != nil {
		t.Fatal(err)
	}
	failure := failureStatus{Version: 1, Identity: identity, SpecDigest: digest, Failure: "relay_failed", At: time.Now().UTC()}
	encoded, _ := json.Marshal(failure)
	if err := atomicWritePrivate(paths.failure, encoded); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(context.Background(), identity)
	if err != nil || status.State != "error" || status.Failure != "relay_failed" {
		t.Fatalf("Status = %#v, %v", status, err)
	}
	statusJSON, _ := json.Marshal(status)
	if string(statusJSON) == "" || containsPrivateDestination(string(statusJSON)) {
		t.Fatalf("status exposed private plan: %s", statusJSON)
	}
}

func TestDifferentWorkspaceCannotRemoveAnotherLeaseEvidence(t *testing.T) {
	manager := leaseTestManager(t)
	first := leaseTestIdentity(t, "cdefghijklmnopqrstuv", "01890f5c-7b00-7000-8000-000000000103")
	second := leaseTestIdentity(t, "defghijklmnopqrstuvw", "01890f5c-7b00-7000-8000-000000000104")
	paths, err := manager.ensurePaths(first)
	if err != nil {
		t.Fatal(err)
	}
	failure := failureStatus{Version: 1, Identity: first, SpecDigest: "exact", Failure: "relay_failed", At: time.Now().UTC()}
	encoded, _ := json.Marshal(failure)
	if err := atomicWritePrivate(paths.failure, encoded); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.failure); err != nil {
		t.Fatalf("other workspace removed evidence: %v", err)
	}
	if filepath.Dir(paths.run) == "" {
		t.Fatal("invalid test lease path")
	}
}

func containsPrivateDestination(value string) bool {
	return len(value) >= len("10.40.0.9:5432") && (value == "10.40.0.9:5432" || contains(value, "10.40.0.9:5432"))
}

func contains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
