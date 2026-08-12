package app

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/ownership"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

func TestBrowserRuntimeContractIsNetworkOnly(t *testing.T) {
	root := t.TempDir()
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := model.ParseWorkspaceName("browser-contract")
	if err != nil {
		t.Fatal(err)
	}
	runID, err := model.ParseRunID("01890f5c-7b00-7000-8000-000000000039")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ownership.NewIdentity(projectID, root, workspace, runID, runtime.ResourceBrowser, browserRole)
	if err != nil {
		t.Fatal(err)
	}
	record := identity.ManifestRecord()
	record.Created = true
	record.RuntimeID = record.ExpectedID
	network := "private-workspace-network"
	snapshot := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(record.ExpectedID), Name: record.Name, Kind: runtime.ResourceBrowser},
		State:    "running", ImageDigest: "sha256:" + browserTestDigest,
		Labels: identity.Labels(), Networks: []string{network},
	}
	if err := verifyBrowserSnapshot(record, snapshot, network, browserTestDigest, true); err != nil {
		t.Fatal(err)
	}
	browserType := reflect.TypeOf(runtime.BrowserSpec{})
	for _, forbidden := range []string{"Mounts", "Ports", "WorkingDir", "User", "HostPath", "Volumes"} {
		if _, found := browserType.FieldByName(forbidden); found {
			t.Fatalf("BrowserSpec exposes forbidden field %q", forbidden)
		}
	}
	snapshot.Mounts = []runtime.Mount{{Target: "/workspace"}}
	if err := verifyBrowserSnapshot(record, snapshot, network, browserTestDigest, true); err == nil {
		t.Fatal("browser snapshot with a source mount was accepted")
	}
}

func TestBrowserNetworkAddressRequiresOnePrivateIPv4(t *testing.T) {
	network := "private-workspace-network"
	for _, test := range []struct {
		name      string
		addresses []netip.Addr
		want      string
	}{
		{name: "private IPv4", addresses: []netip.Addr{netip.MustParseAddr("fd00::10"), netip.MustParseAddr("192.168.64.7")}, want: "192.168.64.7"},
		{name: "public rejected", addresses: []netip.Addr{netip.MustParseAddr("203.0.113.7")}},
		{name: "ambiguous rejected", addresses: []netip.Addr{netip.MustParseAddr("192.168.64.7"), netip.MustParseAddr("192.168.64.8")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			address, err := browserNetworkIPv4(runtime.ResourceSnapshot{NetworkAddresses: map[string][]netip.Addr{network: test.addresses}}, network)
			if test.want == "" && err == nil {
				t.Fatalf("browserNetworkIPv4() = %s, want error", address)
			}
			if test.want != "" && (err != nil || address.String() != test.want) {
				t.Fatalf("browserNetworkIPv4() = %s, %v; want %s", address, err, test.want)
			}
		})
	}
}

func TestManifestResourceIndexRejectsDuplicateBrowserRecords(t *testing.T) {
	records := []state.ResourceRecord{
		{Kind: string(runtime.ResourceBrowser)},
		{Kind: string(runtime.ResourceBrowser)},
	}
	if _, found := manifestResourceIndex(records, runtime.ResourceBrowser); found {
		t.Fatal("duplicate browser ownership records were accepted")
	}
}

type browserStateRepository struct {
	manifest    state.Manifest
	workspaceMu sync.Mutex
	projectMu   sync.Mutex
}

func (repository *browserStateRepository) CreateIntent(_ context.Context, manifest state.Manifest) error {
	repository.manifest = manifest
	return nil
}
func (repository *browserStateRepository) LoadManifest(_ context.Context, projectID model.ProjectID, workspace model.WorkspaceName, runID model.RunID) (state.Manifest, bool, error) {
	if repository.manifest.ProjectID != projectID || repository.manifest.Workspace != workspace || repository.manifest.RunID != runID {
		return state.Manifest{}, false, nil
	}
	return repository.manifest, true, nil
}
func (repository *browserStateRepository) ReplaceManifest(_ context.Context, manifest state.Manifest, expected uint64) error {
	if repository.manifest.Generation != expected {
		return errors.New("manifest generation conflict")
	}
	manifest.Generation = expected + 1
	repository.manifest = manifest
	return nil
}
func (repository *browserStateRepository) ListProjectManifests(_ context.Context, projectID model.ProjectID) ([]state.Manifest, error) {
	if repository.manifest.ProjectID != projectID {
		return nil, nil
	}
	return []state.Manifest{repository.manifest}, nil
}
func (repository *browserStateRepository) ListAllManifests(context.Context) ([]state.Manifest, error) {
	return []state.Manifest{repository.manifest}, nil
}
func (repository *browserStateRepository) DeleteManifest(context.Context, model.ProjectID, model.WorkspaceName, model.RunID) error {
	repository.manifest = state.Manifest{}
	return nil
}
func (repository *browserStateRepository) LockProject(context.Context, model.ProjectID) (state.ProjectLock, error) {
	repository.projectMu.Lock()
	return browserStateLock{mutex: &repository.projectMu}, nil
}
func (repository *browserStateRepository) LockWorkspace(context.Context, model.ProjectID, model.WorkspaceName) (state.ProjectLock, error) {
	repository.workspaceMu.Lock()
	return browserStateLock{mutex: &repository.workspaceMu}, nil
}

type browserStateLock struct{ mutex *sync.Mutex }

func (lock browserStateLock) Unlock() error { lock.mutex.Unlock(); return nil }

type browserSessionRuntime struct {
	*guestClientAdapter
	resources map[runtime.ResourceID]runtime.ResourceSnapshot
	spec      runtime.BrowserSpec
	creates   int
	deletes   int
	startErr  error
}

func (adapter *browserSessionRuntime) EnsureImage(_ context.Context, spec runtime.ImageSpec) (runtime.Image, error) {
	return runtime.Image{Reference: spec.Reference, Digest: "sha256:" + browserTestDigest}, nil
}

func (adapter *browserSessionRuntime) CreateBrowser(_ context.Context, spec runtime.BrowserSpec) (runtime.Resource, error) {
	adapter.creates++
	adapter.spec = spec
	resource := runtime.Resource{ID: runtime.ResourceID(spec.Name), Name: spec.Name, Kind: runtime.ResourceBrowser}
	adapter.resources[resource.ID] = runtime.ResourceSnapshot{
		Resource: resource, State: "created", ImageDigest: spec.Image.Digest,
		Labels: append([]runtime.Label(nil), spec.Labels...), Networks: append([]string(nil), spec.Networks...),
		NetworkAddresses: map[string][]netip.Addr{spec.Networks[0]: {netip.MustParseAddr("192.168.64.10")}},
	}
	return resource, nil
}

func (adapter *browserSessionRuntime) StartWorkspace(_ context.Context, snapshot runtime.ResourceSnapshot) error {
	if adapter.startErr != nil {
		return adapter.startErr
	}
	snapshot.State = "running"
	adapter.resources[snapshot.ID] = snapshot
	return nil
}

func (adapter *browserSessionRuntime) Inspect(_ context.Context, id runtime.ResourceID) (runtime.ResourceSnapshot, error) {
	snapshot, found := adapter.resources[id]
	if !found {
		return runtime.ResourceSnapshot{}, runtime.ErrResourceNotFound
	}
	return snapshot, nil
}

func (adapter *browserSessionRuntime) Exec(_ context.Context, _ runtime.ResourceSnapshot, _ runtime.ExecSpec, _ runtime.ExecIO) (runtime.Exit, error) {
	code := 0
	return runtime.Exit{Code: &code}, nil
}

func (adapter *browserSessionRuntime) Stop(_ context.Context, snapshot runtime.ResourceSnapshot, _ runtime.StopPolicy) error {
	snapshot.State = "stopped"
	adapter.resources[snapshot.ID] = snapshot
	return nil
}

func (adapter *browserSessionRuntime) Delete(_ context.Context, snapshot runtime.ResourceSnapshot) error {
	delete(adapter.resources, snapshot.ID)
	adapter.deletes++
	return nil
}

func TestBrowserSessionIsNewEachTimeAndDeletedAtSessionEnd(t *testing.T) {
	service, access, fake := browserSessionFixture(t)
	for sessionNumber := range 2 {
		session, err := service.createBrowserSession(context.Background(), access)
		if err != nil {
			t.Fatal(err)
		}
		if session.Server.Name != browserMCPName {
			t.Fatalf("session %d MCP = %#v", sessionNumber, session.Server)
		}
		index, found := manifestResourceIndex(access.Manifest.Resources, runtime.ResourceBrowser)
		if !found {
			t.Fatalf("session %d browser intent missing", sessionNumber)
		}
		if err := service.deleteBrowserWithAccess(context.Background(), access, index); err != nil {
			t.Fatal(err)
		}
	}
	if fake.creates != 2 || fake.deletes != 2 {
		t.Fatalf("browser lifetime creates=%d deletes=%d, want two disposable sessions", fake.creates, fake.deletes)
	}
	if len(fake.spec.Networks) != 1 || fake.spec.Networks[0] != access.Network.Name || len(fake.spec.Env) != 0 {
		t.Fatalf("browser isolation spec = %#v", fake.spec)
	}
}

func TestBrowserStartupCancellationDeletesCreatedResource(t *testing.T) {
	service, access, fake := browserSessionFixture(t)
	fake.startErr = context.Canceled
	if _, err := service.createBrowserSession(context.Background(), access); !errors.Is(err, context.Canceled) {
		t.Fatalf("createBrowserSession() error = %v, want cancellation", err)
	}
	if fake.creates != 1 || fake.deletes != 1 {
		t.Fatalf("cancellation cleanup creates=%d deletes=%d", fake.creates, fake.deletes)
	}
}

func TestWorkspaceStopDeletesOrphanedSessionBrowser(t *testing.T) {
	assertWorkspaceLifecycleDeletesOrphanedBrowser(t, "stop")
}

func TestWorkspaceRestartDeletesOrphanedSessionBrowser(t *testing.T) {
	assertWorkspaceLifecycleDeletesOrphanedBrowser(t, "restart")
}

func TestWorkspaceStopRefusesAmbiguousOrphanedSessionBrowser(t *testing.T) {
	service, access, fake := browserSessionFixture(t)
	session, err := service.createBrowserSession(context.Background(), access)
	if err != nil {
		t.Fatal(err)
	}
	id := runtime.ResourceID(session.Record.ExpectedID)
	browser := fake.resources[id]
	browser.Labels[0].Value = "foreign"
	fake.resources[id] = browser
	if _, err := service.workspaces.Stop(context.Background(), WorkspaceStopRequest{
		Root: access.Manifest.CanonicalRoot, Workspace: access.Manifest.Workspace,
	}); model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("Stop() error = %v, want ambiguous ownership", err)
	}
	if fake.deletes != 0 {
		t.Fatalf("ambiguous browser was deleted")
	}
	workspace, err := fake.Inspect(context.Background(), access.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.State != "running" {
		t.Fatalf("workspace stopped before ambiguous browser was resolved: %q", workspace.State)
	}
}

func assertWorkspaceLifecycleDeletesOrphanedBrowser(t *testing.T, operation string) {
	t.Helper()
	service, access, fake := browserSessionFixture(t)
	session, err := service.createBrowserSession(context.Background(), access)
	if err != nil {
		t.Fatal(err)
	}
	switch operation {
	case "stop":
		_, err = service.workspaces.Stop(context.Background(), WorkspaceStopRequest{
			Root: access.Manifest.CanonicalRoot, Workspace: access.Manifest.Workspace,
		})
	case "restart":
		_, err = service.workspaces.Restart(context.Background(), WorkspaceRestartRequest{
			Root: access.Manifest.CanonicalRoot, Workspace: access.Manifest.Workspace,
		})
	default:
		t.Fatalf("unsupported lifecycle operation %q", operation)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, inspectErr := fake.Inspect(context.Background(), runtime.ResourceID(session.Record.ExpectedID)); !errors.Is(inspectErr, runtime.ErrResourceNotFound) {
		t.Fatalf("%s left orphaned browser: %v", operation, inspectErr)
	}
	if fake.deletes != 1 {
		t.Fatalf("%s browser deletes = %d, want 1", operation, fake.deletes)
	}
	manifest, err := service.workspaces.oneWorkspaceManifest(context.Background(), access.Manifest.ProjectID, access.Manifest.Workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	index, found := manifestResourceIndex(manifest.Resources, runtime.ResourceBrowser)
	if !found || !manifest.Resources[index].Deleted || !manifest.Resources[index].Absent {
		t.Fatalf("%s browser cleanup evidence = %#v", operation, manifest.Resources)
	}
	workspace, err := fake.Inspect(context.Background(), access.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantState := "stopped"
	if operation == "restart" {
		wantState = "running"
	}
	if workspace.State != wantState {
		t.Fatalf("%s workspace state = %q, want %q", operation, workspace.State, wantState)
	}
}

func browserSessionFixture(t *testing.T) (*AgentService, workspaceAccess, *browserSessionRuntime) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := model.WorkspaceName("browser-session")
	runID := model.RunID("01890f5c-7b00-7000-8000-000000000042")
	workspaceIdentity, err := ownership.NewIdentity(projectID, root, workspace, runID, runtime.ResourceWorkspace, workspaceOwnerRole)
	if err != nil {
		t.Fatal(err)
	}
	networkIdentity, err := ownership.NewIdentity(projectID, root, workspace, runID, runtime.ResourceNetwork, workspaceNetworkRole)
	if err != nil {
		t.Fatal(err)
	}
	workspaceRecord := workspaceIdentity.ManifestRecord()
	workspaceRecord.Created, workspaceRecord.RuntimeID = true, workspaceRecord.ExpectedID
	networkRecord := networkIdentity.ManifestRecord()
	networkRecord.Created, networkRecord.RuntimeID = true, networkRecord.ExpectedID
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	manifest := state.Manifest{
		Version: state.ManifestVersion, Generation: 1, ProjectID: projectID, CanonicalRoot: root,
		Workspace: workspace, RunID: runID, PlanHash: browserTestDigest, State: model.StateRunning,
		Operation: "", Resources: []state.ResourceRecord{networkRecord, workspaceRecord},
		CreatedAt: now, UpdatedAt: now,
		Git: []state.GitRecord{{
			Repository: "workspace", HostPath: root, GuestPath: "/workspace",
			Identity: gitx.RepositoryIdentity{
				ApprovedRoot: browserPhysicalIdentity(root), Worktree: browserPhysicalIdentity(root),
				GitDir: browserPhysicalIdentity(filepath.Join(root, ".git")),
			},
			SourceBranch: "refs/heads/main", SourceRevision: strings.Repeat("1", 40),
			TrackedFingerprint: strings.Repeat("2", 64), WorkspaceBranch: "dsx/browser-session",
			SourceBundleDigest: strings.Repeat("3", 64),
		}},
	}
	if err := state.ValidateManifest(manifest); err != nil {
		t.Fatal(err)
	}
	repository := &browserStateRepository{manifest: manifest}
	workspaceSnapshot := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(workspaceRecord.ExpectedID), Name: workspaceRecord.Name, Kind: runtime.ResourceWorkspace},
		State:    "running", Labels: workspaceIdentity.Labels(), Networks: []string{networkRecord.Name},
	}
	fake := &browserSessionRuntime{
		guestClientAdapter: &guestClientAdapter{},
		resources:          make(map[runtime.ResourceID]runtime.ResourceSnapshot),
	}
	fake.resources[workspaceSnapshot.ID] = workspaceSnapshot
	workspaces := NewWorkspaceService(WorkspaceDependencies{
		Manifests: repository, Locks: repository, Runtime: fake, Now: func() time.Time { return now.Add(time.Second) },
	})
	execution := plan.ExecutionPlan{
		ContractVersion: plan.ContractVersion, Project: plan.ProjectIdentity{ID: projectID, CanonicalRoot: root},
		Browser: &plan.BrowserPlan{ImageReference: "example/browser@sha256:" + browserTestDigest, ImageDigest: browserTestDigest},
		Limits:  plan.ResourceLimits{CPUs: 2, MemoryBytes: 2 << 30}, ExecutableHash: browserTestDigest,
	}
	workspaces.resolvePlan = func(context.Context, string) (plan.ExecutionPlan, error) {
		return execution, nil
	}
	access := workspaceAccess{Manifest: &manifest, Plan: execution, Workspace: workspaceSnapshot, Network: networkRecord}
	return &AgentService{workspaces: workspaces}, access, fake
}

func browserPhysicalIdentity(value string) gitx.PhysicalPathIdentity {
	components := []gitx.PathComponentIdentity{{Path: string(filepath.Separator), Device: 1, Inode: 1}}
	current := string(filepath.Separator)
	for index, part := range strings.Split(strings.TrimPrefix(value, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		components = append(components, gitx.PathComponentIdentity{Path: current, Device: 1, Inode: uint64(index + 2)})
	}
	return gitx.PhysicalPathIdentity{CanonicalPath: value, Components: components}
}

const browserTestDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
