package apple_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/ownership"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	runtimeapple "github.com/srimajji/dsx/internal/runtime/apple"
	"github.com/srimajji/dsx/internal/state"
	statefs "github.com/srimajji/dsx/internal/state/fs"
)

const (
	appleOptIn   = "DSX_RUN_APPLE_TESTS"
	pinnedImage  = "docker.io/library/alpine@sha256:2c9d26f410d032d5b1525aa8a873e238b05b90c4ae8618743d4311f0cc827e37"
	pinnedDigest = "sha256:2c9d26f410d032d5b1525aa8a873e238b05b90c4ae8618743d4311f0cc827e37"
)

type coreRuntime struct {
	executable string
	runner     runtimeapple.OSRunner
	adapter    *runtimeapple.Adapter
}

type coreInventory struct {
	Containers string
	Networks   string
	Volumes    string
	Builder    string
}

type coreFixture struct {
	t            *testing.T
	runtime      *coreRuntime
	adapter      runtime.Adapter
	service      *app.LifecycleService
	manifests    *statefs.ManifestRepository
	root         string
	stateRoot    string
	projectID    model.ProjectID
	hash         string
	before       coreInventory
	imageBuild   bool
	guestHelper  runtime.HostPath
	bridgeLeases bridge.LeaseManager
	execution    plan.ExecutionPlan
}
type hostFileEvidence struct {
	content []byte
	mode    os.FileMode
}

type shellInvocation struct {
	result app.ShellResult
	stdout string
	stderr string
	err    error
}

type failStartAdapter struct {
	runtime.Adapter
	err   error
	calls int
}

func (adapter *failStartAdapter) StartWorkspace(context.Context, runtime.ResourceSnapshot) error {
	adapter.calls++
	return adapter.err
}

type cancelAfterCreateAdapter struct {
	runtime.Adapter
	cancel  context.CancelFunc
	created runtime.Resource
}

func (adapter *cancelAfterCreateAdapter) CreateWorkspace(ctx context.Context, spec runtime.WorkspaceSpec) (runtime.Resource, error) {
	created, err := adapter.Adapter.CreateWorkspace(ctx, spec)
	if err == nil {
		adapter.created = created
		adapter.cancel()
	}
	return created, err
}

func TestCoreP0B1ProbeVersionGateAndPinnedImageAvailability(t *testing.T) {
	real := requireCoreRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	before := snapshotInventory(t, ctx, real)

	capabilities, err := real.adapter.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe() failed: %v", err)
	}
	if capabilities.HostOS != "Darwin" || capabilities.HostArch != "arm64" || capabilities.CLIVersion != "1.2.2" || capabilities.ServerVersion != "1.2.2" {
		t.Fatalf("unexpected admitted runtime: %#v", capabilities)
	}
	if capabilities.CompatibilityID != "apple-container/cli-1.2.2/server-1.2.2" || !capabilities.ServiceHealthy {
		t.Fatalf("runtime is not the admitted healthy compatibility pair: %#v", capabilities)
	}
	if !capabilities.MachineReadableInspection || !capabilities.Labels || !capabilities.Networks || !capabilities.Volumes || !capabilities.Copy {
		t.Fatalf("required Slice 3 capabilities are absent: %#v", capabilities)
	}
	if capabilities.FixedPublication || capabilities.DynamicPublication {
		t.Fatal("Apple container 1.2.2 port publication must remain gated off after the failed reachability experiment")
	}

	image, err := real.adapter.EnsureImage(ctx, runtime.ImageSpec{Reference: pinnedImage})
	if err != nil {
		t.Fatalf("EnsureImage(%q) failed: %v", pinnedImage, err)
	}
	if image.Digest != pinnedDigest {
		t.Fatalf("EnsureImage() digest = %q, want %q", image.Digest, pinnedDigest)
	}
	after := snapshotInventory(t, ctx, real)
	assertInventoryUnchanged(t, before, after)
}

func TestCoreC1E1T1V1M1N1L1Lifecycle(t *testing.T) {
	real := requireCoreRuntime(t)
	fixture := newCoreFixture(t, real, real.adapter, true, 0)
	defer fixture.recover()

	if err := os.WriteFile(filepath.Join(fixture.root, "host-before.txt"), []byte("host-before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := fixture.start(context.Background())
	if started.State != model.StateRunning || started.Existing {
		t.Fatalf("Start() = %#v", started)
	}
	if _, err := model.ParseRunID(string(started.RunID)); err != nil {
		t.Fatalf("production lifecycle did not generate a UUIDv7 run ID: %v", err)
	}

	manifest := fixture.oneManifest()
	if err := state.ValidateManifest(manifest); err != nil {
		t.Fatalf("running manifest is invalid: %v", err)
	}
	if manifest.PlanHash != fixture.hash || manifest.RunID != started.RunID || manifest.State != model.StateRunning {
		t.Fatalf("running manifest does not match approved lifecycle result: %#v", manifest)
	}
	if len(manifest.Resources) != 3 {
		t.Fatalf("manifest resources = %d, want network, volume, workspace", len(manifest.Resources))
	}

	var workspace runtime.ResourceSnapshot
	for _, record := range manifest.Resources {
		if len(record.Labels) != 7 {
			t.Fatalf("manifest resource %q has %d ownership labels, want exactly 7", record.Name, len(record.Labels))
		}
		wantLabels := state.ResourceOwnershipLabels(manifest.ProjectID, manifest.Sandbox, manifest.RunID, record.Kind, record.Role)
		if !reflect.DeepEqual(record.Labels, wantLabels) {
			t.Fatalf("manifest resource %q labels = %#v, want %#v", record.Name, record.Labels, wantLabels)
		}
		if !record.Created || record.RuntimeID != record.ExpectedID || record.ExpectedID != record.Name {
			t.Fatalf("manifest resource identity is incomplete: %#v", record)
		}
		observed, err := real.adapter.Inspect(context.Background(), runtime.ResourceID(record.RuntimeID))
		if err != nil {
			t.Fatalf("Inspect(%q) failed: %v", record.RuntimeID, err)
		}
		classification := ownership.Classify(&record, &observed)
		if classification.Outcome != ownership.OutcomeOwned || !classification.DeleteAllowed {
			t.Fatalf("resource %q classification = %#v", record.Name, classification)
		}
		if len(observed.Labels) != 7 {
			t.Fatalf("runtime resource %q has %d labels, want exactly 7", record.Name, len(observed.Labels))
		}
		if record.Kind == string(runtime.ResourceWorkspace) {
			workspace = observed
		}
	}
	assertLiveTopology(t, fixture.root, manifest, workspace)

	assertShellOutput(t, fixture, []string{"/bin/pwd"}, nil, "/workspace\n")
	assertShellOutput(t, fixture, []string{"/bin/cat", "/workspace/host-before.txt"}, nil, "host-before\n")
	uid := strings.TrimSpace(shellOutput(t, fixture, []string{"/usr/bin/id", "-u"}, nil))
	gid := strings.TrimSpace(shellOutput(t, fixture, []string{"/usr/bin/id", "-g"}, nil))
	if uid != strconv.Itoa(os.Getuid()) || gid != strconv.Itoa(os.Getgid()) || uid == "0" {
		t.Fatalf("workspace user = %s:%s, want nonroot host %d:%d", uid, gid, os.Getuid(), os.Getgid())
	}

	if err := os.WriteFile(filepath.Join(fixture.root, "host-live.txt"), []byte("host-live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertShellOutput(t, fixture, []string{"/bin/cat", "/workspace/host-live.txt"}, nil, "host-live\n")
	guestContent := "guest-live\n"
	assertShellOutput(t, fixture, []string{"/usr/bin/tee", "/workspace/guest-live.txt"}, strings.NewReader(guestContent), guestContent)
	readBack, err := os.ReadFile(filepath.Join(fixture.root, "guest-live.txt"))
	if err != nil || string(readBack) != guestContent {
		t.Fatalf("guest-to-host live edit = %q, %v", readBack, err)
	}

	stopped, err := fixture.service.Stop(context.Background(), app.StopRequest{Root: fixture.root})
	if err != nil || stopped.State != model.StateStopped || stopped.RunID != started.RunID {
		t.Fatalf("Stop() = %#v, %v", stopped, err)
	}
	cleaned := fixture.clean()
	if cleaned.DeletedResources != 3 || cleaned.DeletedManifests != 1 || len(cleaned.Preserved) != 0 {
		t.Fatalf("first Clean() = %#v", cleaned)
	}
	second := fixture.clean()
	if second.DeletedResources != 0 || second.DeletedManifests != 0 || len(second.Preserved) != 0 {
		t.Fatalf("second Clean() = %#v", second)
	}
	fixture.assertRecovered()
}

func TestCoreS1FixedLoopbackPublication(t *testing.T) {
	real := requireCoreRuntime(t)
	hostPort := reserveLoopbackPort(t)
	fixture := newCoreFixture(t, real, real.adapter, false, int(hostPort))
	defer fixture.recover()
	buildGuestHTTPServer(t, fixture.root)
	assertCoreHTTPPublication(t, real, fixture, "127.0.0.1", hostPort)
}

func TestCoreS1DynamicLoopbackPublication(t *testing.T) {
	real := requireCoreRuntime(t)
	fixture := newCoreFixture(t, real, real.adapter, false, -1)
	defer fixture.recover()
	buildGuestHTTPServer(t, fixture.root)
	assertCoreHTTPPublication(t, real, fixture, "127.0.0.1", 0)
}

func TestCoreS1DynamicExplicitNonLoopbackPublication(t *testing.T) {
	real := requireCoreRuntime(t)
	fixture := newCoreFixtureWithBind(t, real, real.adapter, false, -1, "0.0.0.0")
	defer fixture.recover()
	buildGuestHTTPServer(t, fixture.root)
	assertCoreHTTPPublication(t, real, fixture, "0.0.0.0", 0)
}

func assertCoreHTTPPublication(t *testing.T, real *coreRuntime, fixture *coreFixture, expectedHost string, expectedHostPort uint16) {
	t.Helper()
	started := fixture.start(context.Background())
	if len(started.URLs) != 1 {
		t.Fatalf("verified publication URLs = %#v", started.URLs)
	}
	parsed, err := url.Parse(started.URLs[0])
	if err != nil || parsed.Hostname() != expectedHost {
		t.Fatalf("publication URL = %q, %v", started.URLs[0], err)
	}
	portNumber, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil || portNumber == 0 || (expectedHostPort != 0 && uint16(portNumber) != expectedHostPort) {
		t.Fatalf("publication URL port = %q, %v; expected %d", parsed.Port(), err, expectedHostPort)
	}
	manifest := fixture.oneManifest()
	if len(manifest.HostBindings) != 1 || manifest.HostBindings[0].Name != "web" || manifest.HostBindings[0].HostIP.String() != expectedHost || manifest.HostBindings[0].HostPort != uint16(portNumber) || manifest.HostBindings[0].GuestPort != 3000 || manifest.HostBindings[0].Protocol != "tcp" {
		t.Fatalf("durable helper binding = %#v", manifest.HostBindings)
	}
	workspaceRecord := manifestRecord(t, manifest, string(runtime.ResourceWorkspace), "workspace")
	workspace, err := real.adapter.Inspect(context.Background(), runtime.ResourceID(workspaceRecord.RuntimeID))
	if err != nil {
		t.Fatal(err)
	}
	if len(workspace.Ports) != 0 {
		t.Fatalf("Apple fallback workspace has native published ports: %#v", workspace.Ports)
	}
	assertShellOutput(t, fixture, []string{"/bin/cp", "/workspace/dsx-test-http-server", "/tmp/dsx-test-http-server"}, nil, "")
	assertShellOutput(t, fixture, []string{"/bin/chmod", "700", "/tmp/dsx-test-http-server"}, nil, "")
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelServer()
	serverDone := make(chan shellInvocation, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		result, shellErr := fixture.service.Shell(serverCtx, app.ShellRequest{
			Root: fixture.root, Argv: []string{"/tmp/dsx-test-http-server"},
			Stdout: &stdout, Stderr: &stderr,
		})
		serverDone <- shellInvocation{result: result, stdout: stdout.String(), stderr: stderr.String(), err: shellErr}
	}()
	if err := waitForFile(filepath.Join(fixture.root, "dsx-http-listening"), 5*time.Second); err != nil {
		t.Fatalf("guest HTTP helper did not prove its listener ready: %v", err)
	}
	requestURL := started.URLs[0]
	if expectedHost == "0.0.0.0" {
		parsed.Host = net.JoinHostPort("127.0.0.1", parsed.Port())
		requestURL = parsed.String()
	}
	body, requestErr := getEventually(requestURL, 8*time.Second)
	if requestErr != nil {
		cancelServer()
		server := <-serverDone
		_, acceptedErr := os.Stat(filepath.Join(fixture.root, "dsx-http-accepted"))
		t.Fatalf("guest HTTP server at %s failed: request=%v guestAccepted=%t server=%v stderr=%q", requestURL, requestErr, acceptedErr == nil, server.err, server.stderr)
	}
	server := <-serverDone
	if server.err != nil || server.result.Exit.Code == nil || *server.result.Exit.Code != 0 || server.result.Exit.Signal != "" {
		t.Fatalf("guest HTTP server Shell exit = %#v, error=%v stderr=%q", server.result.Exit, server.err, server.stderr)
	}
	if body != "dsx-apple-fixed-port\n" {
		t.Fatalf("GET %s = %q", started.URLs[0], body)
	}
	if _, err := fixture.service.Stop(context.Background(), app.StopRequest{Root: fixture.root}); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}
	fixture.clean()
	fixture.assertRecovered()
}

func TestCoreCP1RollbackCancellationAndUnrelatedBuilderPreservation(t *testing.T) {
	real := requireCoreRuntime(t)

	t.Run("injected_post_create_startup_failure", func(t *testing.T) {
		injected := errors.New("DSX-025 injected post-create startup failure")
		fault := &failStartAdapter{Adapter: real.adapter, err: injected}
		fixture := newCoreFixture(t, real, fault, true, 0)
		defer fixture.recover()

		_, err := fixture.service.Start(context.Background(), app.StartRequest{Root: fixture.root, ApproveConfig: fixture.hash})
		if !errors.Is(err, injected) || fault.calls != 1 {
			t.Fatalf("Start() = %v, injected calls = %d", err, fault.calls)
		}
		fixture.clean()
		fixture.assertRecovered()
	})

	t.Run("context_cancellation_after_runtime_create", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fault := &cancelAfterCreateAdapter{Adapter: real.adapter, cancel: cancel}
		fixture := newCoreFixture(t, real, fault, true, 0)
		defer fixture.recover()

		_, err := fixture.service.Start(ctx, app.StartRequest{Root: fixture.root, ApproveConfig: fixture.hash})
		if err == nil || fault.created.ID == "" {
			t.Fatalf("Start() = %v, created = %#v", err, fault.created)
		}
		fixture.clean()
		fixture.assertRecovered()
	})
}

func TestCoreGuestReadinessRecoveryFailureAndCleanup(t *testing.T) {
	real := requireCoreRuntime(t)
	fixture := newCoreFixture(t, real, real.adapter, false, 0)
	defer fixture.recover()
	writeCoreGuestConfig(t, fixture.root)
	inspected, err := app.NewInspectionService(plan.NewResolver()).Inspect(context.Background(), app.InspectRequest{Root: fixture.root, SandboxName: "main", Mode: string(model.ModeLive)})
	if err != nil {
		t.Fatal(err)
	}
	fixture.hash = inspected.Plan.ExecutableHash
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fixture.start(ctx)
	status, err := fixture.service.ProcessStatus(ctx, app.ProcessStatusRequest{Root: fixture.root})
	if err != nil || status.Failed || len(status.Processes) != 1 || !status.Processes[0].Ready {
		t.Fatalf("initial guest status = %#v, %v", status, err)
	}
	var noNewPrivileges bytes.Buffer
	shell, err := fixture.service.Shell(ctx, app.ShellRequest{Root: fixture.root, Argv: []string{"/bin/sh", "-c", "grep '^NoNewPrivs:' /proc/self/status"}, Stdout: &noNewPrivileges})
	if err != nil || shell.Exit.Code == nil || *shell.Exit.Code != 0 || !strings.Contains(noNewPrivileges.String(), "NoNewPrivs:\t1") {
		t.Fatalf("NNP shell = %#v stdout=%q err=%v", shell, noNewPrivileges.String(), err)
	}
	manifest := fixture.oneManifest()
	workspaceRecord := manifestRecord(t, manifest, string(runtime.ResourceWorkspace), "workspace")
	workspace, err := fixture.adapter.Inspect(ctx, runtime.ResourceID(workspaceRecord.ExpectedID))
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.adapter.Signal(ctx, workspace, runtime.Signal("KILL")); err != nil {
		t.Fatalf("kill guest PID1: %v", err)
	}
	fixture.start(ctx)
	status, err = fixture.service.ProcessStatus(ctx, app.ProcessStatusRequest{Root: fixture.root})
	if err != nil || status.Failed || len(status.Processes) != 1 || !status.Processes[0].Ready {
		t.Fatalf("recovered guest status = %#v, %v", status, err)
	}
	trigger, err := fixture.service.Shell(ctx, app.ShellRequest{Root: fixture.root, Argv: []string{"/bin/sh", "-c", "printf x > /workspace/fail"}})
	if err != nil || trigger.Exit.Code == nil || *trigger.Exit.Code != 0 {
		t.Fatalf("trigger required failure = %#v, %v", trigger, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err = fixture.service.ProcessStatus(ctx, app.ProcessStatusRequest{Root: fixture.root})
		if err == nil && status.Failed && len(status.Processes) == 1 && status.Processes[0].Exit != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("required failure not observed: status=%#v err=%v", status, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if manifest = fixture.oneManifest(); manifest.State != model.StateFailed {
		t.Fatalf("required failure manifest = %#v", manifest)
	}
	fixture.clean()
	second, err := fixture.service.Clean(ctx, app.CleanRequest{Root: fixture.root, Confirmed: true})
	if err != nil || second.DeletedManifests != 0 || second.DeletedResources != 0 {
		t.Fatalf("repeated clean = %#v, %v", second, err)
	}
	fixture.assertRecovered()
}

func TestCoreIntegratedServiceAndPublicationExternalGates(t *testing.T) {
	real := requireCoreRuntime(t)
	fixture := newCompositeCoreFixture(t, real)
	defer fixture.recover()
	hostFiles := snapshotHostFiles(t, fixture.root, []string{".dsx/config.jsonc", "host-sentinel.txt", "api/member.txt", "web/member.txt"})
	if len(fixture.execution.Repositories) != 2 ||
		fixture.execution.Repositories[0].Name != "api" ||
		fixture.execution.Repositories[0].HostPath != filepath.Join(fixture.root, "api") ||
		fixture.execution.Repositories[0].GuestPath != "/workspace/api" ||
		fixture.execution.Repositories[1].Name != "web" ||
		fixture.execution.Repositories[1].HostPath != filepath.Join(fixture.root, "web") ||
		fixture.execution.Repositories[1].GuestPath != "/workspace/web" {
		t.Fatalf("composite repository plan = %#v", fixture.execution.Repositories)
	}
	started := startComposite(t, fixture)
	if started.State != model.StateRunning || started.Existing {
		t.Fatalf("composite Start() = %#v", started)
	}
	if len(started.URLs) != 1 {
		t.Fatalf("composite publication URLs = %#v, want only Caddy", started.URLs)
	}
	published, err := url.Parse(started.URLs[0])
	if err != nil || published.Scheme != "http" || published.Hostname() != "127.0.0.1" {
		t.Fatalf("Caddy publication URL = %q, %v", started.URLs[0], err)
	}
	hostPort, err := strconv.ParseUint(published.Port(), 10, 16)
	if err != nil || hostPort == 0 {
		t.Fatalf("dynamic Caddy host port = %q, %v", published.Port(), err)
	}
	builderAfterStart := snapshotInventory(t, context.Background(), real).Builder

	manifest := fixture.oneManifest()
	if len(manifest.HostBindings) != 1 {
		t.Fatalf("durable Caddy bindings = %#v", manifest.HostBindings)
	}
	binding := manifest.HostBindings[0]
	if binding.Name != "caddy" || binding.HostIP.String() != "127.0.0.1" || binding.HostPort != uint16(hostPort) || binding.GuestPort != 8080 || binding.Protocol != "tcp" {
		t.Fatalf("durable Caddy binding = %#v", binding)
	}
	leaseIdentity := bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}
	leaseStatus, err := fixture.bridgeLeases.Status(context.Background(), leaseIdentity)
	if err != nil || leaseStatus.State != "running" || len(leaseStatus.Result.Bindings) != 1 {
		t.Fatalf("running publication lease = %#v, %v", leaseStatus, err)
	}
	workspaceRecord := manifestRecord(t, manifest, string(runtime.ResourceWorkspace), "workspace")
	workspace, err := real.adapter.Inspect(context.Background(), runtime.ResourceID(workspaceRecord.RuntimeID))
	if err != nil {
		t.Fatalf("inspect composite workspace: %v", err)
	}
	if len(workspace.Ports) != 0 {
		t.Fatalf("fallback-published composite workspace has native ports: %#v", workspace.Ports)
	}

	statusContext, cancelStatus := context.WithTimeout(context.Background(), 30*time.Second)
	status, err := fixture.service.ProcessStatus(statusContext, app.ProcessStatusRequest{Root: fixture.root})
	cancelStatus()
	if err != nil {
		t.Fatalf("composite ProcessStatus() failed: %v", err)
	}
	if status.Generation == 0 || status.Failed || !reflect.DeepEqual(status.URLs, started.URLs) || len(status.Processes) != 4 {
		t.Fatalf("durable composite process/port status = %#v", status)
	}
	expectedProcesses := map[string]struct{}{"mariadb": {}, "redis": {}, "app": {}, "caddy": {}}
	for _, process := range status.Processes {
		if _, exists := expectedProcesses[process.ID]; !exists {
			t.Fatalf("unexpected composite process status: %#v", process)
		}
		delete(expectedProcesses, process.ID)
		if process.Generation != status.Generation || process.State != guestproto.StateReady || !process.Ready || !process.Required || process.Exit != nil || process.Failure != "" {
			t.Fatalf("composite process is not durably ready: %#v", process)
		}
	}
	if len(expectedProcesses) != 0 {
		t.Fatalf("missing composite process statuses: %#v", expectedProcesses)
	}

	if output := shellOutput(t, fixture, []string{"/usr/bin/redis-cli", "-h", "127.0.0.1", "-p", "6379", "ping"}, nil); output != "PONG\n" {
		t.Fatalf("real Redis PING = %q", output)
	}
	mariaDBPing := shellOutput(t, fixture, []string{"/usr/bin/mariadb-admin", "--protocol=tcp", "--host=127.0.0.1", "--port=3306", "--user=root", "ping"}, nil)
	if !strings.Contains(mariaDBPing, "mysqld is alive") {
		t.Fatalf("real MariaDB ping = %q", mariaDBPing)
	}
	body, requestErr := getEventually(started.URLs[0], 15*time.Second)
	if requestErr != nil || body != "mariadb=ready redis=ready\n" {
		t.Fatalf("GET Caddy publication %s = %q, %v", started.URLs[0], body, requestErr)
	}
	response, err := http.Get(started.URLs[0])
	if err != nil {
		t.Fatalf("GET Caddy marker %s: %v", started.URLs[0], err)
	}
	_ = response.Body.Close()
	if response.Header.Get("X-DSX-Composite") != "caddy" {
		t.Fatalf("published response did not traverse Caddy: headers=%#v", response.Header)
	}
	assertLoopbackOnlyPublication(t, published)
	assertShellOutput(t, fixture, []string{"/bin/cat", "/workspace/api/member.txt"}, nil, "api\n")
	assertShellOutput(t, fixture, []string{"/bin/cat", "/workspace/web/member.txt"}, nil, "web\n")

	stopped, err := fixture.service.Stop(context.Background(), app.StopRequest{Root: fixture.root})
	if err != nil || stopped.State != model.StateStopped || stopped.RunID != started.RunID {
		t.Fatalf("composite Stop() = %#v, %v", stopped, err)
	}
	stoppedManifest := fixture.oneManifest()
	if stoppedManifest.State != model.StateStopped || !reflect.DeepEqual(stoppedManifest.HostBindings, manifest.HostBindings) {
		t.Fatalf("stopped manifest lost durable publication state: %#v", stoppedManifest)
	}
	stoppedWorkspace, err := real.adapter.Inspect(context.Background(), runtime.ResourceID(workspaceRecord.RuntimeID))
	if err != nil || stoppedWorkspace.State != "stopped" {
		t.Fatalf("stopped composite workspace = %#v, %v", stoppedWorkspace, err)
	}
	assertHTTPPublicationClosed(t, started.URLs[0], 5*time.Second)
	leaseStatus, err = fixture.bridgeLeases.Status(context.Background(), leaseIdentity)
	if err != nil || leaseStatus.State != "absent" {
		t.Fatalf("stopped publication lease = %#v, %v", leaseStatus, err)
	}
	if afterStop := snapshotInventory(t, context.Background(), real).Builder; afterStop != builderAfterStart {
		t.Fatalf("Apple builder changed during composite Stop\nbefore: %s\nafter:  %s", builderAfterStart, afterStop)
	}

	firstClean := fixture.clean()
	if firstClean.DeletedResources != 2 || firstClean.DeletedManifests != 1 || len(firstClean.Preserved) != 0 {
		t.Fatalf("first composite Clean() = %#v", firstClean)
	}
	assertHostFiles(t, fixture.root, hostFiles)
	if afterClean := snapshotInventory(t, context.Background(), real).Builder; afterClean != builderAfterStart {
		t.Fatalf("Apple builder changed during composite Clean\nbefore: %s\nafter:  %s", builderAfterStart, afterClean)
	}
	secondClean := fixture.clean()
	if secondClean.DeletedResources != 0 || secondClean.DeletedManifests != 0 || len(secondClean.Preserved) != 0 {
		t.Fatalf("second composite Clean() = %#v", secondClean)
	}
	assertHostFiles(t, fixture.root, hostFiles)
	leaseStatus, err = fixture.bridgeLeases.Status(context.Background(), leaseIdentity)
	if err != nil || leaseStatus.State != "absent" {
		t.Fatalf("cleaned publication lease = %#v, %v", leaseStatus, err)
	}
	if afterSecondClean := snapshotInventory(t, context.Background(), real).Builder; afterSecondClean != builderAfterStart {
		t.Fatalf("Apple builder changed during repeated composite Clean\nbefore: %s\nafter:  %s", builderAfterStart, afterSecondClean)
	}
	fixture.assertRecovered()
}

func newCompositeCoreFixture(t *testing.T, real *coreRuntime) *coreFixture {
	t.Helper()
	fixture := newCoreFixtureWithConfig(t, real, real.adapter, func(root string) {
		for _, member := range []string{"api", "web"} {
			memberRoot := filepath.Join(root, member)
			if err := os.Mkdir(memberRoot, 0o700); err != nil {
				t.Fatalf("create composite member %s: %v", member, err)
			}
			if err := os.WriteFile(filepath.Join(memberRoot, "member.txt"), []byte(member+"\n"), 0o600); err != nil {
				t.Fatalf("write composite member %s: %v", member, err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "host-sentinel.txt"), []byte("host-preserved\n"), 0o600); err != nil {
			t.Fatalf("write composite host sentinel: %v", err)
		}
		fixtureRoot := filepath.Join(root, "composite")
		if err := os.CopyFS(fixtureRoot, os.DirFS("composite")); err != nil {
			t.Fatalf("copy permanent composite fixture: %v", err)
		}
		writeCompositeCoreConfig(t, root)
	})
	fixture.imageBuild = true
	return fixture
}

func writeCompositeCoreConfig(t *testing.T, root string) {
	t.Helper()
	configuration := `{
  "schemaVersion": 1,
  "workspace": {"root": ".", "members": [{"name": "api", "path": "api"}, {"name": "web", "path": "web"}]},
  "image": {"build": {"context": "composite", "file": "composite/Containerfile"}},
  "setup": [
    {"argv": ["/opt/dsx-composite/initialize-mariadb.sh"], "cwd": "/tmp"}
  ],
  "processes": {
    "mariadb": {
      "argv": ["/usr/bin/mariadbd", "--no-defaults", "--datadir=/tmp/dsx-composite-mariadb", "--socket=/tmp/dsx-composite-mariadb.sock", "--pid-file=/tmp/dsx-composite-mariadb.pid", "--bind-address=127.0.0.1", "--port=3306", "--skip-networking=0", "--skip-grant-tables", "--skip-log-bin", "--skip-name-resolve"],
      "cwd": "/tmp",
      "required": true,
      "health": {"command": {"argv": ["/opt/dsx-composite/health-mariadb.sh"]}, "interval": "250ms", "timeout": "2s", "retries": 100}
    },
    "redis": {
      "argv": ["/usr/bin/redis-server", "/opt/dsx-composite/redis.conf"],
      "cwd": "/tmp",
      "required": true,
      "health": {"command": {"argv": ["/opt/dsx-composite/health-redis.sh"]}, "interval": "250ms", "timeout": "2s", "retries": 100}
    },
    "app": {
      "argv": ["/usr/bin/node", "/opt/dsx-composite/app.js"],
      "cwd": "/opt/dsx-composite",
      "dependsOn": ["mariadb", "redis"],
      "required": true,
      "health": {"http": {"url": "http://127.0.0.1:3000/health"}, "interval": "250ms", "timeout": "2s", "retries": 100}
    },
    "caddy": {
      "argv": ["/opt/dsx-composite/start-caddy.sh"],
      "cwd": "/opt/dsx-composite",
      "dependsOn": ["app"],
      "required": true,
      "health": {"http": {"url": "http://127.0.0.1:8080/health"}, "interval": "250ms", "timeout": "2s", "retries": 100}
    }
  },
  "ports": [
    {"name": "caddy", "guest": 8080, "host": "dynamic", "bind": "127.0.0.1", "protocol": "tcp"}
  ]
}`
	if err := os.WriteFile(filepath.Join(root, ".dsx", "config.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatalf("write composite DSX config: %v", err)
	}
}

func startComposite(t *testing.T, fixture *coreFixture) app.StartResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	started, err := fixture.service.Start(ctx, app.StartRequest{Root: fixture.root, ApproveConfig: fixture.hash})
	if err != nil {
		t.Fatalf("physical composite lifecycle failed on its first start: %v", err)
	}
	return started
}

func assertHTTPPublicationClosed(t *testing.T, address string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(timeout)
	var closedAt time.Time
	for time.Now().Before(deadline) {
		response, err := client.Get(address)
		if err == nil {
			_ = response.Body.Close()
			if !closedAt.IsZero() {
				t.Fatalf("stopped Caddy publication %s became reachable again", address)
			}
		} else if closedAt.IsZero() {
			closedAt = time.Now()
		} else if time.Since(closedAt) >= time.Second {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("stopped Caddy publication %s did not remain closed for one second within %s", address, timeout)
}

func assertLoopbackOnlyPublication(t *testing.T, published *url.URL) {
	t.Helper()
	address := hostNonLoopbackIPv4(t)
	probe := *published
	probe.Host = net.JoinHostPort(address.String(), published.Port())
	client := &http.Client{
		Timeout: 750 * time.Millisecond,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			Proxy:             nil,
		},
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(probe.String())
	if err == nil {
		_ = response.Body.Close()
		t.Fatalf("loopback publication was reachable through host address %s", address)
	}
}

func hostNonLoopbackIPv4(t *testing.T) net.IP {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("list host interfaces: %v", err)
	}
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 || networkInterface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, addressErr := networkInterface.Addrs()
		if addressErr != nil {
			t.Fatalf("list addresses for %s: %v", networkInterface.Name, addressErr)
		}
		for _, candidate := range addresses {
			ip, _, parseErr := net.ParseCIDR(candidate.String())
			if parseErr == nil && ip.To4() != nil && !ip.IsUnspecified() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsMulticast() {
				return ip.To4()
			}
		}
	}
	t.Fatal("host has no active non-loopback IPv4 address for publication isolation probe")
	return nil
}

func snapshotHostFiles(t *testing.T, root string, paths []string) map[string]hostFileEvidence {
	t.Helper()
	result := make(map[string]hostFileEvidence, len(paths))
	for _, path := range paths {
		absolute := filepath.Join(root, path)
		content, err := os.ReadFile(absolute)
		if err != nil {
			t.Fatalf("read host sentinel %s: %v", path, err)
		}
		info, err := os.Lstat(absolute)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("inspect host sentinel %s: mode=%v err=%v", path, info, err)
		}
		result[path] = hostFileEvidence{content: content, mode: info.Mode().Perm()}
	}
	return result
}

func assertHostFiles(t *testing.T, root string, expected map[string]hostFileEvidence) {
	t.Helper()
	for path, want := range expected {
		absolute := filepath.Join(root, path)
		content, err := os.ReadFile(absolute)
		if err != nil {
			t.Fatalf("read preserved host file %s: %v", path, err)
		}
		info, err := os.Lstat(absolute)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != want.mode || !bytes.Equal(content, want.content) {
			t.Fatalf("host file %s changed: mode=%v content=%q err=%v", path, info, content, err)
		}
	}
}

func TestCoreNativeDynamicPublicationUnsupported(t *testing.T) {
	requireCoreRuntime(t)
	t.Skip("Apple container 1.2.2 native dynamic publication is an explicit unsupported capability")
}

func requireCoreRuntime(t *testing.T) *coreRuntime {
	t.Helper()
	if os.Getenv(appleOptIn) != "1" {
		t.Skipf("destructive Apple compatibility tests disabled; set %s=1 on a dedicated Apple-silicon host", appleOptIn)
	}
	executable, err := runtimeapple.DiscoverContainerExecutable()
	if err != nil {
		t.Fatalf("discover trusted Apple container executable: %v", err)
	}
	runner := runtimeapple.OSRunner{}
	adapter, err := runtimeapple.NewAdapter(runner, executable)
	if err != nil {
		t.Fatalf("construct Apple adapter: %v", err)
	}
	return &coreRuntime{executable: executable, runner: runner, adapter: adapter}
}

func newCoreFixture(t *testing.T, real *coreRuntime, adapter runtime.Adapter, withVolume bool, hostPort int) *coreFixture {
	return newCoreFixtureWithBind(t, real, adapter, withVolume, hostPort, "127.0.0.1")
}

func newCoreFixtureWithBind(t *testing.T, real *coreRuntime, adapter runtime.Adapter, withVolume bool, hostPort int, bind string) *coreFixture {
	t.Helper()
	return newCoreFixtureWithConfig(t, real, adapter, func(root string) {
		writeCoreConfigWithBind(t, root, withVolume, hostPort, bind)
	})
}

func newCoreFixtureWithConfig(t *testing.T, real *coreRuntime, adapter runtime.Adapter, configure func(string)) *coreFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := snapshotInventory(t, ctx, real)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, ".dsx")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	configure(root)
	stateRoot := filepath.Join(t.TempDir(), "state")
	manifests, err := statefs.NewManifestRepository(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := statefs.NewApprovalRepository(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	inspection := app.NewInspectionService(plan.NewResolver())
	inspected, err := inspection.Inspect(context.Background(), app.InspectRequest{Root: root, SandboxName: "main", Mode: string(model.ModeLive)})
	if err != nil {
		t.Fatalf("inspect fixture plan: %v", err)
	}
	helperPath, err := filepath.Abs(filepath.Join("..", "..", "bin", "dsx-guest"))
	if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.EvalSymlinks(helperPath)
	if err != nil {
		t.Fatalf("resolve built dsx-guest helper: %v", err)
	}
	helperCacheParent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stagedHelper, err := app.StageGuestHelper(runtime.HostPath(helper), filepath.Join(helperCacheParent, "guest-helper"))
	if err != nil {
		t.Fatalf("stage built dsx-guest helper: %v", err)
	}
	dsxExecutable, err := filepath.Abs(filepath.Join("..", "..", "bin", "dsx"))
	if err != nil {
		t.Fatal(err)
	}
	dsxExecutable, err = filepath.EvalSymlinks(dsxExecutable)
	if err != nil {
		t.Fatalf("resolve built dsx helper: %v", err)
	}
	containerExecutable, err := runtimeapple.DiscoverContainerExecutable()
	if err != nil {
		t.Fatalf("discover Apple container executable: %v", err)
	}
	bridgeLeases, err := bridge.NewProductionLeaseManagerWithContainer(stateRoot, dsxExecutable, containerExecutable)
	if err != nil {
		t.Fatalf("construct host relay manager: %v", err)
	}
	service := app.NewLifecycleService(app.LifecycleDependencies{
		Inspection:   inspection,
		Approvals:    approvals,
		Manifests:    manifests,
		Locks:        manifests,
		Runtime:      adapter,
		Guest:        app.NewGuestClient(adapter),
		BridgeLeases: bridgeLeases,
		GuestHelperSource: func() (runtime.HostPath, error) {
			return stagedHelper, nil
		},
	})
	return &coreFixture{t: t, runtime: real, adapter: adapter, service: service, manifests: manifests, root: root, stateRoot: stateRoot, projectID: inspected.Plan.Project.ID, hash: inspected.Plan.ExecutableHash, before: before, guestHelper: stagedHelper, bridgeLeases: bridgeLeases, execution: inspected.Plan}
}

func writeCoreConfig(t *testing.T, root string, withVolume bool, hostPort int) {
	writeCoreConfigWithBind(t, root, withVolume, hostPort, "127.0.0.1")
}

func writeCoreConfigWithBind(t *testing.T, root string, withVolume bool, hostPort int, bind string) {
	t.Helper()
	volume := ""
	if withVolume {
		volume = `,"volumes":{"cache":{"target":"/cache","scope":"sandbox"}}`
	}
	port := ""
	if hostPort > 0 {
		port = fmt.Sprintf(`,"ports":[{"name":"web","guest":3000,"host":%d,"bind":%q,"protocol":"tcp"}]`, hostPort, bind)
	} else if hostPort < 0 {
		port = fmt.Sprintf(`,"ports":[{"name":"web","guest":3000,"host":"dynamic","bind":%q,"protocol":"tcp"}]`, bind)
	}
	configuration := `{"schemaVersion":1,"workspace":{"root":"."},"image":{"ref":"` + pinnedImage + `"}` + volume + port + `}`
	directory := filepath.Join(root, ".dsx")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeCoreGuestConfig(t *testing.T, root string) {
	t.Helper()
	configuration := `{"schemaVersion":1,"workspace":{"root":"."},"image":{"ref":"` + pinnedImage + `"},"processes":{"worker":{"argv":["/bin/sh","-c","while [ ! -e /workspace/fail ]; do sleep 1; done; exit 17"],"required":true,"health":{"command":{"argv":["/bin/true"]},"interval":"100ms","timeout":"2s","retries":10}}}}`
	if err := os.WriteFile(filepath.Join(root, ".dsx", "config.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture *coreFixture) start(ctx context.Context) app.StartResult {
	fixture.t.Helper()
	started, err := fixture.service.Start(ctx, app.StartRequest{Root: fixture.root, ApproveConfig: fixture.hash})
	if err != nil {
		fixture.t.Fatalf("Start() failed: %v", err)
	}
	return started
}

func (fixture *coreFixture) oneManifest() state.Manifest {
	fixture.t.Helper()
	manifests, err := fixture.manifests.ListProjectManifests(context.Background(), fixture.projectID)
	if err != nil || len(manifests) != 1 {
		fixture.t.Fatalf("project manifests = %#v, %v", manifests, err)
	}
	return manifests[0]
}

func (fixture *coreFixture) clean() app.CleanResult {
	fixture.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cleaned, err := fixture.service.Clean(ctx, app.CleanRequest{Root: fixture.root, Confirmed: true})
	if err != nil {
		fixture.t.Fatalf("LifecycleService.Clean recovery failed: %v", err)
	}
	return cleaned
}

func (fixture *coreFixture) recover() {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := fixture.service.Clean(ctx, app.CleanRequest{Root: fixture.root, Confirmed: true}); err != nil {
		fixture.t.Errorf("deferred LifecycleService.Clean recovery failed: %v", err)
	}
	fixture.assertRecoveredWithContext(ctx)
}

func (fixture *coreFixture) assertRecovered() {
	fixture.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture.assertRecoveredWithContext(ctx)
}

func (fixture *coreFixture) assertRecoveredWithContext(ctx context.Context) {
	manifests, err := fixture.manifests.ListProjectManifests(ctx, fixture.projectID)
	if err != nil || len(manifests) != 0 {
		fixture.t.Errorf("project manifests remain after recovery: %#v, %v", manifests, err)
	}
	after := snapshotInventoryNoFail(ctx, fixture.runtime)
	if after.err != nil {
		fixture.t.Errorf("snapshot runtime after recovery: %v", after.err)
		return
	}
	if strings.Contains(after.inventory.Containers, string(fixture.projectID)) || strings.Contains(after.inventory.Networks, string(fixture.projectID)) || strings.Contains(after.inventory.Volumes, string(fixture.projectID)) {
		fixture.t.Errorf("current project labels or names remain in runtime inventory: project=%s inventory=%#v", fixture.projectID, after.inventory)
	}
	if fixture.imageBuild {
		assertBuildInventoryUnchanged(fixture.t, fixture.before, after.inventory)
	} else {
		assertInventoryUnchanged(fixture.t, fixture.before, after.inventory)
	}
}

func assertLiveTopology(t *testing.T, root string, manifest state.Manifest, workspace runtime.ResourceSnapshot) {
	t.Helper()
	if workspace.State != "running" {
		t.Fatalf("workspace state = %q", workspace.State)
	}
	if len(workspace.Networks) != 1 {
		t.Fatalf("workspace networks = %#v", workspace.Networks)
	}
	network := manifestRecord(t, manifest, string(runtime.ResourceNetwork), "network")
	if workspace.Networks[0] != network.Name {
		t.Fatalf("workspace network = %q, want %q", workspace.Networks[0], network.Name)
	}
	cache := manifestRecordByKind(t, manifest, string(runtime.ResourceVolume))
	var liveMount, cacheMount bool
	for _, mount := range workspace.Mounts {
		if mount.Source == root && mount.Target == "/workspace" && mount.Type == "bind" && !mount.ReadOnly {
			liveMount = true
		}
		if mount.Source == cache.Name && mount.Target == "/cache" && mount.Type == "volume" && !mount.ReadOnly {
			cacheMount = true
		}
	}
	if !liveMount || !cacheMount {
		t.Fatalf("canonical live/volume mounts not observed: %#v", workspace.Mounts)
	}
}

func manifestRecord(t *testing.T, manifest state.Manifest, kind, role string) state.ResourceRecord {
	t.Helper()
	for _, record := range manifest.Resources {
		if record.Kind == kind && record.Role == role {
			return record
		}
	}
	t.Fatalf("manifest has no %s/%s record: %#v", kind, role, manifest.Resources)
	return state.ResourceRecord{}
}

func manifestRecordByKind(t *testing.T, manifest state.Manifest, kind string) state.ResourceRecord {
	t.Helper()
	for _, record := range manifest.Resources {
		if record.Kind == kind {
			return record
		}
	}
	t.Fatalf("manifest has no %s record: %#v", kind, manifest.Resources)
	return state.ResourceRecord{}
}

func shellOutput(t *testing.T, fixture *coreFixture, argv []string, stdin io.Reader) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	result, err := fixture.service.Shell(context.Background(), app.ShellRequest{Root: fixture.root, Argv: append([]string(nil), argv...), Stdin: stdin, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatalf("Shell(%q) failed: %v (stderr %q)", argv, err, stderr.String())
	}
	if result.Exit.Code == nil || *result.Exit.Code != 0 || result.Exit.Signal != "" {
		t.Fatalf("Shell(%q) exit = %#v (stderr %q)", argv, result.Exit, stderr.String())
	}
	return stdout.String()
}

func assertShellOutput(t *testing.T, fixture *coreFixture, argv []string, stdin io.Reader, want string) {
	t.Helper()
	if got := shellOutput(t, fixture, argv, stdin); got != want {
		t.Fatalf("Shell(%q) stdout = %q, want %q", argv, got, want)
	}
}

func buildGuestHTTPServer(t *testing.T, root string) {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	environment := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "CGO_ENABLED=") || strings.HasPrefix(entry, "GOOS=") || strings.HasPrefix(entry, "GOARCH=") {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment, "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	command := exec.Command("go", "build", "-o", filepath.Join(root, "dsx-test-http-server"), "./testserver")
	command.Dir = workingDirectory
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Linux/arm64 HTTP test helper: %v: %s", err, output)
	}
}

func reserveLoopbackPort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return uint16(port)
}

func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else {
			last = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	return last
}

func getEventually(url string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 1024))
			closeErr := response.Body.Close()
			if response.StatusCode == http.StatusOK && readErr == nil && closeErr == nil {
				return string(body), nil
			}
			last = errors.Join(readErr, closeErr, fmt.Errorf("status %s", response.Status))
		} else {
			last = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", last
}

func snapshotInventory(t *testing.T, ctx context.Context, real *coreRuntime) coreInventory {
	t.Helper()
	result := snapshotInventoryNoFail(ctx, real)
	if result.err != nil {
		t.Fatalf("snapshot Apple runtime inventory: %v", result.err)
	}
	assertBuilderPresent(t, result.inventory.Builder)
	return result.inventory
}

type inventoryResult struct {
	inventory coreInventory
	err       error
}

func snapshotInventoryNoFail(ctx context.Context, real *coreRuntime) inventoryResult {
	containers, err := runCanonicalJSON(ctx, real, "list", "--all", "--format", "json")
	if err != nil {
		return inventoryResult{err: err}
	}
	networks, err := runCanonicalJSON(ctx, real, "network", "list", "--format", "json")
	if err != nil {
		return inventoryResult{err: err}
	}
	volumes, err := runCanonicalJSON(ctx, real, "volume", "list", "--format", "json")
	if err != nil {
		return inventoryResult{err: err}
	}
	builder, err := runCanonicalJSON(ctx, real, "builder", "status", "--format", "json")
	if err != nil {
		return inventoryResult{err: err}
	}
	return inventoryResult{inventory: coreInventory{Containers: containers, Networks: networks, Volumes: volumes, Builder: builder}}
}

func runCanonicalJSON(ctx context.Context, real *coreRuntime, args ...string) (string, error) {
	result, err := real.runner.Run(ctx, runtimeapple.Command{Executable: real.executable, Args: append([]string(nil), args...), Env: []string{"LANG=C", "LC_ALL=C"}})
	if err != nil {
		return "", fmt.Errorf("container %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(result.Stderr)))
	}
	var value any
	if err := json.Unmarshal(result.Stdout, &value); err != nil {
		return "", fmt.Errorf("decode container %s JSON: %w", strings.Join(args, " "), err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

func assertInventoryUnchanged(t *testing.T, before, after coreInventory) {
	t.Helper()
	if before.Containers != after.Containers {
		t.Errorf("preexisting container inventory changed\nbefore: %s\nafter:  %s", before.Containers, after.Containers)
	}
	if before.Networks != after.Networks {
		t.Errorf("preexisting network inventory changed\nbefore: %s\nafter:  %s", before.Networks, after.Networks)
	}
	if before.Volumes != after.Volumes {
		t.Errorf("preexisting volume inventory changed\nbefore: %s\nafter:  %s", before.Volumes, after.Volumes)
	}
	if before.Builder != after.Builder {
		t.Errorf("Apple builder identity/status changed\nbefore: %s\nafter:  %s", before.Builder, after.Builder)
	}
	assertBuilderPresent(t, after.Builder)
}

func assertBuildInventoryUnchanged(t *testing.T, before, after coreInventory) {
	t.Helper()
	beforeContainers, err := canonicalContainersWithoutBuilder(before.Containers)
	if err != nil {
		t.Errorf("decode pre-build container inventory: %v", err)
		return
	}
	afterContainers, err := canonicalContainersWithoutBuilder(after.Containers)
	if err != nil {
		t.Errorf("decode post-build container inventory: %v", err)
		return
	}
	if beforeContainers != afterContainers {
		t.Errorf("unrelated container inventory changed during image build\nbefore: %s\nafter:  %s", beforeContainers, afterContainers)
	}
	if before.Networks != after.Networks {
		t.Errorf("preexisting network inventory changed during image build\nbefore: %s\nafter:  %s", before.Networks, after.Networks)
	}
	if before.Volumes != after.Volumes {
		t.Errorf("preexisting volume inventory changed during image build\nbefore: %s\nafter:  %s", before.Volumes, after.Volumes)
	}
	beforeBuilder, err := stableBuilderIdentity(before.Builder)
	if err != nil {
		t.Errorf("decode pre-build builder identity: %v", err)
		return
	}
	afterBuilder, err := stableBuilderIdentity(after.Builder)
	if err != nil {
		t.Errorf("decode post-build builder identity: %v", err)
		return
	}
	if beforeBuilder != afterBuilder {
		t.Errorf("Apple builder ID/image/labels changed during its build lifecycle\nbefore: %s\nafter:  %s", beforeBuilder, afterBuilder)
	}
	assertBuilderPresent(t, after.Builder)
}

func canonicalContainersWithoutBuilder(canonical string) (string, error) {
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(canonical), &rows); err != nil {
		return "", err
	}
	filtered := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		var identity struct {
			Configuration struct {
				ID string `json:"id"`
			} `json:"configuration"`
		}
		if err := json.Unmarshal(row, &identity); err != nil {
			return "", err
		}
		if identity.Configuration.ID != "buildkit" {
			filtered = append(filtered, row)
		}
	}
	encoded, err := json.Marshal(filtered)
	return string(encoded), err
}

func stableBuilderIdentity(canonical string) (string, error) {
	type builderIdentity struct {
		ID     string            `json:"id"`
		Image  string            `json:"image"`
		Digest string            `json:"digest"`
		Labels map[string]string `json:"labels"`
	}
	var rows []struct {
		Configuration struct {
			ID    string `json:"id"`
			Image struct {
				Reference  string `json:"reference"`
				Descriptor struct {
					Digest string `json:"digest"`
				} `json:"descriptor"`
			} `json:"image"`
			Labels map[string]string `json:"labels"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal([]byte(canonical), &rows); err != nil {
		return "", err
	}
	identities := make([]builderIdentity, 0, 1)
	for _, row := range rows {
		if row.Configuration.ID == "buildkit" {
			identities = append(identities, builderIdentity{
				ID: row.Configuration.ID, Image: row.Configuration.Image.Reference,
				Digest: row.Configuration.Image.Descriptor.Digest, Labels: row.Configuration.Labels,
			})
		}
	}
	encoded, err := json.Marshal(identities)
	return string(encoded), err
}

func assertBuilderPresent(t *testing.T, canonical string) {
	t.Helper()
	var rows []struct {
		Configuration struct {
			ID string `json:"id"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal([]byte(canonical), &rows); err != nil {
		t.Errorf("decode canonical builder inventory: %v", err)
		return
	}
	for _, row := range rows {
		if row.Configuration.ID == "buildkit" {
			return
		}
	}
	t.Errorf("Apple buildkit builder is absent: %s", canonical)
}
