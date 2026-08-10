package apple_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	runtimeapple "github.com/srimajji/dsx/internal/runtime/apple"
	"github.com/srimajji/dsx/internal/state"
	statefs "github.com/srimajji/dsx/internal/state/fs"
)

type faultBoundary string

const (
	faultCreateNetwork   faultBoundary = "create-network"
	faultCreateVolume    faultBoundary = "create-volume"
	faultCreateWorkspace faultBoundary = "create-workspace"
	faultInspect         faultBoundary = "inspect"
	faultStart           faultBoundary = "start"
	faultReadiness       faultBoundary = "readiness"
	faultCapture         faultBoundary = "capture"
)

var faultCleanupBoundaries = []faultBoundary{
	faultCreateNetwork, faultCreateVolume, faultCreateWorkspace,
	faultInspect, faultStart, faultReadiness, faultCapture,
}

type boundaryFaultAdapter struct {
	runtime.Adapter
	boundary  faultBoundary
	err       error
	mu        sync.Mutex
	fired     bool
	execs     int
	created   []appleResourceEvidence
	recordErr error
}

func (adapter *boundaryFaultAdapter) fire(boundary faultBoundary) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.boundary == boundary && !adapter.fired {
		adapter.fired = true
		return adapter.err
	}
	return nil
}

func (adapter *boundaryFaultAdapter) CreateNetwork(ctx context.Context, spec runtime.NetworkSpec) (runtime.Resource, error) {
	if err := adapter.fire(faultCreateNetwork); err != nil {
		return runtime.Resource{}, err
	}
	created, err := adapter.Adapter.CreateNetwork(ctx, spec)
	if err == nil {
		adapter.recordCreated(created, spec.Labels)
	}
	return created, err
}

func (adapter *boundaryFaultAdapter) CreateVolume(ctx context.Context, spec runtime.VolumeSpec) (runtime.Resource, error) {
	if err := adapter.fire(faultCreateVolume); err != nil {
		return runtime.Resource{}, err
	}
	created, err := adapter.Adapter.CreateVolume(ctx, spec)
	if err == nil {
		adapter.recordCreated(created, spec.Labels)
	}
	return created, err
}

func (adapter *boundaryFaultAdapter) CreateWorkspace(ctx context.Context, spec runtime.WorkspaceSpec) (runtime.Resource, error) {
	if err := adapter.fire(faultCreateWorkspace); err != nil {
		return runtime.Resource{}, err
	}
	created, err := adapter.Adapter.CreateWorkspace(ctx, spec)
	if err == nil {
		adapter.recordCreated(created, spec.Labels)
	}
	return created, err
}

func (adapter *boundaryFaultAdapter) Inspect(ctx context.Context, id runtime.ResourceID) (runtime.ResourceSnapshot, error) {
	if err := adapter.fire(faultInspect); err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	return adapter.Adapter.Inspect(ctx, id)
}

func (adapter *boundaryFaultAdapter) StartWorkspace(ctx context.Context, snapshot runtime.ResourceSnapshot) error {
	if err := adapter.fire(faultStart); err != nil {
		return err
	}
	return adapter.Adapter.StartWorkspace(ctx, snapshot)
}

func (adapter *boundaryFaultAdapter) Exec(ctx context.Context, snapshot runtime.ResourceSnapshot, spec runtime.ExecSpec, streams runtime.ExecIO) (runtime.Exit, error) {
	var request struct {
		Operation string `json:"operation"`
	}
	if streams.Stdin != nil {
		encoded, err := io.ReadAll(io.LimitReader(streams.Stdin, 1<<20))
		if err != nil {
			return runtime.Exit{}, err
		}
		streams.Stdin = bytes.NewReader(encoded)
		_ = json.Unmarshal(encoded, &request)
	}
	adapter.mu.Lock()
	adapter.execs++
	readiness := adapter.boundary == faultReadiness && request.Operation == "status"
	if readiness {
		adapter.fired = true
	}
	adapter.mu.Unlock()
	if readiness {
		return runtime.Exit{}, adapter.err
	}
	return adapter.Adapter.Exec(ctx, snapshot, spec, streams)
}

func (adapter *boundaryFaultAdapter) CopyFrom(ctx context.Context, snapshot runtime.ResourceSnapshot, source runtime.GuestPath, destination runtime.HostPath) error {
	if err := adapter.fire(faultCapture); err != nil {
		return err
	}
	return adapter.Adapter.CopyFrom(ctx, snapshot, source, destination)
}
func (adapter *boundaryFaultAdapter) recordCreated(created runtime.Resource, labels []runtime.Label) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	values := make(map[string]string, len(labels))
	for _, label := range labels {
		if _, duplicate := values[label.Key]; duplicate {
			adapter.recordErr = fmt.Errorf("created resource %q has duplicate label %q", created.ID, label.Key)
			return
		}
		values[label.Key] = label.Value
	}
	projectID, projectErr := model.ParseProjectID(values[state.OwnershipProjectLabel])
	sandbox, sandboxErr := model.ParseSandboxName(values[state.OwnershipSandboxLabel])
	runID, runErr := model.ParseRunID(values[state.OwnershipRunLabel])
	role := values[state.OwnershipRoleLabel]
	if len(values) != 7 || projectErr != nil || sandboxErr != nil || runErr != nil || values[state.OwnershipManagedLabel] != "true" || values[state.OwnershipContractLabel] != state.OwnershipContractValue || values[state.OwnershipKindLabel] != string(created.Kind) || role == "" || created.ID != runtime.ResourceID(created.Name) || created.Name != state.CanonicalResourceName(projectID, sandbox, role) {
		adapter.recordErr = fmt.Errorf("created resource %q lacks exact complete run ownership", created.ID)
		return
	}
	adapter.created = append(adapter.created, appleResourceEvidence{
		ID: string(created.ID), Kind: string(created.Kind), Project: string(projectID),
		Sandbox: string(sandbox), Run: string(runID), Role: role,
	})
}

func (adapter *boundaryFaultAdapter) CreatedEvidence(t *testing.T) []appleResourceEvidence {
	t.Helper()
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.recordErr != nil {
		t.Fatalf("record created run ownership: %v", adapter.recordErr)
	}
	return append([]appleResourceEvidence(nil), adapter.created...)
}

func TestFaultCleanup(t *testing.T) {
	real, evidenceDirectory, ciRunID, ciRunLabel := requireFaultCleanupRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	baseline := snapshotInventory(t, ctx, real)
	cancel()
	baselineEvidence, err := inventoryEvidence(baseline)
	if err != nil {
		t.Fatalf("baseline evidence: %v", err)
	}

	bundle := appleFaultEvidenceBundle{
		Schema: "dsx.apple.fault-cleanup/v1", Suite: "DSX-080", CIRunID: ciRunID, CIRunLabel: ciRunLabel,
		Runtime: exactAppleRuntimeEvidence(), Baseline: baselineEvidence, Verdict: "running",
	}
	if err := writeFaultEvidence(evidenceDirectory, bundle); err != nil {
		t.Fatalf("write pre-mutation DSX-080 baseline evidence: %v", err)
	}
	t.Cleanup(func() {
		finalCtx, finalCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer finalCancel()
		result := snapshotInventoryNoFail(finalCtx, real)
		if result.err != nil {
			t.Errorf("final Apple inventory: %v", result.err)
		} else {
			finalEvidence, evidenceErr := inventoryEvidence(result.inventory)
			if evidenceErr != nil {
				t.Errorf("final inventory evidence: %v", evidenceErr)
			} else {
				bundle.Final = finalEvidence
				bundle.BuilderSame = baselineEvidence.Builder == finalEvidence.Builder
				bundle.DefaultSame = baselineEvidence.DefaultNetwork == finalEvidence.DefaultNetwork
				if !reflect.DeepEqual(baseline, result.inventory) {
					t.Errorf("unrelated baseline changed\nbefore=%#v\nafter=%#v", baseline, result.inventory)
				}
			}
		}
		if evidenceErr := validateScenarioEvidence(bundle.Scenarios); evidenceErr != nil {
			t.Errorf("validate exact run-labeled evidence: %v", evidenceErr)
		}
		bundle.Verdict = "failed"
		if !t.Failed() && bundle.BuilderSame && bundle.DefaultSame && bundle.Baseline.SHA256 == bundle.Final.SHA256 {
			bundle.Verdict = "passed"
		}
		if writeErr := writeFaultEvidence(evidenceDirectory, bundle); writeErr != nil {
			t.Errorf("write DSX-080 evidence: %v", writeErr)
		}
	})

	for _, boundary := range []faultBoundary{faultCreateNetwork, faultCreateVolume, faultCreateWorkspace, faultInspect, faultStart} {
		boundary := boundary
		runFaultScenario(t, &bundle, string(boundary), func(t *testing.T, scenario *appleScenarioEvidence) {
			injected := fmt.Errorf("DSX-080 injected %s failure", boundary)
			fault := &boundaryFaultAdapter{Adapter: real.adapter, boundary: boundary, err: injected}
			fixture := newCoreFixture(t, real, fault, true, 0)
			defer fixture.recover()
			_, startErr := fixture.service.Start(context.Background(), app.StartRequest{Root: fixture.root, ApproveConfig: fixture.hash})
			if !errors.Is(startErr, injected) || !fault.fired {
				t.Fatalf("Start() = %v, fault fired=%t", startErr, fault.fired)
			}
			scenario.Resources = mergeScenarioResources(t, fault.CreatedEvidence(t), observeFixtureResources(t, fixture))
			assertCleanTwice(t, fixture, scenario.Resources)
		})
	}

	runFaultScenario(t, &bundle, string(faultReadiness), func(t *testing.T, scenario *appleScenarioEvidence) {
		injected := errors.New("DSX-080 injected guest readiness read failure")
		fault := &boundaryFaultAdapter{Adapter: real.adapter, boundary: faultReadiness, err: injected}
		fixture := newCoreFixture(t, real, fault, false, 0)
		defer fixture.recover()
		writeCoreGuestConfig(t, fixture.root)
		refreshFixtureHash(t, fixture)
		_, startErr := fixture.service.Start(context.Background(), app.StartRequest{Root: fixture.root, ApproveConfig: fixture.hash})
		fault.mu.Lock()
		fired, execs := fault.fired, fault.execs
		fault.boundary = ""
		fault.mu.Unlock()
		if startErr == nil || !fired {
			t.Fatalf("readiness Start() = %v, fault fired=%t execs=%d", startErr, fired, execs)
		}
		scenario.Resources = mergeScenarioResources(t, fault.CreatedEvidence(t), observeFixtureResources(t, fixture))
		assertCleanTwice(t, fixture, scenario.Resources)
	})

	runFaultScenario(t, &bundle, string(faultCapture), func(t *testing.T, scenario *appleScenarioEvidence) {
		injected := errors.New("DSX-080 injected result capture failure")
		fault := &boundaryFaultAdapter{Adapter: real.adapter, boundary: faultCapture, err: injected}
		fixture := newCoreFixture(t, real, fault, false, 0)
		defer fixture.recover()
		fixture.start(context.Background())
		manifest := fixture.oneManifest()
		workspace := manifestRecord(t, manifest, string(runtime.ResourceWorkspace), "workspace")
		snapshot, inspectErr := real.adapter.Inspect(context.Background(), runtime.ResourceID(workspace.RuntimeID))
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		destination := filepath.Join(t.TempDir(), "capture.bundle")
		captureErr := fault.CopyFrom(context.Background(), snapshot, "/tmp/dsx-result.bundle", runtime.HostPath(destination))
		if !errors.Is(captureErr, injected) || !fault.fired {
			t.Fatalf("CopyFrom() = %v, fault fired=%t", captureErr, fault.fired)
		}
		if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed capture destination exists: %v", statErr)
		}
		scenario.Resources = mergeScenarioResources(t, fault.CreatedEvidence(t), observeFixtureResources(t, fixture))
		assertCleanTwice(t, fixture, scenario.Resources)
	})

	runFaultScenario(t, &bundle, "ctrl-c-after-create", func(t *testing.T, scenario *appleScenarioEvidence) {
		cancelCtx, cancelStart := context.WithCancel(context.Background())
		recorder := &boundaryFaultAdapter{Adapter: real.adapter}
		fault := &cancelAfterCreateAdapter{Adapter: recorder, cancel: cancelStart}
		fixture := newCoreFixture(t, real, fault, true, 0)
		defer fixture.recover()
		_, startErr := fixture.service.Start(cancelCtx, app.StartRequest{Root: fixture.root, ApproveConfig: fixture.hash})
		if startErr == nil || fault.created.ID == "" {
			t.Fatalf("cancelled Start() = %v created=%#v", startErr, fault.created)
		}
		scenario.Resources = mergeScenarioResources(t, recorder.CreatedEvidence(t), observeFixtureResources(t, fixture))
		assertCleanTwice(t, fixture, scenario.Resources)
	})

	runFaultScenario(t, &bundle, "child-sigkill-recovery", func(t *testing.T, scenario *appleScenarioEvidence) {
		fixture := newCoreFixture(t, real, real.adapter, false, 0)
		defer fixture.recover()
		writeCoreGuestConfig(t, fixture.root)
		refreshFixtureHash(t, fixture)
		fixture.start(context.Background())
		manifest := fixture.oneManifest()
		workspaceRecord := manifestRecord(t, manifest, string(runtime.ResourceWorkspace), "workspace")
		workspace, inspectErr := real.adapter.Inspect(context.Background(), runtime.ResourceID(workspaceRecord.RuntimeID))
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		if signalErr := real.adapter.Signal(context.Background(), workspace, runtime.Signal("KILL")); signalErr != nil {
			t.Fatalf("safe child PID1 SIGKILL: %v", signalErr)
		}
		fixture.start(context.Background())
		scenario.Resources = observeFixtureResources(t, fixture)
		assertCleanTwice(t, fixture, scenario.Resources)
	})

	for _, manifestCase := range []string{"stale", "corrupt", "ambiguous"} {
		manifestCase := manifestCase
		runFaultScenario(t, &bundle, manifestCase+"-manifest", func(t *testing.T, scenario *appleScenarioEvidence) {
			fixture := newCoreFixture(t, real, real.adapter, true, 0)
			defer fixture.recover()
			fixture.start(context.Background())
			manifest := fixture.oneManifest()
			scenario.Resources = observeFixtureResources(t, fixture)
			path := fixtureManifestPath(fixture, manifest)
			original, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			restored := false
			defer func() {
				if !restored {
					_ = os.WriteFile(path, original, 0o600)
				}
			}()

			var replacement []byte
			switch manifestCase {
			case "corrupt":
				replacement = []byte(`{"version":1,"truncated":`)
			case "stale":
				stale := manifest
				for index := range stale.Resources {
					stale.Resources[index].Created = false
					stale.Resources[index].RuntimeID = ""
				}
				replacement, readErr = json.Marshal(stale)
			case "ambiguous":
				ambiguous := manifest
				ambiguous.Resources[0].Labels = append([]state.OwnershipLabel(nil), ambiguous.Resources[0].Labels...)
				for index := range ambiguous.Resources[0].Labels {
					if ambiguous.Resources[0].Labels[index].Key == state.OwnershipRoleLabel {
						ambiguous.Resources[0].Labels[index].Value = "other"
					}
				}
				replacement, readErr = json.Marshal(ambiguous)
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			if writeErr := os.WriteFile(path, replacement, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			cleaned, cleanErr := fixture.service.Clean(context.Background(), app.CleanRequest{Root: fixture.root, Confirmed: true})
			if manifestCase == "stale" {
				if cleanErr != nil {
					t.Fatalf("stale manifest recovery failed: %#v: %v", cleaned, cleanErr)
				}
				restored = true
				assertEvidenceResourcesAbsent(t, real.adapter, scenario.Resources)
				second := fixture.clean()
				if second.DeletedResources != 0 || second.DeletedManifests != 0 || len(second.Preserved) != 0 {
					t.Fatalf("second stale cleanup = %#v", second)
				}
				return
			}
			if cleanErr == nil {
				t.Fatalf("unsafe %s manifest cleanup succeeded: %#v", manifestCase, cleaned)
			}
			assertEvidenceResourcesPresent(t, real.adapter, scenario.Resources)
			if writeErr := os.WriteFile(path, original, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			restored = true
			assertCleanTwice(t, fixture, scenario.Resources)
		})
	}

	runFaultScenario(t, &bundle, "runtime-restart-discovery", func(t *testing.T, scenario *appleScenarioEvidence) {
		fixture := newCoreFixture(t, real, real.adapter, true, 0)
		defer fixture.recover()
		started := fixture.start(context.Background())
		scenario.Resources = observeFixtureResources(t, fixture)
		restartedAdapter, adapterErr := runtimeapple.NewAdapter(real.runner, real.executable)
		if adapterErr != nil {
			t.Fatalf("recreate runtime client after restart boundary: %v", adapterErr)
		}
		rebuildFixtureService(t, fixture, restartedAdapter)
		resumed := fixture.start(context.Background())
		if !resumed.Existing || resumed.RunID != started.RunID {
			t.Fatalf("restart discovery = %#v, initial=%#v", resumed, started)
		}
		assertCleanTwice(t, fixture, scenario.Resources)
	})

	runFaultScenario(t, &bundle, "project-sandbox-isolation", func(t *testing.T, scenario *appleScenarioEvidence) {
		first := newCoreFixture(t, real, real.adapter, true, 0)
		defer cleanFixtureNoAssertion(first)
		first.start(context.Background())
		createSandboxNetworkFixture(t, first, "other")
		firstEvidence := observeFixtureResources(t, first)
		sandboxes := make(map[string]struct{})
		for _, resource := range firstEvidence {
			sandboxes[resource.Sandbox] = struct{}{}
		}
		if _, found := sandboxes["main"]; !found {
			t.Fatal("main sandbox evidence is absent")
		}
		if _, found := sandboxes["other"]; !found {
			t.Fatal("isolated sandbox evidence is absent")
		}
		second := newCoreFixture(t, real, real.adapter, true, 0)
		defer cleanFixtureNoAssertion(second)
		second.start(context.Background())
		secondEvidence := observeFixtureResources(t, second)
		if first.projectID == second.projectID {
			t.Fatal("independent roots derived the same project ID")
		}
		first.clean()
		assertEvidenceResourcesAbsent(t, real.adapter, firstEvidence)
		assertEvidenceResourcesPresent(t, real.adapter, secondEvidence)
		second.clean()
		assertEvidenceResourcesAbsent(t, real.adapter, secondEvidence)
		scenario.Resources = append(firstEvidence, secondEvidence...)
	})
}

func TestFaultCleanupBoundaryCatalog(t *testing.T) {
	want := []faultBoundary{faultCapture, faultCreateNetwork, faultCreateVolume, faultCreateWorkspace, faultInspect, faultReadiness, faultStart}
	got := append([]faultBoundary(nil), faultCleanupBoundaries...)
	sortFaultBoundaries(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fault boundary catalog = %q, want %q", got, want)
	}
}

func requireFaultCleanupRuntime(t *testing.T) (*coreRuntime, string, string, string) {
	t.Helper()
	if os.Getenv(appleOptIn) != "1" {
		t.Skipf("DSX-080 physical suite disabled; set %s=1 on a dedicated Apple-silicon runner", appleOptIn)
	}
	if os.Getenv(faultCleanupOptIn) != "1" {
		t.Skipf("DSX-080 destructive fault suite disabled; set %s=1 in addition to %s=1", faultCleanupOptIn, appleOptIn)
	}
	directory := os.Getenv(faultCleanupEvidenceDir)
	ciRunID := strings.TrimSpace(os.Getenv(faultCleanupRunID))
	ciRunLabel := strings.TrimSpace(os.Getenv(faultCleanupRunLabel))
	if directory == "" || ciRunID == "" || ciRunLabel == "" {
		t.Skipf("DSX-080 runner prerequisites absent; require %s, %s, and %s", faultCleanupEvidenceDir, faultCleanupRunID, faultCleanupRunLabel)
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		t.Fatalf("%s must be a clean absolute path before mutation", faultCleanupEvidenceDir)
	}
	info, statErr := os.Lstat(directory)
	if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s must be an existing non-symlink directory before mutation: %v", faultCleanupEvidenceDir, statErr)
	}
	real := requireCoreRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	capabilities, err := real.adapter.Probe(ctx)
	if err != nil {
		t.Fatalf("attest Apple runtime: %v", err)
	}
	if capabilities.HostOS != "Darwin" || capabilities.HostArch != "arm64" || capabilities.CLIVersion != "1.2.2" || capabilities.ServerVersion != "1.2.2" || capabilities.CompatibilityID != "apple-container/cli-1.2.2/server-1.2.2" || !capabilities.ServiceHealthy || !capabilities.BuilderHealthy || !capabilities.MachineReadableInspection || !capabilities.Labels || !capabilities.Networks || !capabilities.Volumes || !capabilities.Copy {
		t.Fatalf("DSX-080 requires exact healthy Apple container 1.2.2 attestation: %#v", capabilities)
	}
	return real, directory, ciRunID, ciRunLabel
}

func runFaultScenario(t *testing.T, bundle *appleFaultEvidenceBundle, name string, run func(*testing.T, *appleScenarioEvidence)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		scenario := appleScenarioEvidence{Name: name, Outcome: "failed"}
		defer func() {
			if !t.Failed() {
				scenario.Outcome = "passed"
			}
			bundle.Scenarios = append(bundle.Scenarios, scenario)
		}()
		run(t, &scenario)
	})
}

func observeFixtureResources(t *testing.T, fixture *coreFixture) []appleResourceEvidence {
	t.Helper()
	manifests, err := fixture.manifests.ListProjectManifests(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("list fixture manifests for ledger: %v", err)
	}
	ledger := newAppleResourceLedger()
	for _, manifest := range manifests {
		if err := ledger.ObserveManifest(context.Background(), fixture.runtime.adapter, manifest); err != nil {
			t.Fatalf("admit exact fixture resources: %v", err)
		}
	}
	return ledger.Evidence()
}

func mergeScenarioResources(t *testing.T, groups ...[]appleResourceEvidence) []appleResourceEvidence {
	t.Helper()
	byID := make(map[string]appleResourceEvidence)
	for _, group := range groups {
		for _, resource := range group {
			if existing, duplicate := byID[resource.ID]; duplicate && !reflect.DeepEqual(existing, resource) {
				t.Fatalf("contradictory exact evidence for resource %q", resource.ID)
			}
			byID[resource.ID] = resource
		}
	}
	merged := make([]appleResourceEvidence, 0, len(byID))
	for _, resource := range byID {
		merged = append(merged, resource)
	}
	return merged
}

func assertCleanTwice(t *testing.T, fixture *coreFixture, resources ...[]appleResourceEvidence) {
	t.Helper()
	configPath := filepath.Join(fixture.root, ".dsx", "config.jsonc")
	persistentConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read intended persistent project configuration: %v", err)
	}
	fixture.clean()
	second := fixture.clean()
	if second.DeletedResources != 0 || second.DeletedManifests != 0 || len(second.Preserved) != 0 {
		t.Fatalf("second cleanup was not idempotent: %#v", second)
	}
	retainedConfig, err := os.ReadFile(configPath)
	if err != nil || !reflect.DeepEqual(persistentConfig, retainedConfig) {
		t.Fatalf("ordinary cleanup changed intended persistent project configuration: %v", err)
	}
	for _, group := range resources {
		assertEvidenceResourcesAbsent(t, fixture.runtime.adapter, group)
	}
	fixture.assertRecovered()
}

func assertEvidenceResourcesPresent(t *testing.T, adapter runtime.Adapter, resources []appleResourceEvidence) {
	t.Helper()
	for _, resource := range resources {
		if _, err := adapter.Inspect(context.Background(), runtime.ResourceID(resource.ID)); err != nil {
			t.Fatalf("run-owned exact resource %q was not retained: %v", resource.ID, err)
		}
	}
}

func assertEvidenceResourcesAbsent(t *testing.T, adapter runtime.Adapter, resources []appleResourceEvidence) {
	t.Helper()
	for _, resource := range resources {
		_, err := adapter.Inspect(context.Background(), runtime.ResourceID(resource.ID))
		if !errors.Is(err, runtime.ErrResourceNotFound) {
			t.Fatalf("run-owned exact resource %q remains or cannot be proven absent: %v", resource.ID, err)
		}
	}
}

func refreshFixtureHash(t *testing.T, fixture *coreFixture) {
	t.Helper()
	inspected, err := app.NewInspectionService(plan.NewResolver()).Inspect(context.Background(), app.InspectRequest{Root: fixture.root, SandboxName: "main", Mode: string(model.ModeLive)})
	if err != nil {
		t.Fatal(err)
	}
	fixture.hash = inspected.Plan.ExecutableHash
}

func fixtureManifestPath(fixture *coreFixture, manifest state.Manifest) string {
	return filepath.Join(fixture.stateRoot, "manifests", string(manifest.ProjectID), string(manifest.Sandbox), string(manifest.RunID)+".json")
}

func createSandboxNetworkFixture(t *testing.T, fixture *coreFixture, sandboxValue string) {
	t.Helper()
	sandbox, err := model.ParseSandboxName(sandboxValue)
	if err != nil || sandbox == "main" {
		t.Fatalf("parse isolated sandbox: %v", err)
	}
	runID, err := model.NewRunID(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	name := state.CanonicalResourceName(fixture.projectID, sandbox, "network")
	labels := state.ResourceOwnershipLabels(fixture.projectID, sandbox, runID, string(runtime.ResourceNetwork), "network")
	record := state.ResourceRecord{
		Kind: string(runtime.ResourceNetwork), Role: "network", Name: name, ExpectedID: name, Labels: labels,
	}
	now := time.Now().UTC()
	manifest := state.Manifest{
		Version: state.ManifestVersion, Generation: 1, ProjectID: fixture.projectID, CanonicalRoot: fixture.root,
		Sandbox: sandbox, RunID: runID, Mode: model.ModeLive, PlanHash: fixture.hash,
		State: model.StatePlanned, Operation: "create", Resources: []state.ResourceRecord{record},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatalf("write isolated sandbox intent: %v", err)
	}
	runtimeOwnership := make([]runtime.Label, len(labels))
	for index, label := range labels {
		runtimeOwnership[index] = runtime.Label{Key: label.Key, Value: label.Value}
	}
	created, err := fixture.runtime.adapter.CreateNetwork(context.Background(), runtime.NetworkSpec{Name: name, Labels: runtimeOwnership})
	if err != nil {
		t.Fatalf("create exact isolated sandbox network: %v", err)
	}
	if created.ID != runtime.ResourceID(name) || created.Name != name || created.Kind != runtime.ResourceNetwork {
		t.Fatalf("isolated sandbox network identity = %#v", created)
	}
	manifest.State = model.StateCreating
	manifest.Resources[0].Created = true
	manifest.Resources[0].RuntimeID = string(created.ID)
	manifest.UpdatedAt = time.Now().UTC()
	if err := fixture.manifests.ReplaceManifest(context.Background(), manifest, 1); err != nil {
		t.Fatalf("record exact isolated sandbox network: %v", err)
	}
}

func rebuildFixtureService(t *testing.T, fixture *coreFixture, adapter runtime.Adapter) {
	t.Helper()
	manifests, err := statefs.NewManifestRepository(fixture.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := statefs.NewApprovalRepository(fixture.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	fixture.manifests = manifests
	fixture.adapter = adapter
	fixture.service = app.NewLifecycleService(app.LifecycleDependencies{
		Inspection: app.NewInspectionService(plan.NewResolver()), Approvals: approvals, Manifests: manifests, Locks: manifests,
		Runtime: adapter, Guest: app.NewGuestClient(adapter),
		GuestHelperSource: func() (runtime.HostPath, error) { return fixture.guestHelper, nil },
	})
}

func cleanFixtureNoAssertion(fixture *coreFixture) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := fixture.service.Clean(ctx, app.CleanRequest{Root: fixture.root, Confirmed: true}); err != nil {
		fixture.t.Errorf("deferred production cleanup: %v", err)
	}
}

func sortFaultBoundaries(boundaries []faultBoundary) {
	for index := 1; index < len(boundaries); index++ {
		for cursor := index; cursor > 0 && boundaries[cursor] < boundaries[cursor-1]; cursor-- {
			boundaries[cursor], boundaries[cursor-1] = boundaries[cursor-1], boundaries[cursor]
		}
	}
}

var _ runtime.Adapter = (*boundaryFaultAdapter)(nil)
