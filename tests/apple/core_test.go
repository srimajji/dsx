package apple_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/gitx"
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

type workspaceFixture struct {
	t         *testing.T
	real      *coreRuntime
	adapter   runtime.Adapter
	service   *app.WorkspaceService
	manifests *statefs.ManifestRepository
	root      string
	projectID model.ProjectID
	planHash  string
	workspace model.WorkspaceName
	before    coreInventory
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
		t.Fatalf("required workspace capabilities are absent: %#v", capabilities)
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

func TestCoreNamedWorkspacePrivateCloneLifecycle(t *testing.T) {
	real := requireCoreRuntime(t)
	fixture := newWorkspaceFixture(t, real, real.adapter, "core")
	defer fixture.recover()

	created := fixture.create(context.Background())
	if created.State != model.StateRunning || created.Workspace != fixture.workspace || created.Existing {
		t.Fatalf("Create() = %#v", created)
	}
	manifest := fixture.oneManifest()
	if err := state.ValidateManifest(manifest); err != nil {
		t.Fatalf("running manifest is invalid: %v", err)
	}
	if manifest.Version != state.ManifestVersion || manifest.Workspace != fixture.workspace || manifest.PlanHash != fixture.planHash || manifest.RunID != created.RunID {
		t.Fatalf("manifest identity = %#v", manifest)
	}
	if len(manifest.Resources) != 7 {
		t.Fatalf("resource count = %d, want network, five private volumes, workspace", len(manifest.Resources))
	}
	var workspace runtime.ResourceSnapshot
	for _, record := range manifest.Resources {
		if len(record.Labels) != 7 {
			t.Fatalf("resource %q labels = %#v", record.Name, record.Labels)
		}
		want := state.ResourceOwnershipLabels(manifest.ProjectID, manifest.Workspace, manifest.RunID, record.Kind, record.Role)
		if !reflect.DeepEqual(record.Labels, want) {
			t.Fatalf("resource %q labels = %#v, want %#v", record.Name, record.Labels, want)
		}
		observed, err := real.adapter.Inspect(context.Background(), runtime.ResourceID(record.RuntimeID))
		if err != nil {
			t.Fatalf("inspect %q: %v", record.Name, err)
		}
		classification := ownership.Classify(&record, &observed)
		if classification.Outcome != ownership.OutcomeOwned || !classification.DeleteAllowed {
			t.Fatalf("resource %q classification = %#v", record.Name, classification)
		}
		if record.Kind == string(runtime.ResourceWorkspace) {
			workspace = observed
		}
	}
	assertPrivateWorkspaceTopology(t, fixture.root, manifest, workspace)

	beforeIDs := manifestResourceIDs(manifest)
	if _, err := fixture.service.Restart(context.Background(), app.WorkspaceRestartRequest{Root: fixture.root, Workspace: fixture.workspace}); err != nil {
		t.Fatalf("Restart() failed: %v", err)
	}
	afterRestart := fixture.oneManifest()
	if got := manifestResourceIDs(afterRestart); !reflect.DeepEqual(got, beforeIDs) {
		t.Fatalf("restart replaced persistent resources: before=%#v after=%#v", beforeIDs, got)
	}
	if _, err := fixture.service.Stop(context.Background(), app.WorkspaceStopRequest{Root: fixture.root, Workspace: fixture.workspace}); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}
	if _, err := fixture.service.Start(context.Background(), app.WorkspaceStartRequest{Root: fixture.root, Workspace: fixture.workspace}); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	removed := fixture.remove()
	if removed.State != model.StateDeleted || removed.DeletedResources != 7 || !removed.DeletedManifest || len(removed.Preserved) != 0 {
		t.Fatalf("Remove() = %#v", removed)
	}
	fixture.assertRecovered()
}

func TestCoreWorkspaceCreateRollbackCancellationAndBuilderPreservation(t *testing.T) {
	real := requireCoreRuntime(t)

	t.Run("start_failure", func(t *testing.T) {
		injected := errors.New("injected workspace start failure")
		fault := &failStartAdapter{Adapter: real.adapter, err: injected}
		fixture := newWorkspaceFixture(t, real, fault, "start-failure")
		defer fixture.recover()
		_, err := fixture.service.Create(context.Background(), app.WorkspaceCreateRequest{Root: fixture.root, Workspace: fixture.workspace, ApproveConfig: fixture.planHash})
		if !errors.Is(err, injected) || fault.calls != 1 {
			t.Fatalf("Create() = %v, start calls = %d", err, fault.calls)
		}
		fixture.assertRecovered()
	})

	t.Run("cancellation_after_create", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fault := &cancelAfterCreateAdapter{Adapter: real.adapter, cancel: cancel}
		fixture := newWorkspaceFixture(t, real, fault, "cancelled")
		defer fixture.recover()
		_, err := fixture.service.Create(ctx, app.WorkspaceCreateRequest{Root: fixture.root, Workspace: fixture.workspace, ApproveConfig: fixture.planHash})
		if err == nil || fault.created.ID == "" {
			t.Fatalf("Create() = %v, created = %#v", err, fault.created)
		}
		fixture.assertRecovered()
	})
}

func TestCoreNativeDynamicPublicationUnsupported(t *testing.T) {
	requireCoreRuntime(t)
	t.Skip("Apple container 1.2.2 native dynamic publication is an explicit unsupported capability")
}

func newWorkspaceFixture(t *testing.T, real *coreRuntime, adapter runtime.Adapter, name model.WorkspaceName) *workspaceFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	before := snapshotInventory(t, ctx, real)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".dsx"), 0o700); err != nil {
		t.Fatal(err)
	}
	containerfile := "FROM " + pinnedImage + "\nRUN apk add --no-cache git zsh\n"
	if err := os.WriteFile(filepath.Join(root, "Containerfile"), []byte(containerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration := `{"schemaVersion":1,"workspace":{"root":"."},"image":{"build":{"context":".","file":"Containerfile"}},"agents":{"default":"codex","allowed":["codex"]}}`
	configuration = strings.ReplaceAll(configuration, `\"`, `"`)
	if err := os.WriteFile(filepath.Join(root, ".dsx", "config.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	initializeGitRepository(t, root)
	stateRoot := filepath.Join(t.TempDir(), "state")
	manifests, err := statefs.NewManifestRepository(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	inspection := app.NewInspectionService(plan.NewResolver())
	inspected, err := inspection.Inspect(context.Background(), app.InspectRequest{Root: root})
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
	gitExecutable, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("locate git: %v", err)
	}
	gitService, err := gitx.NewService(gitx.OSRunner{}, gitExecutable)
	if err != nil {
		t.Fatalf("construct Git source service: %v", err)
	}
	service := app.NewWorkspaceService(app.WorkspaceDependencies{
		Inspection: inspection, Manifests: manifests, Locks: manifests, Runtime: adapter, Git: gitService,
		TempRoot: t.TempDir(), GuestHelperSource: func() (runtime.HostPath, error) { return stagedHelper, nil },
	})
	return &workspaceFixture{t: t, real: real, adapter: adapter, service: service, manifests: manifests, root: root, projectID: inspected.Plan.Project.ID, planHash: inspected.Plan.ExecutableHash, workspace: name, before: before}
}

func initializeGitRepository(t *testing.T, root string) {
	t.Helper()
	commands := [][]string{
		{"init", "--initial-branch=main"},
		{"add", "--all"},
		{"-c", "user.name=DSX Apple Test", "-c", "user.email=dsx@example.invalid", "commit", "--quiet", "-m", "initial"},
	}
	for _, arguments := range commands {
		command := exec.Command("git", arguments...)
		command.Dir = root
		command.Env = append(os.Environ(), "LANG=C", "LC_ALL=C")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
		}
	}
}

func (fixture *workspaceFixture) create(ctx context.Context) app.WorkspaceResult {
	fixture.t.Helper()
	result, err := fixture.service.Create(ctx, app.WorkspaceCreateRequest{Root: fixture.root, Workspace: fixture.workspace, ApproveConfig: fixture.planHash})
	if err != nil {
		fixture.t.Fatalf("Create() failed: %v", err)
	}
	return result
}

func (fixture *workspaceFixture) oneManifest() state.Manifest {
	fixture.t.Helper()
	manifests, err := fixture.manifests.ListProjectManifests(context.Background(), fixture.projectID)
	if err != nil || len(manifests) != 1 {
		fixture.t.Fatalf("project manifests = %#v, %v", manifests, err)
	}
	return manifests[0]
}

func (fixture *workspaceFixture) remove() app.WorkspaceRemoveResult {
	fixture.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result, err := fixture.service.Remove(ctx, app.WorkspaceRemoveRequest{Root: fixture.root, Workspace: fixture.workspace, Confirmed: true, DiscardUnfetched: true})
	if err != nil {
		fixture.t.Fatalf("WorkspaceService.Remove recovery failed: %v", err)
	}
	return result
}

func (fixture *workspaceFixture) recover() {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := fixture.service.Remove(ctx, app.WorkspaceRemoveRequest{Root: fixture.root, Workspace: fixture.workspace, Confirmed: true, DiscardUnfetched: true, LegacyResources: true}); err != nil && model.ErrorCodeOf(err) != model.CodeUnavailable {
		fixture.t.Errorf("deferred WorkspaceService.Remove recovery failed: %v", err)
	}
	fixture.assertRecoveredWithContext(ctx)
}

func (fixture *workspaceFixture) assertRecovered() {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	fixture.assertRecoveredWithContext(ctx)
}

func (fixture *workspaceFixture) assertRecoveredWithContext(ctx context.Context) {
	fixture.t.Helper()
	manifests, err := fixture.manifests.ListProjectManifests(ctx, fixture.projectID)
	if err != nil || len(manifests) != 0 {
		fixture.t.Errorf("workspace manifests remain after recovery: %#v, %v", manifests, err)
	}
	after := snapshotInventory(fixture.t, ctx, fixture.real)
	assertInventoryUnchanged(fixture.t, fixture.before, after)
}

func assertPrivateWorkspaceTopology(t *testing.T, root string, manifest state.Manifest, workspace runtime.ResourceSnapshot) {
	t.Helper()
	if workspace.State != "running" || len(workspace.Networks) != 1 {
		t.Fatalf("workspace topology = %#v", workspace)
	}
	network := manifestRecord(t, manifest, string(runtime.ResourceNetwork), "network")
	if workspace.Networks[0] != network.Name {
		t.Fatalf("workspace network = %q, want %q", workspace.Networks[0], network.Name)
	}
	privateTargets := map[string]bool{"/workspace": false, "/home/dsx/.dsx/auth": false, "/home/dsx/.local/state/dsx": false, "/home/dsx/.cache": false, "/var/lib/dsx": false}
	for _, mount := range workspace.Mounts {
		if mount.Source == root || strings.HasPrefix(mount.Source, root+string(os.PathSeparator)) {
			t.Fatalf("host project source mounted into workspace: %#v", mount)
		}
		if _, expected := privateTargets[mount.Target]; expected {
			if mount.Type != "volume" || mount.ReadOnly {
				t.Fatalf("private target is not a writable volume: %#v", mount)
			}
			privateTargets[mount.Target] = true
		}
	}
	for target, found := range privateTargets {
		if !found {
			t.Fatalf("private volume target %q missing: %#v", target, workspace.Mounts)
		}
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

func manifestResourceIDs(manifest state.Manifest) []string {
	ids := make([]string, len(manifest.Resources))
	for index, record := range manifest.Resources {
		ids[index] = record.RuntimeID
	}
	sort.Strings(ids)
	return ids
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

func snapshotInventory(t *testing.T, ctx context.Context, real *coreRuntime) coreInventory {
	t.Helper()
	containers, err := runCanonicalJSON(ctx, real, "list", "--all", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	networks, err := runCanonicalJSON(ctx, real, "network", "list", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	volumes, err := runCanonicalJSON(ctx, real, "volume", "list", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	builder, err := runCanonicalJSON(ctx, real, "builder", "status", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	assertBuilderPresent(t, builder)
	return coreInventory{Containers: containers, Networks: networks, Volumes: volumes, Builder: builder}
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
	return string(canonical), err
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
