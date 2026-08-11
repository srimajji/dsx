package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/ownership"
	dsxruntime "github.com/srimajji/dsx/internal/runtime"

	"golang.org/x/sys/unix"
)

const leaseInvokerPayloadEnvironment = "DSX_TEST_LEASE_INVOKER"

type leaseInvokerPayload struct {
	StateRoot string        `json:"state_root"`
	Identity  LeaseIdentity `json:"identity"`
	Specs     []RelaySpec   `json:"specs"`
}

type leaseInvokerReady struct {
	PID int `json:"pid"`
}

func TestMain(m *testing.M) {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "__bridge-helper":
			os.Exit(RunLeaseHelper())
		case "__bridge-control":
			os.Exit(RunLeaseControlClient())
		case "__bridge-test-invoker":
			os.Exit(runLeaseTestInvoker())
		}
	}
	os.Exit(m.Run())
}

func runLeaseTestInvoker() int {
	encoded := os.Getenv(leaseInvokerPayloadEnvironment)
	payloadBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 2
	}
	var payload leaseInvokerPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		return 2
	}
	manager, err := NewProductionLeaseManager(payload.StateRoot, executable)
	if err != nil {
		return 2
	}
	if _, err := manager.Ensure(context.Background(), payload.Identity, payload.Specs); err != nil {
		return 1
	}
	paths := leaseLifetimePaths(payload.StateRoot, payload.Identity)
	ledger, found, err := loadPrivateJSON[leaseLedger](paths.ledger, MaxControlBytes)
	if err != nil || !found {
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(leaseInvokerReady{PID: ledger.PID}); err != nil {
		return 1
	}
	for {
		time.Sleep(time.Hour)
	}
}

func leaseLifetimeManager(t *testing.T) *ProductionLeaseManager {
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
	manager.readyWait = 5 * time.Second
	manager.stopWait = 5 * time.Second
	return manager
}

func leaseOwnedLifetimeManager(t *testing.T, identity LeaseIdentity) *ProductionLeaseManager {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owned, err := ownership.NewIdentity(identity.ProjectID, identity.Sandbox, identity.RunID, dsxruntime.ResourceWorkspace, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	labels := make(map[string]string)
	for _, label := range owned.Labels() {
		labels[label.Key] = label.Value
	}
	inspect, err := json.Marshal([]map[string]any{{
		"configuration": map[string]any{"id": owned.Name(), "labels": labels},
		"status":        map[string]any{"state": "running"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	containerExecutable := filepath.Join(t.TempDir(), "container")
	script := "#!/bin/sh\nprintf '%s' '" + string(inspect) + "'\n"
	if err := os.WriteFile(containerExecutable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewProductionLeaseManagerWithContainer(root, executable, containerExecutable)
	if err != nil {
		t.Fatal(err)
	}
	manager.readyWait = 5 * time.Second
	manager.stopWait = 5 * time.Second
	return manager
}

func leaseLifetimeSpecs(t *testing.T, lease time.Duration) []RelaySpec {
	t.Helper()
	owner := netip.MustParseAddr("192.168.64.12")
	listener, err := RouteSourceAddr(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	return []RelaySpec{{
		Name: "durable", Mode: RelayModePrivateHost, ListenerIP: listener, OwnerIP: owner,
		Destination: netip.MustParseAddrPort("10.40.0.9:5432"), DestinationLiteral: true, Lease: lease,
	}}
}

func leaseLifetimePaths(stateRoot string, identity LeaseIdentity) leasePaths {
	root := filepath.Join(stateRoot, bridgeDirectoryName)
	project := filepath.Join(root, string(identity.ProjectID))
	sandbox := filepath.Join(project, string(identity.Sandbox))
	run := filepath.Join(sandbox, string(identity.RunID))
	return makeLeasePaths(root, project, sandbox, run)
}

func leaseControlEvidence(t *testing.T, manager *ProductionLeaseManager, identity LeaseIdentity, operation, token, digest string) controlResponse {
	t.Helper()
	paths := leaseLifetimePaths(manager.stateRoot, identity)
	response, err := manager.control(context.Background(), paths.socket, controlRequest{
		Version: 1, Operation: operation, Token: token, Identity: identity, SpecDigest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestAuthenticatedExactRenewalExtendsActiveLease(t *testing.T) {
	manager := leaseLifetimeManager(t)
	identity := leaseTestIdentity(t, "efghijklmnopqrstuvwx", "01890f5c-7b00-7000-8000-000000000201")
	specs := leaseLifetimeSpecs(t, 30*time.Second)
	if _, err := manager.Ensure(context.Background(), identity, specs); err != nil {
		status, statusErr := manager.Status(context.Background(), identity)
		t.Fatalf("ensure = %v; status = %#v, %v", err, status, statusErr)
	}
	defer manager.Stop(context.Background(), identity)

	paths := leaseLifetimePaths(manager.stateRoot, identity)
	ledger, found, err := loadPrivateJSON[leaseLedger](paths.ledger, MaxControlBytes)
	if err != nil || !found {
		t.Fatalf("load live ledger = %#v, %v", ledger, err)
	}
	token, err := readPrivateToken(paths.token)
	if err != nil {
		t.Fatal(err)
	}
	initial := leaseControlEvidence(t, manager, identity, "status", token, ledger.SpecDigest)
	wrongTokenBytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(wrongTokenBytes) != 32 {
		t.Fatalf("decode test control token = %d bytes, %v", len(wrongTokenBytes), err)
	}
	wrongTokenBytes[0] ^= 0xff
	wrongToken := base64.RawURLEncoding.EncodeToString(wrongTokenBytes)
	rejected := leaseControlEvidence(t, manager, identity, "renew", wrongToken, ledger.SpecDigest)
	if rejected.State != "error" || rejected.Failure != "control_failed" || !rejected.ExpiresAt.IsZero() {
		t.Fatalf("unauthenticated renewal response = %#v", rejected)
	}
	rejectedExact := leaseControlEvidence(t, manager, identity, "renew", token, ledger.SpecDigest+"-other")
	if rejectedExact.State != "error" || rejectedExact.Failure != "control_failed" || !rejectedExact.ExpiresAt.IsZero() {
		t.Fatalf("contradictory renewal response = %#v", rejectedExact)
	}
	unchanged := leaseControlEvidence(t, manager, identity, "status", token, ledger.SpecDigest)
	if !unchanged.ExpiresAt.Equal(initial.ExpiresAt) {
		t.Fatalf("rejected renewal changed expiry from %s to %s", initial.ExpiresAt, unchanged.ExpiresAt)
	}

	time.Sleep(50 * time.Millisecond)
	if _, err := manager.Ensure(context.Background(), identity, specs); err != nil {
		t.Fatal(err)
	}
	renewed := leaseControlEvidence(t, manager, identity, "status", token, ledger.SpecDigest)
	if renewed.State != "running" || !renewed.ExpiresAt.After(initial.ExpiresAt) {
		t.Fatalf("renewed response %#v did not extend initial expiry %s", renewed, initial.ExpiresAt)
	}
	if err := manager.Stop(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	absent, err := exactLeaseArtifactsAbsent(paths)
	if err != nil || !absent {
		t.Fatalf("exact artifacts absent after stop = %v, %v", absent, err)
	}
}

func TestAuthenticatedOldExecutableLeaseCanBeInspectedAndStoppedAfterUpgrade(t *testing.T) {
	manager := leaseLifetimeManager(t)
	identity := leaseTestIdentity(t, "ghijklmnopqrstuvwxyz", "01890f5c-7b00-7000-8000-000000000206")
	specs := leaseLifetimeSpecs(t, 30*time.Second)
	if _, err := manager.Ensure(context.Background(), identity, specs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background(), identity) })
	executable, err := os.ReadFile(manager.executable.Path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "upgraded-dsx")
	if err := os.WriteFile(replacement, executable, 0o700); err != nil {
		t.Fatal(err)
	}
	upgraded, err := NewProductionLeaseManager(manager.stateRoot, replacement)
	if err != nil {
		t.Fatal(err)
	}
	upgraded.stopWait = 5 * time.Second
	status, err := upgraded.Status(context.Background(), identity)
	if err != nil || status.State != "running" {
		t.Fatalf("upgraded manager Status() = %#v, %v", status, err)
	}
	if err := upgraded.Stop(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	status, err = upgraded.Status(context.Background(), identity)
	if err != nil || status.State != "absent" {
		t.Fatalf("stopped old executable lease Status() = %#v, %v", status, err)
	}
}

func TestIdleExactRunningWorkspaceSelfRenewsBeyondInitialLease(t *testing.T) {
	identity := leaseTestIdentity(t, "ijklmnopqrstuvwxyzab", "01890f5c-7b00-7000-8000-000000000205")
	manager := leaseOwnedLifetimeManager(t, identity)
	specs := leaseLifetimeSpecs(t, 700*time.Millisecond)
	if _, err := manager.Ensure(context.Background(), identity, specs); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background(), identity)
	time.Sleep(1100 * time.Millisecond)
	status, err := manager.Status(context.Background(), identity)
	if err != nil || status.State != "running" {
		t.Fatalf("idle exact-owned workspace lease after initial boundary = %#v, %v", status, err)
	}
	if err := manager.Stop(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
}

func TestUnrenewedHelperExpiresAndStopCleansExactFailure(t *testing.T) {
	manager := leaseLifetimeManager(t)
	identity := leaseTestIdentity(t, "fghijklmnopqrstuvwxy", "01890f5c-7b00-7000-8000-000000000202")
	specs := leaseLifetimeSpecs(t, 350*time.Millisecond)
	if _, err := manager.Ensure(context.Background(), identity, specs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := manager.Status(context.Background(), identity)
		if err == nil && status.State == "error" && status.Failure == "expired" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unrenewed helper did not expire: %#v, %v", status, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	paths := leaseLifetimePaths(manager.stateRoot, identity)
	if _, err := os.Lstat(paths.failure); err != nil {
		t.Fatalf("exact expiry evidence missing: %v", err)
	}
	if err := manager.Stop(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.run); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory remains after exact stop cleanup: %v", err)
	}
}

func TestAcceptedHelperSurvivesInvokerTerminalGroupSignals(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin terminal-session contract")
	}
	tests := []struct {
		name, project, run string
		signal             syscall.Signal
	}{
		{name: "interrupt", signal: syscall.SIGINT, project: "ghijklmnopqrstuvwxyz", run: "01890f5c-7b00-7000-8000-000000000203"},
		{name: "hangup", signal: syscall.SIGHUP, project: "hijklmnopqrstuvwxyza", run: "01890f5c-7b00-7000-8000-000000000204"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testAcceptedHelperSurvivesInvokerGroupSignal(t, test.signal, test.project, test.run)
		})
	}
}

func testAcceptedHelperSurvivesInvokerGroupSignal(t *testing.T, signal syscall.Signal, project, run string) {
	t.Helper()
	manager := leaseLifetimeManager(t)
	identity := leaseTestIdentity(t, project, run)
	payload := leaseInvokerPayload{StateRoot: manager.stateRoot, Identity: identity, Specs: leaseLifetimeSpecs(t, 10*time.Second)}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(manager.executable.Path, "__bridge-test-invoker")
	command.Env = append(os.Environ(), leaseInvokerPayloadEnvironment+"="+base64.RawURLEncoding.EncodeToString(payloadBytes))
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var ready leaseInvokerReady
	if err := json.NewDecoder(stdout).Decode(&ready); err != nil {
		_ = command.Process.Kill()
		t.Fatal(err)
	}
	defer manager.Stop(context.Background(), identity)

	invokerGroup, err := syscall.Getpgid(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	helperGroup, err := syscall.Getpgid(ready.PID)
	if err != nil {
		t.Fatal(err)
	}
	helperSession, err := unix.Getsid(ready.PID)
	if err != nil {
		t.Fatal(err)
	}
	if helperGroup == invokerGroup || helperGroup != ready.PID || helperSession != ready.PID {
		t.Fatalf("helper pid/group/session = %d/%d/%d, invoker group %d", ready.PID, helperGroup, helperSession, invokerGroup)
	}
	if err := syscall.Kill(-invokerGroup, signal); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatalf("invoker unexpectedly survived terminal group signal %s", signal)
	}
	status, err := manager.Status(context.Background(), identity)
	if err != nil || status.State != "running" {
		t.Fatalf("detached helper status after invoker signal = %#v, %v", status, err)
	}
	if err := manager.Stop(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	paths := leaseLifetimePaths(manager.stateRoot, identity)
	absent, err := exactLeaseArtifactsAbsent(paths)
	if err != nil || !absent {
		t.Fatalf("exact artifacts absent after detached stop = %v, %v", absent, err)
	}
	if _, err := os.Lstat(filepath.Dir(paths.socket)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("detached helper run directory remains: %v", err)
	}
}
