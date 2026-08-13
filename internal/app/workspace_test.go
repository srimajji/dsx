package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

func TestWorkspaceCreatePersistsIntentBeforeMutationAndNeverMountsHostSource(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	result, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != model.StateRunning {
		t.Fatalf("state = %s", result.State)
	}
	if !fixture.runtime.intentBeforeEveryCreate {
		t.Fatal("runtime mutation preceded durable resource intent")
	}
	if got, want := fixture.locks.events[:2], []string{"workspace:alpha", "project"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lock order = %#v, want %#v", got, want)
	}
	if got, want := fixture.locks.events[len(fixture.locks.events)-2:], []string{"unlock:project", "unlock:workspace:alpha"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unlock order = %#v, want %#v", got, want)
	}
	spec := fixture.runtime.workspaceSpecs["alpha"]
	if len(spec.Mounts) != 6 {
		t.Fatalf("mounts = %#v", spec.Mounts)
	}
	roles := map[string]bool{}
	for _, mount := range spec.Mounts {
		if string(mount.Source) == fixture.root || strings.HasPrefix(string(mount.Source), fixture.root+string(os.PathSeparator)) {
			t.Fatalf("host project path mounted: %#v", mount)
		}
		if runtime.GuestPath(mount.Target) == workspaceGuestRoot {
			if mount.Type != "volume" || mount.Authority != runtime.MountAuthorityVolume {
				t.Fatalf("workspace source is not a private volume: %#v", mount)
			}
		}
		if mount.Authority == runtime.MountAuthorityGuestHelper {
			if mount.Target != DefaultGuestHelperDirectory || filepath.Base(string(mount.Source)) == "dsx-guest" {
				t.Fatalf("guest helper must mount its private directory: %#v", mount)
			}
		}
		roles[string(mount.Target)] = true
	}
	for _, target := range []string{"/workspace", "/home/dsx/.dsx/auth", "/home/dsx/.local/state/dsx", "/home/dsx/.cache", "/var/lib/dsx"} {
		if !roles[target] {
			t.Fatalf("missing independent volume target %s", target)
		}
	}
	if got := spec.Entrypoint; len(got) < 2 || got[0] != DefaultGuestHelperPath || got[1] != "serve" {
		t.Fatalf("workspace entrypoint = %#v", got)
	}
	if spec.User != standardWorkspaceUser {
		t.Fatalf("workspace user = %q, want %q", spec.User, standardWorkspaceUser)
	}
	if slices.Contains(spec.Entrypoint, "--initialize-workspace") {
		t.Fatalf("workspace entrypoint retains root initialization: %#v", spec.Entrypoint)
	}
	if len(fixture.runtime.execs) == 0 || !reflect.DeepEqual(fixture.runtime.execs[0].Argv, workspaceInitializationArgv) ||
		fixture.runtime.execs[0].User != "0:0" || fixture.runtime.execs[0].WorkingDir != "/workspace" {
		t.Fatalf("first workspace exec is not exact volume initialization: %#v", fixture.runtime.execs)
	}
}

func TestWorkspaceCreateHostDefaultPlansPrivateDisabledChannelMount(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	manager := newRecordingHostAWSManager()
	configureHostDefaultAWS(t, fixture, manager)
	createAWSWorkspace(t, fixture, "aws-channel")

	manifest := fixture.manifest("aws-channel")
	if manifest.AWSGrant == nil || manifest.AWSGrant.Enabled {
		t.Fatalf("new host-default grant = %#v", manifest.AWSGrant)
	}
	if !slices.Equal(manager.events, []string{"prepare:aws-channel"}) {
		t.Fatalf("channel preparation events = %v", manager.events)
	}
	spec := fixture.runtime.workspaceSpecs["aws-channel"]
	if spec.HostAWSMirrorSource == "" {
		t.Fatal("workspace spec omitted stable AWS mirror source")
	}
	var channelMount *runtime.Mount
	for index := range spec.Mounts {
		if spec.Mounts[index].Target == plan.AWSGuestDestination {
			channelMount = &spec.Mounts[index]
			break
		}
	}
	if channelMount == nil || channelMount.Source != string(spec.HostAWSMirrorSource) || channelMount.Type != "bind" || !channelMount.ReadOnly || channelMount.Authority != runtime.MountAuthorityHostAWSMirror {
		t.Fatalf("host AWS channel mount = %#v, source = %q", channelMount, spec.HostAWSMirrorSource)
	}
}

func TestWorkspaceCreateRollbackRemovesExactAWSChannel(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	manager := newRecordingHostAWSManager()
	configureHostDefaultAWS(t, fixture, manager)
	fixture.runtime.failCreateKind = runtime.ResourceWorkspace
	manager.onRemove = func(identity bridge.LeaseIdentity) {
		manifest := fixture.manifest(identity.Workspace)
		if manifest.AWSGrant == nil || manifest.AWSGrant.Enabled {
			t.Fatalf("rollback channel removal manifest = %#v", manifest)
		}
	}
	if _, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: "aws-rollback"}); err == nil {
		t.Fatal("host-default creation failure was accepted")
	}
	if !slices.Equal(manager.events, []string{"prepare:aws-rollback", "remove:aws-rollback"}) {
		t.Fatalf("rollback channel events = %v", manager.events)
	}
	if manifests, _ := fixture.manifests.ListProjectManifests(context.Background(), fixture.projectID); len(manifests) != 0 {
		t.Fatalf("rollback retained manifest: %#v", manifests)
	}
}

func TestWorkspaceCreateRollbackAndSiblingLifecycleAreIndependent(t *testing.T) {
	failure := newWorkspaceFixture(t)
	failure.runtime.failCreateKind = runtime.ResourceWorkspace
	if _, err := failure.service.Create(context.Background(), WorkspaceCreateRequest{Root: failure.root, Workspace: "broken"}); err == nil {
		t.Fatal("creation failure was accepted")
	}
	if len(failure.runtime.resources) != 0 {
		t.Fatalf("rollback leaked resources: %#v", failure.runtime.resources)
	}
	if manifests, _ := failure.manifests.ListProjectManifests(context.Background(), failure.projectID); len(manifests) != 0 {
		t.Fatalf("rollback retained manifests: %#v", manifests)
	}

	fixture := newWorkspaceFixture(t)
	for _, name := range []model.WorkspaceName{"alpha", "beta", "gamma"} {
		if _, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: name}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	before := fixture.resourceNamesFor("beta")
	if _, err := fixture.service.Stop(context.Background(), WorkspaceStopRequest{Root: fixture.root, Workspace: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Remove(context.Background(), WorkspaceRemoveRequest{Root: fixture.root, Workspace: "alpha", Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	if got := fixture.resourceNamesFor("beta"); !reflect.DeepEqual(got, before) {
		t.Fatalf("sibling resources changed: before=%#v after=%#v", before, got)
	}
	listed, err := fixture.service.List(context.Background(), WorkspaceListRequest{Root: fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	if got := []model.WorkspaceName{listed.Workspaces[0].Workspace, listed.Workspaces[1].Workspace}; !reflect.DeepEqual(got, []model.WorkspaceName{"beta", "gamma"}) {
		t.Fatalf("siblings = %#v", got)
	}
}

func TestWorkspaceRestartPreservesVolumesAndStartsOnlyGuestControl(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	if _, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: "restart"}); err != nil {
		t.Fatal(err)
	}
	before := fixture.resourceNamesFor("restart")
	execCount := len(fixture.runtime.execs)
	starts := fixture.runtime.starts
	if _, err := fixture.service.Restart(context.Background(), WorkspaceRestartRequest{Root: fixture.root, Workspace: "restart"}); err != nil {
		t.Fatal(err)
	}
	if got := fixture.resourceNamesFor("restart"); !reflect.DeepEqual(got, before) {
		t.Fatalf("restart replaced resources: before=%#v after=%#v", before, got)
	}
	if fixture.runtime.starts != starts+1 || fixture.runtime.stops == 0 {
		t.Fatalf("restart calls start=%d stop=%d", fixture.runtime.starts, fixture.runtime.stops)
	}
	if len(fixture.runtime.execs) != execCount {
		t.Fatalf("restart relaunched guest processes: %#v", fixture.runtime.execs[execCount:])
	}
}

func TestWorkspaceRemovalProtectsUnfetchedAndLegacyCleanupFailsClosed(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	if _, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: "guarded"}); err != nil {
		t.Fatal(err)
	}
	manifest := fixture.manifest("guarded")
	manifest.Git[0].ResultCommit = strings.Repeat("4", 40)
	manifest.Git[0].ResultBundleDigest = strings.Repeat("5", 64)
	if err := fixture.manifests.ReplaceManifest(context.Background(), manifest, manifest.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Remove(context.Background(), WorkspaceRemoveRequest{Root: fixture.root, Workspace: "guarded", Confirmed: true}); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("unfetched removal error = %v", err)
	}
	if len(fixture.resourceNamesFor("guarded")) == 0 {
		t.Fatal("unfetched workspace was destroyed")
	}

	legacy := newWorkspaceFixture(t)
	name := model.WorkspaceName("legacy")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000099")
	labels := state.LegacyResourceOwnershipLabels(legacy.projectID, name, runID, string(runtime.ResourceWorkspace), workspaceOwnerRole)
	record := state.ResourceRecord{Kind: string(runtime.ResourceWorkspace), Role: workspaceOwnerRole, Name: "dsx-legacy", ExpectedID: "dsx-legacy", RuntimeID: "dsx-legacy", Created: true, Labels: labels}
	legacyManifest := state.Manifest{Version: state.LegacyManifestVersion, Generation: 1, ProjectID: legacy.projectID, CanonicalRoot: legacy.root, Workspace: name, RunID: runID, State: model.StateStopped, Legacy: true, Resources: []state.ResourceRecord{record}}
	legacy.manifests.put(legacyManifest)
	legacy.runtime.resources["dsx-legacy"] = runtime.ResourceSnapshot{Resource: runtime.Resource{ID: "dsx-legacy", Name: "dsx-legacy", Kind: runtime.ResourceWorkspace}, State: "stopped", Labels: workspaceRuntimeLabels(labels)}
	if _, err := legacy.service.Remove(context.Background(), WorkspaceRemoveRequest{Root: legacy.root, Workspace: name, Confirmed: true}); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("implicit legacy cleanup error = %v", err)
	}
	legacy.runtime.resources["dsx-legacy"] = func() runtime.ResourceSnapshot {
		snapshot := legacy.runtime.resources["dsx-legacy"]
		snapshot.Labels[0].Value = "false"
		return snapshot
	}()
	if _, err := legacy.service.Remove(context.Background(), WorkspaceRemoveRequest{Root: legacy.root, Workspace: name, Confirmed: true, LegacyResources: true, DiscardUnfetched: true}); model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("ambiguous legacy cleanup error = %v", err)
	}
	if _, found := legacy.runtime.resources["dsx-legacy"]; !found {
		t.Fatal("ambiguous legacy resource was deleted")
	}
	if _, found, _ := legacy.manifests.LoadManifest(context.Background(), legacy.projectID, name, runID); !found {
		t.Fatal("ambiguous legacy manifest was deleted")
	}
}

func TestWorkspaceCreateLowersOnlyReviewedHostMounts(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	source := canonicalTemporaryDirectory(t)
	authority, err := resolveHostMount(source)
	if err != nil {
		t.Fatal(err)
	}
	fixture.execution.Mounts = []plan.ResolvedMount{{SourceType: "host", Source: authority.CanonicalPath, SourceIdentity: authority.Identity, Target: "/mnt/reviewed", ReadOnly: true}}
	if _, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: "mounts"}); err != nil {
		t.Fatal(err)
	}
	spec := fixture.runtime.workspaceSpecs["mounts"]
	var reviewed []runtime.Mount
	for _, mount := range spec.Mounts {
		if mount.Authority == runtime.MountAuthorityReviewedHost {
			reviewed = append(reviewed, mount)
		}
	}
	if got, want := reviewed, []runtime.Mount{{Source: authority.CanonicalPath, Target: "/mnt/reviewed", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityReviewedHost}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reviewed mounts = %#v, want %#v", got, want)
	}

	rejected := newWorkspaceFixture(t)
	rootAuthority, err := resolveHostMount(rejected.root)
	if err != nil {
		t.Fatal(err)
	}
	rejected.execution.Mounts = []plan.ResolvedMount{{SourceType: "host", Source: rootAuthority.CanonicalPath, SourceIdentity: rootAuthority.Identity, Target: "/mnt/project", ReadOnly: true}}
	if _, err := rejected.service.Create(context.Background(), WorkspaceCreateRequest{Root: rejected.root, Workspace: "alias"}); model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("project source alias error = %v", err)
	}
	if len(rejected.runtime.resources) != 0 {
		t.Fatal("rejected host source mutated runtime")
	}
	for _, test := range []struct {
		name  string
		mount plan.ResolvedMount
	}{
		{name: "writable", mount: plan.ResolvedMount{SourceType: "host", Source: authority.CanonicalPath, SourceIdentity: authority.Identity, Target: "/mnt/reviewed"}},
		{name: "changed identity", mount: plan.ResolvedMount{SourceType: "host", Source: authority.CanonicalPath, SourceIdentity: "changed", Target: "/mnt/reviewed", ReadOnly: true}},
		{name: "protected target", mount: plan.ResolvedMount{SourceType: "host", Source: authority.CanonicalPath, SourceIdentity: authority.Identity, Target: "/workspace/escape", ReadOnly: true}},
		{name: "unsupported source", mount: plan.ResolvedMount{SourceType: "workspace", Source: ".", Target: "/mnt/reviewed", ReadOnly: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			execution := *fixture.execution
			execution.Mounts = []plan.ResolvedMount{test.mount}
			if _, err := reviewedRuntimeMounts(execution); err == nil {
				t.Fatal("unsafe mount was accepted")
			}
		})
	}
}

func TestWorkspaceRemovalFailsClosedBeforeAnyDeletion(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	if _, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: "guard"}); err != nil {
		t.Fatal(err)
	}
	manifest := fixture.manifest("guard")
	owner, err := workspaceManifestResource(manifest, runtime.ResourceWorkspace, workspaceOwnerRole)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime.inspectErrors[runtime.ResourceID(owner.ExpectedID)] = errors.New("injected inspect uncertainty")
	if _, err := fixture.service.Remove(context.Background(), WorkspaceRemoveRequest{Root: fixture.root, Workspace: "guard", Confirmed: true, DiscardUnfetched: true}); model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatalf("removal uncertainty error = %v", err)
	}
	if fixture.runtime.deleteCalls != 0 {
		t.Fatalf("delete calls = %d", fixture.runtime.deleteCalls)
	}

	fixture.runtime.inspectErrors = map[runtime.ResourceID]error{}
	fixture.service.removalGuard = func(context.Context, state.Manifest, runtime.ResourceSnapshot) ([]string, error) {
		return nil, errors.New("injected removal guard uncertainty")
	}
	if _, err := fixture.service.Remove(context.Background(), WorkspaceRemoveRequest{Root: fixture.root, Workspace: "guard", Confirmed: true, DiscardUnfetched: true}); err == nil {
		t.Fatal("removal guard uncertainty was bypassed")
	}
	if fixture.runtime.deleteCalls != 0 {
		t.Fatalf("delete calls after guard error = %d", fixture.runtime.deleteCalls)
	}

	preflight := newWorkspaceFixture(t)
	if _, err := preflight.service.Create(context.Background(), WorkspaceCreateRequest{Root: preflight.root, Workspace: "preflight"}); err != nil {
		t.Fatal(err)
	}
	preflightManifest := preflight.manifest("preflight")
	volume, err := workspaceManifestResource(preflightManifest, runtime.ResourceVolume, workspaceSourceRole)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := preflight.runtime.resources[runtime.ResourceID(volume.ExpectedID)]
	snapshot.Labels[0].Value = "false"
	preflight.runtime.resources[runtime.ResourceID(volume.ExpectedID)] = snapshot
	if _, err := preflight.service.Remove(context.Background(), WorkspaceRemoveRequest{Root: preflight.root, Workspace: "preflight", Confirmed: true, DiscardUnfetched: true}); model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("resource preflight error = %v", err)
	}
	if preflight.runtime.deleteCalls != 0 {
		t.Fatalf("preflight deleted %d resources before ambiguity", preflight.runtime.deleteCalls)
	}
}

func TestWorkspaceLimitSerializesRacingCreates(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	fixture.execution.Limits.MaxConcurrentWorkspaces = 1
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, name := range []model.WorkspaceName{"alpha", "beta"} {
		go func(workspace model.WorkspaceName) {
			<-start
			_, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: workspace})
			errs <- err
		}(name)
	}
	close(start)
	var succeeded, rejected int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			succeeded++
		case model.ErrorCodeOf(err) == model.CodeConflict:
			rejected++
		default:
			t.Fatalf("unexpected create result: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("creates succeeded=%d rejected=%d", succeeded, rejected)
	}
	manifests, err := fixture.manifests.ListProjectManifests(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || manifests[0].State != model.StateRunning {
		t.Fatalf("admitted manifests = %#v", manifests)
	}
}

func TestWorkspaceLimitSerializesRacingStarts(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	fixture.execution.Limits.MaxConcurrentWorkspaces = 2
	for _, name := range []model.WorkspaceName{"alpha", "beta"} {
		if _, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: name}); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.Stop(context.Background(), WorkspaceStopRequest{Root: fixture.root, Workspace: name}); err != nil {
			t.Fatal(err)
		}
	}
	fixture.execution.Limits.MaxConcurrentWorkspaces = 1
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, name := range []model.WorkspaceName{"alpha", "beta"} {
		go func(workspace model.WorkspaceName) {
			<-start
			_, err := fixture.service.Start(context.Background(), WorkspaceStartRequest{Root: fixture.root, Workspace: workspace})
			errs <- err
		}(name)
	}
	close(start)
	var succeeded, rejected int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			succeeded++
		case model.ErrorCodeOf(err) == model.CodeConflict:
			rejected++
		default:
			t.Fatalf("unexpected start result: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("starts succeeded=%d rejected=%d", succeeded, rejected)
	}
	running := 0
	for _, snapshot := range fixture.runtime.resources {
		if snapshot.Kind == runtime.ResourceWorkspace && snapshot.State == "running" {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("running workspaces = %d", running)
	}
}

func TestWorkspaceLifecycleFailuresFinalizeDurablyAndReconcileRuntime(t *testing.T) {
	t.Run("canceled start before mutation", func(t *testing.T) {
		fixture := newWorkspaceFixture(t)
		if _, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: "cancel"}); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.Stop(context.Background(), WorkspaceStopRequest{Root: fixture.root, Workspace: "cancel"}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		fixture.runtime.startHook = cancel
		if _, err := fixture.service.Start(ctx, WorkspaceStartRequest{Root: fixture.root, Workspace: "cancel"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("start error = %v", err)
		}
		manifest := fixture.manifest("cancel")
		if manifest.State != model.StateStopped || manifest.Operation != "start" || !strings.Contains(manifest.Failure, "canceled") {
			t.Fatalf("finalized manifest = state %s operation %q failure %q", manifest.State, manifest.Operation, manifest.Failure)
		}
		list, err := fixture.service.List(context.Background(), WorkspaceListRequest{Root: fixture.root})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Workspaces) != 1 || !list.Workspaces[0].MutationActive {
			t.Fatalf("unfinished mutation not reported: %#v", list.Workspaces)
		}
		if _, err := fixture.service.Start(context.Background(), WorkspaceStartRequest{Root: fixture.root, Workspace: "cancel"}); model.ErrorCodeOf(err) != model.CodeConflict {
			t.Fatalf("unfinished operation was overwritten: %v", err)
		}
	})

	t.Run("start error after mutation", func(t *testing.T) {
		fixture := newWorkspaceFixture(t)
		if _, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: "post"}); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.Stop(context.Background(), WorkspaceStopRequest{Root: fixture.root, Workspace: "post"}); err != nil {
			t.Fatal(err)
		}
		fixture.runtime.startError = errors.New("injected post-start error " + strings.Repeat("x", 5000))
		fixture.runtime.startMutatesBeforeError = true
		if _, err := fixture.service.Start(context.Background(), WorkspaceStartRequest{Root: fixture.root, Workspace: "post"}); err == nil {
			t.Fatal("post-mutation start error was accepted")
		}
		manifest := fixture.manifest("post")
		if manifest.State != model.StateRunning || manifest.Operation != "start" || !strings.Contains(manifest.Failure, "post-start") || len(manifest.Failure) != 4096 {
			t.Fatalf("post-start reconciliation = state %s operation %q failure %q", manifest.State, manifest.Operation, manifest.Failure)
		}
	})

	t.Run("stop and restart errors", func(t *testing.T) {
		stopping := newWorkspaceFixture(t)
		if _, err := stopping.service.Create(context.Background(), WorkspaceCreateRequest{Root: stopping.root, Workspace: "stopping"}); err != nil {
			t.Fatal(err)
		}
		stopping.runtime.stopError = errors.New("injected stop error")
		if _, err := stopping.service.Stop(context.Background(), WorkspaceStopRequest{Root: stopping.root, Workspace: "stopping"}); err == nil {
			t.Fatal("stop error was accepted")
		}
		stoppedManifest := stopping.manifest("stopping")
		if stoppedManifest.State != model.StateRunning || stoppedManifest.Operation != "stop" || stoppedManifest.Failure == "" {
			t.Fatalf("stop reconciliation = %#v", stoppedManifest)
		}

		restarting := newWorkspaceFixture(t)
		if _, err := restarting.service.Create(context.Background(), WorkspaceCreateRequest{Root: restarting.root, Workspace: "restarting"}); err != nil {
			t.Fatal(err)
		}
		restarting.runtime.startError = errors.New("injected restart start error")
		if _, err := restarting.service.Restart(context.Background(), WorkspaceRestartRequest{Root: restarting.root, Workspace: "restarting"}); err == nil {
			t.Fatal("restart error was accepted")
		}
		restartedManifest := restarting.manifest("restarting")
		if restartedManifest.State != model.StateStopped || restartedManifest.Operation != "restart" || restartedManifest.Failure == "" {
			t.Fatalf("restart reconciliation = %#v", restartedManifest)
		}
	})
}

type workspaceFixture struct {
	root      string
	projectID model.ProjectID
	manifests *memoryWorkspaceManifests
	locks     *recordingWorkspaceLocks
	runtime   *recordingWorkspaceRuntime
	service   *WorkspaceService
	execution *plan.ExecutionPlan
}

func newWorkspaceFixture(t *testing.T) *workspaceFixture {
	t.Helper()
	root := canonicalTemporaryDirectory(t)
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	manifests := &memoryWorkspaceManifests{values: map[string]state.Manifest{}}
	locks := &recordingWorkspaceLocks{}
	fakeRuntime := &recordingWorkspaceRuntime{resources: map[runtime.ResourceID]runtime.ResourceSnapshot{}, inspectErrors: map[runtime.ResourceID]error{}, manifests: manifests, projectID: projectID, intentBeforeEveryCreate: true}
	git := &workspaceGitFake{t: t, root: root}
	execution := &plan.ExecutionPlan{ContractVersion: plan.ContractVersion, Project: plan.ProjectIdentity{ID: projectID, CanonicalRoot: root}, Agents: plan.AgentPlan{Allowed: []string{"codex", "omp"}, Default: "codex"}, Image: plan.ResolvedImage{Reference: "example.invalid/dev@sha256:" + strings.Repeat("a", 64)}, Repositories: []plan.RepositoryPlan{{Name: "workspace", HostPath: root, GuestPath: "/workspace"}}, Limits: plan.ResourceLimits{CPUs: 2, MemoryBytes: 1 << 30, MaxConcurrentWorkspaces: 3}, ExecutableHash: strings.Repeat("b", 64)}
	service := NewWorkspaceService(WorkspaceDependencies{ResolvePlan: func(context.Context, string) (plan.ExecutionPlan, error) { return *execution, nil }, Manifests: manifests, Locks: locks, Runtime: fakeRuntime, Git: git, TempRoot: t.TempDir(), NewRunID: sequentialRunIDs(), GuestHelperSource: func() (runtime.HostPath, error) {
		return runtime.HostPath(filepath.Join(t.TempDir(), "dsx-guest")), nil
	}, RemovalGuard: func(context.Context, state.Manifest, runtime.ResourceSnapshot) ([]string, error) { return nil, nil }})
	return &workspaceFixture{root: root, projectID: projectID, manifests: manifests, locks: locks, runtime: fakeRuntime, service: service, execution: execution}
}

func (fixture *workspaceFixture) manifest(name model.WorkspaceName) state.Manifest {
	values, _ := fixture.manifests.ListProjectManifests(context.Background(), fixture.projectID)
	for _, value := range values {
		if value.Workspace == name {
			return value
		}
	}
	return state.Manifest{}
}
func (fixture *workspaceFixture) resourceNamesFor(name model.WorkspaceName) []string {
	prefix := "dsx-"
	values := make([]string, 0)
	for id, snapshot := range fixture.runtime.resources {
		labels := map[string]string{}
		for _, label := range snapshot.Labels {
			labels[label.Key] = label.Value
		}
		if labels[state.OwnershipWorkspaceLabel] == string(name) && strings.HasPrefix(string(id), prefix) {
			values = append(values, string(id))
		}
	}
	sort.Strings(values)
	return values
}
func sequentialRunIDs() func(time.Time) (model.RunID, error) {
	var mu sync.Mutex
	value := 0
	return func(time.Time) (model.RunID, error) {
		mu.Lock()
		defer mu.Unlock()
		value++
		return model.ParseRunID("01890f5c-7b00-7000-8000-" + fmt12(value))
	}
}
func fmt12(value int) string {
	return strings.Repeat("0", 12-len(string(rune('0'+value)))) + string(rune('0'+value))
}

type memoryWorkspaceManifests struct {
	mu     sync.Mutex
	values map[string]state.Manifest
}

func manifestKey(project model.ProjectID, workspace model.WorkspaceName, run model.RunID) string {
	return string(project) + "/" + string(workspace) + "/" + string(run)
}
func (repository *memoryWorkspaceManifests) put(manifest state.Manifest) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	repository.values[manifestKey(manifest.ProjectID, manifest.Workspace, manifest.RunID)] = cloneWorkspaceTestManifest(manifest)
}
func (repository *memoryWorkspaceManifests) CreateIntent(_ context.Context, manifest state.Manifest) error {
	if err := state.ValidateManifest(manifest); err != nil {
		return err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	key := manifestKey(manifest.ProjectID, manifest.Workspace, manifest.RunID)
	if _, ok := repository.values[key]; ok {
		return errors.New("duplicate")
	}
	repository.values[key] = cloneWorkspaceTestManifest(manifest)
	return nil
}
func (repository *memoryWorkspaceManifests) LoadManifest(_ context.Context, p model.ProjectID, w model.WorkspaceName, r model.RunID) (state.Manifest, bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	value, ok := repository.values[manifestKey(p, w, r)]
	return cloneWorkspaceTestManifest(value), ok, nil
}
func (repository *memoryWorkspaceManifests) ReplaceManifest(_ context.Context, manifest state.Manifest, expected uint64) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	key := manifestKey(manifest.ProjectID, manifest.Workspace, manifest.RunID)
	current, ok := repository.values[key]
	if !ok || current.Generation != expected {
		return errors.New("generation conflict")
	}
	if err := state.ValidateManifest(manifest); err != nil {
		return err
	}
	manifest.Generation = expected + 1
	repository.values[key] = cloneWorkspaceTestManifest(manifest)
	return nil
}
func (repository *memoryWorkspaceManifests) ListProjectManifests(_ context.Context, p model.ProjectID) ([]state.Manifest, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	values := make([]state.Manifest, 0)
	for _, value := range repository.values {
		if value.ProjectID == p {
			values = append(values, cloneWorkspaceTestManifest(value))
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Workspace < values[j].Workspace })
	return values, nil
}
func (repository *memoryWorkspaceManifests) ListAllManifests(context.Context) ([]state.Manifest, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	values := make([]state.Manifest, 0, len(repository.values))
	for _, value := range repository.values {
		values = append(values, cloneWorkspaceTestManifest(value))
	}
	return values, nil
}
func (repository *memoryWorkspaceManifests) DeleteManifest(_ context.Context, p model.ProjectID, w model.WorkspaceName, r model.RunID) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	delete(repository.values, manifestKey(p, w, r))
	return nil
}

type recordingWorkspaceLocks struct {
	mu      sync.Mutex
	project sync.Mutex
	events  []string
}
type recordingWorkspaceLock struct {
	parent  *recordingWorkspaceLocks
	name    string
	release func()
}

func (locks *recordingWorkspaceLocks) LockWorkspace(_ context.Context, _ model.ProjectID, w model.WorkspaceName) (state.ProjectLock, error) {
	locks.mu.Lock()
	locks.events = append(locks.events, "workspace:"+string(w))
	locks.mu.Unlock()
	return &recordingWorkspaceLock{parent: locks, name: "workspace:" + string(w)}, nil
}
func (locks *recordingWorkspaceLocks) LockProject(context.Context, model.ProjectID) (state.ProjectLock, error) {
	locks.project.Lock()
	locks.mu.Lock()
	locks.events = append(locks.events, "project")
	locks.mu.Unlock()
	return &recordingWorkspaceLock{parent: locks, name: "project", release: locks.project.Unlock}, nil
}
func (lock *recordingWorkspaceLock) Unlock() error {
	lock.parent.mu.Lock()
	lock.parent.events = append(lock.parent.events, "unlock:"+lock.name)
	lock.parent.mu.Unlock()
	if lock.release != nil {
		lock.release()
	}
	return nil
}

type recordingWorkspaceRuntime struct {
	resources                  map[runtime.ResourceID]runtime.ResourceSnapshot
	inspectErrors              map[runtime.ResourceID]error
	manifests                  *memoryWorkspaceManifests
	projectID                  model.ProjectID
	workspaceSpecs             map[string]runtime.WorkspaceSpec
	failCreateKind             runtime.ResourceKind
	intentBeforeEveryCreate    bool
	execs                      []runtime.ExecSpec
	starts, stops, deleteCalls int
	startError, stopError      error
	startMutatesBeforeError    bool
	startHook                  func()
}

func (fake *recordingWorkspaceRuntime) Probe(context.Context) (runtime.Capabilities, error) {
	return runtime.Capabilities{}, nil
}
func (fake *recordingWorkspaceRuntime) EnsureImage(context.Context, runtime.ImageSpec) (runtime.Image, error) {
	return runtime.Image{Reference: "image", Digest: strings.Repeat("a", 64)}, nil
}
func (fake *recordingWorkspaceRuntime) create(name string, kind runtime.ResourceKind, labels []runtime.Label) (runtime.Resource, error) {
	if fake.failCreateKind == kind {
		return runtime.Resource{}, errors.New("injected create failure")
	}
	manifests, _ := fake.manifests.ListProjectManifests(context.Background(), fake.projectID)
	found := false
	for _, manifest := range manifests {
		for _, record := range manifest.Resources {
			if record.ExpectedID == name && !record.Created {
				found = true
			}
		}
	}
	fake.intentBeforeEveryCreate = fake.intentBeforeEveryCreate && found
	resource := runtime.Resource{ID: runtime.ResourceID(name), Name: name, Kind: kind}
	fake.resources[resource.ID] = runtime.ResourceSnapshot{Resource: resource, State: "created", Labels: append([]runtime.Label(nil), labels...)}
	return resource, nil
}
func (fake *recordingWorkspaceRuntime) CreateVolume(_ context.Context, s runtime.VolumeSpec) (runtime.Resource, error) {
	return fake.create(s.Name, runtime.ResourceVolume, s.Labels)
}
func (fake *recordingWorkspaceRuntime) CreateAuthLoginVolume(context.Context, runtime.AuthLoginVolumeSpec) (runtime.Resource, error) {
	return runtime.Resource{}, errors.New("unexpected")
}
func (fake *recordingWorkspaceRuntime) CreateNetwork(_ context.Context, s runtime.NetworkSpec) (runtime.Resource, error) {
	return fake.create(s.Name, runtime.ResourceNetwork, s.Labels)
}
func (fake *recordingWorkspaceRuntime) CreateWorkspace(_ context.Context, s runtime.WorkspaceSpec) (runtime.Resource, error) {
	resource, err := fake.create(s.Name, runtime.ResourceWorkspace, s.Labels)
	if err == nil {
		if fake.workspaceSpecs == nil {
			fake.workspaceSpecs = map[string]runtime.WorkspaceSpec{}
		}
		labels := map[string]string{}
		for _, label := range s.Labels {
			labels[label.Key] = label.Value
		}
		fake.workspaceSpecs[labels[state.OwnershipWorkspaceLabel]] = s
		snapshot := fake.resources[resource.ID]
		snapshot.Mounts = append([]runtime.Mount(nil), s.Mounts...)
		snapshot.Networks = append([]string(nil), s.Networks...)
		fake.resources[resource.ID] = snapshot
	}
	return resource, err
}
func (fake *recordingWorkspaceRuntime) CreateBrowser(context.Context, runtime.BrowserSpec) (runtime.Resource, error) {
	return runtime.Resource{}, errors.New("unexpected")
}
func (fake *recordingWorkspaceRuntime) CreateAuthLogin(context.Context, runtime.AuthLoginSpec) (runtime.Resource, error) {
	return runtime.Resource{}, errors.New("unexpected")
}
func (fake *recordingWorkspaceRuntime) StartWorkspace(ctx context.Context, s runtime.ResourceSnapshot) error {
	if fake.startHook != nil {
		fake.startHook()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if fake.startError != nil && !fake.startMutatesBeforeError {
		return fake.startError
	}
	current := fake.resources[s.ID]
	current.State = "running"
	fake.resources[s.ID] = current
	fake.starts++
	return fake.startError
}
func (fake *recordingWorkspaceRuntime) StartAuthLogin(context.Context, runtime.ResourceSnapshot) error {
	return errors.New("unexpected")
}
func (fake *recordingWorkspaceRuntime) PrepareExec(context.Context, runtime.ResourceSnapshot, runtime.ExecSpec) (runtime.ProcessSpec, error) {
	return runtime.ProcessSpec{}, errors.New("unexpected")
}
func (fake *recordingWorkspaceRuntime) Exec(_ context.Context, _ runtime.ResourceSnapshot, s runtime.ExecSpec, _ runtime.ExecIO) (runtime.Exit, error) {
	fake.execs = append(fake.execs, s)
	code := 0
	return runtime.Exit{Code: &code}, nil
}
func (fake *recordingWorkspaceRuntime) CopyTo(context.Context, runtime.ResourceSnapshot, runtime.HostPath, runtime.GuestPath) error {
	return nil
}
func (fake *recordingWorkspaceRuntime) CopyFrom(context.Context, runtime.ResourceSnapshot, runtime.GuestPath, runtime.HostPath) error {
	return errors.New("unexpected")
}
func (fake *recordingWorkspaceRuntime) Inspect(_ context.Context, id runtime.ResourceID) (runtime.ResourceSnapshot, error) {
	if err := fake.inspectErrors[id]; err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	snapshot, ok := fake.resources[id]
	if !ok {
		return runtime.ResourceSnapshot{}, runtime.ErrResourceNotFound
	}
	return snapshot, nil
}
func (fake *recordingWorkspaceRuntime) List(_ context.Context, kind runtime.ResourceKind) ([]runtime.ResourceSnapshot, error) {
	values := make([]runtime.ResourceSnapshot, 0)
	for _, value := range fake.resources {
		if value.Kind == kind {
			values = append(values, value)
		}
	}
	return values, nil
}
func (fake *recordingWorkspaceRuntime) Signal(context.Context, runtime.ResourceSnapshot, runtime.Signal) error {
	return nil
}
func (fake *recordingWorkspaceRuntime) Stop(ctx context.Context, s runtime.ResourceSnapshot, _ runtime.StopPolicy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if fake.stopError != nil {
		return fake.stopError
	}
	current := fake.resources[s.ID]
	current.State = "stopped"
	fake.resources[s.ID] = current
	fake.stops++
	return nil
}
func (fake *recordingWorkspaceRuntime) Delete(_ context.Context, s runtime.ResourceSnapshot) error {
	fake.deleteCalls++
	delete(fake.resources, s.ID)
	return nil
}

type workspaceGitFake struct {
	t    *testing.T
	root string
}

func (fake *workspaceGitFake) ValidateRepository(context.Context, gitx.Repository) error { return nil }
func (fake *workspaceGitFake) PrepareSource(_ context.Context, request gitx.SourceRequest) (gitx.SourceArtifact, error) {
	bundle := filepath.Join(fake.t.TempDir(), "source.bundle")
	if err := os.WriteFile(bundle, []byte("bundle"), 0o600); err != nil {
		return gitx.SourceArtifact{}, err
	}
	repository := request.Repository
	repository.Identity = workspaceRepositoryIdentity(fake.t, fake.root)
	return gitx.SourceArtifact{Repository: repository, SourceBranch: "refs/heads/main", SourceRevision: strings.Repeat("1", 40), TrackedFingerprint: strings.Repeat("2", 64), BundlePath: bundle, BundleDigest: strings.Repeat("3", 64), BundleRef: "refs/dsx/source"}, nil
}
func (fake *workspaceGitFake) PrepareUpdateSource(context.Context, gitx.UpdateSourceRequest) (gitx.SourceArtifact, error) {
	return gitx.SourceArtifact{}, errors.New("unexpected")
}
func (fake *workspaceGitFake) VerifyBundle(context.Context, string, string) error { return nil }
func (fake *workspaceGitFake) FetchResult(context.Context, gitx.FetchRequest) (gitx.FetchResult, error) {
	return gitx.FetchResult{}, errors.New("unexpected")
}
func (fake *workspaceGitFake) Status(context.Context, gitx.StatusRequest) (gitx.Status, error) {
	return gitx.Status{}, nil
}
func (fake *workspaceGitFake) Diff(context.Context, gitx.DiffRequest) (gitx.DiffResult, error) {
	return gitx.DiffResult{}, errors.New("unexpected")
}
func (fake *workspaceGitFake) PrepareApply(context.Context, gitx.ApplyRequest) (gitx.ApplyTransaction, error) {
	return nil, errors.New("unexpected")
}
func (fake *workspaceGitFake) RemoveArtifact(name string) error {
	err := os.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func workspaceRepositoryIdentity(t *testing.T, root string) gitx.RepositoryIdentity {
	t.Helper()
	return gitx.RepositoryIdentity{ApprovedRoot: workspacePhysicalIdentity(t, root), Worktree: workspacePhysicalIdentity(t, root), GitDir: workspacePhysicalIdentity(t, filepath.Join(root, ".git"))}
}
func workspacePhysicalIdentity(t *testing.T, value string) gitx.PhysicalPathIdentity {
	t.Helper()
	current := string(filepath.Separator)
	rootInfo, err := os.Stat(current)
	if err != nil {
		t.Fatal(err)
	}
	rootStat := rootInfo.Sys().(*syscall.Stat_t)
	components := []gitx.PathComponentIdentity{{Path: current, Device: uint64(rootStat.Dev), Inode: uint64(rootStat.Ino)}}
	for _, component := range strings.Split(strings.TrimPrefix(value, current), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Stat(current)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		components = append(components, gitx.PathComponentIdentity{Path: current, Device: uint64(stat.Dev), Inode: uint64(stat.Ino)})
	}
	return gitx.PhysicalPathIdentity{CanonicalPath: value, Components: components}
}

func TestWorkspaceCreateRejectsDisplayedSourceRevisionChangeBeforeMutation(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	_, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{
		Root: fixture.root, Workspace: "stale-source",
		SourceBranch: "refs/heads/main", SourceRevision: strings.Repeat("0", 40),
	})
	if model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("Create stale source error = %v", err)
	}
	if len(fixture.runtime.resources) != 0 {
		t.Fatalf("runtime resources created before source identity check: %#v", fixture.runtime.resources)
	}
	manifests, listErr := fixture.manifests.ListProjectManifests(context.Background(), fixture.projectID)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(manifests) != 0 {
		t.Fatalf("manifest created before source identity check: %#v", manifests)
	}
}

func TestWorkspaceRemoveRevokesAWSBeforePublicationAndResourceCleanup(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	manager := newRecordingHostAWSManager()
	configureHostDefaultAWS(t, fixture, manager)
	createAWSWorkspace(t, fixture, "remove-aws")
	if _, err := fixture.service.EnableAWS(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "remove-aws"}); err != nil {
		t.Fatal(err)
	}
	manager.onRemove = func(identity bridge.LeaseIdentity) {
		manifest := fixture.manifest(identity.Workspace)
		if manifest.AWSGrant == nil || manifest.AWSGrant.Enabled {
			t.Fatalf("manifest at publication removal = %#v", manifest)
		}
		if fixture.runtime.deleteCalls != 0 {
			t.Fatalf("runtime cleanup preceded AWS removal: %d deletes", fixture.runtime.deleteCalls)
		}
	}
	result, err := fixture.service.Remove(context.Background(), WorkspaceRemoveRequest{
		Root: fixture.root, Workspace: "remove-aws", Confirmed: true, DiscardUnfetched: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.DeletedManifest || manager.state("remove-aws").State != "" {
		t.Fatalf("remove result=%#v mirror=%#v", result, manager.state("remove-aws"))
	}
}

func TestWorkspaceInternalGitTemporaryStartKeepsAWSUnpublished(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	manager := newRecordingHostAWSManager()
	configureHostDefaultAWS(t, fixture, manager)
	createAWSWorkspace(t, fixture, "git-aws")
	if _, err := fixture.service.Stop(context.Background(), WorkspaceStopRequest{Root: fixture.root, Workspace: "git-aws"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.EnableAWS(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "git-aws"}); err != nil {
		t.Fatal(err)
	}
	manager.events = nil
	fixture.runtime.startHook = func() {
		if !slices.Equal(manager.events, []string{"disable:git-aws"}) || manager.state("git-aws").State != AWSMirrorDisabled {
			t.Fatalf("temporary start exposed AWS: events=%v state=%#v", manager.events, manager.state("git-aws"))
		}
	}
	access, unlock, finish, err := fixture.service.workspaceGitAccess(context.Background(), fixture.root, "git-aws")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.workspaceCommand(context.Background(), access.Workspace, []string{"/usr/bin/true"}, "/workspace", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(fixture.runtime.execs) == 0 {
		t.Fatal("internal command was not executed")
	}
	for _, environment := range fixture.runtime.execs[len(fixture.runtime.execs)-1].Env {
		if strings.HasPrefix(environment, "AWS_") {
			t.Fatalf("internal Git command received AWS environment: %v", fixture.runtime.execs[len(fixture.runtime.execs)-1].Env)
		}
	}
	if err := errors.Join(finish(false), unlock()); err != nil {
		t.Fatal(err)
	}
}
