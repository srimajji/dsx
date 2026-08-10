package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/ports"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

const (
	cloneTestCommit      = "1111111111111111111111111111111111111111"
	cloneTestResult      = "2222222222222222222222222222222222222222"
	cloneTestDigest      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cloneTestFingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type cloneGitStub struct {
	prepare      func(context.Context, gitx.SourceRequest) (gitx.SourceArtifact, error)
	prepareApply func(context.Context, gitx.ApplyRequest) (gitx.ApplyTransaction, error)
	removed      []string
	verified     []string
	statusCalls  []gitx.StatusRequest
	diffCalls    []gitx.DiffRequest
	fetchCalls   []gitx.FetchRequest
	applyCalls   []gitx.ApplyRequest
	status       gitx.Status
	fetch        gitx.FetchResult
	apply        gitx.ApplyResult
	verifyErr    error
	statusErr    error
	validateErr  error
}

func (stub *cloneGitStub) ValidateRepository(context.Context, gitx.Repository) error {
	return stub.validateErr
}

func (stub *cloneGitStub) PrepareSource(ctx context.Context, request gitx.SourceRequest) (gitx.SourceArtifact, error) {
	if stub.prepare == nil {
		return gitx.SourceArtifact{}, errors.New("unexpected PrepareSource")
	}
	return stub.prepare(ctx, request)
}
func (stub *cloneGitStub) VerifyBundle(_ context.Context, path, _ string) error {
	stub.verified = append(stub.verified, path)
	return stub.verifyErr
}
func (stub *cloneGitStub) FetchResult(_ context.Context, request gitx.FetchRequest) (gitx.FetchResult, error) {
	stub.fetchCalls = append(stub.fetchCalls, request)
	result := stub.fetch
	result.Repository = request.Repository.Name
	return result, nil
}
func (stub *cloneGitStub) Status(_ context.Context, request gitx.StatusRequest) (gitx.Status, error) {
	stub.statusCalls = append(stub.statusCalls, request)
	if stub.statusErr != nil {
		return gitx.Status{}, stub.statusErr
	}
	result := stub.status
	result.Repository = request.Repository.Name
	result.Sandbox = request.Sandbox
	result.SourceRef = request.SourceRef
	result.SourceCommit = request.SourceCommit
	result.ResultBranch = request.ResultBranch
	result.ResultCommit = request.ResultCommit
	result.FetchedCommit = request.FetchedCommit
	return result, nil
}
func (stub *cloneGitStub) Diff(_ context.Context, request gitx.DiffRequest) (gitx.DiffResult, error) {
	stub.diffCalls = append(stub.diffCalls, request)
	return gitx.DiffResult{Patch: []byte("diff")}, nil
}
func (stub *cloneGitStub) PrepareApply(ctx context.Context, request gitx.ApplyRequest) (gitx.ApplyTransaction, error) {
	stub.applyCalls = append(stub.applyCalls, request)
	if stub.prepareApply != nil {
		return stub.prepareApply(ctx, request)
	}
	result := stub.apply
	result.Repository = request.Repository.Name
	return &cloneApplyTransaction{result: result}, nil
}

type cloneApplyTransaction struct {
	result      gitx.ApplyResult
	commitErr   error
	rollbackErr error
	committed   bool
	rolledBack  bool
}

func (transaction *cloneApplyTransaction) Commit(context.Context) (gitx.ApplyResult, error) {
	transaction.committed = true
	return transaction.result, transaction.commitErr
}

func (transaction *cloneApplyTransaction) Rollback(context.Context) error {
	transaction.rolledBack = true
	return transaction.rollbackErr
}
func (stub *cloneGitStub) RemoveArtifact(path string) error {
	stub.removed = append(stub.removed, path)
	return nil
}

type failingCloneManifestRepository struct {
	state.ManifestRepository
	replaceErr error
	failures   int
}

func (repository *failingCloneManifestRepository) ReplaceManifest(ctx context.Context, manifest state.Manifest, expected uint64) error {
	if repository.failures > 0 {
		repository.failures--
		return repository.replaceErr
	}
	return repository.ManifestRepository.ReplaceManifest(ctx, manifest, expected)
}

type cloneRecordingRuntime struct {
	*lifecycleRuntime
	diffCode             int
	resultCommit         string
	failWorkingDirectory string
	failErr              error
	failCommandContains  string
	exportErr            error
	specs                []runtime.ExecSpec
}

func (fake *cloneRecordingRuntime) Exec(_ context.Context, _ runtime.ResourceSnapshot, spec runtime.ExecSpec, streams runtime.ExecIO) (runtime.Exit, error) {
	fake.specs = append(fake.specs, spec)
	fake.calls = append(fake.calls, "exec:"+strings.Join(spec.Argv, " "))
	if fake.failErr != nil && string(spec.WorkingDir) == fake.failWorkingDirectory {
		return runtime.Exit{}, fake.failErr
	}
	if fake.failErr != nil && fake.failCommandContains != "" && strings.Contains(strings.Join(spec.Argv, " "), fake.failCommandContains) {
		return runtime.Exit{}, fake.failErr
	}
	command := strings.Join(spec.Argv, " ")
	if strings.Contains(command, " export-file ") && fake.exportErr != nil {
		return runtime.Exit{}, fake.exportErr
	}
	if strings.Contains(command, " export-file --kind result ") && streams.Stdout != nil {
		_, _ = io.WriteString(streams.Stdout, "verified bundle bytes")
	}
	if strings.Contains(command, " export-file --kind auth ") && streams.Stdout != nil {
		_, _ = io.WriteString(streams.Stdout, `{"token":"refreshed"}`)
	}
	if strings.Contains(command, "/usr/bin/curl") && fake.execExit.Code != nil {
		return fake.execExit, nil
	}
	code := 0
	if strings.Contains(command, " diff --cached --quiet --exit-code") {
		code = fake.diffCode
	}
	if strings.Contains(command, " rev-parse --verify HEAD^{commit}") && streams.Stdout != nil {
		_, _ = io.WriteString(streams.Stdout, fake.resultCommit+"\n")
	}
	return runtime.Exit{Code: &code}, nil
}

func clonePlanFixture(t *testing.T, sandbox string) (plan.ExecutionPlan, []gitx.SourceArtifact) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	name, err := model.ParseSandboxName(sandbox)
	if err != nil {
		t.Fatal(err)
	}
	execution := plan.ExecutionPlan{
		ContractVersion: plan.ContractVersion,
		Project:         plan.ProjectIdentity{ID: projectID, CanonicalRoot: root},
		Sandbox:         plan.SandboxIdentity{Name: name}, Mode: model.ModeClone,
		Image:          plan.ResolvedImage{Reference: "example@sha256:" + cloneTestDigest, InputDigest: cloneTestDigest},
		Repositories:   []plan.RepositoryPlan{{Name: "workspace", HostPath: root, GuestPath: "/workspace"}},
		Limits:         plan.ResourceLimits{CPUs: 2, MemoryBytes: 1 << 30, MaxConcurrentClones: 2},
		ExecutableHash: cloneTestDigest,
	}
	artifact := gitx.SourceArtifact{
		Repository: gitx.Repository{Name: "workspace", HostPath: root, GuestPath: "/workspace", Identity: cleanupRepositoryIdentity(root, root)},
		SourceRef:  "refs/heads/main", SourceCommit: cloneTestCommit,
		TrackedFingerprint: cloneTestFingerprint, BundlePath: filepath.Join(root, "source.bundle"), BundleDigest: cloneTestDigest,
		BundleRef: "refs/dsx/private/source/00000000000000000000000000000000",
	}
	return execution, []gitx.SourceArtifact{artifact}
}

func TestCloneRunHarnessScopesGlobalAuthenticationCopy(t *testing.T) {
	lifecycle, _, _, fakeRuntime, _ := lifecycleFixture(t)
	fakeRuntime.execOutput = func(spec runtime.ExecSpec) ([]byte, []byte) {
		argv := strings.Join(spec.Argv, " ")
		if strings.Contains(argv, "/bin/cat -- "+harness.BuildAttestationPath) {
			data, err := os.ReadFile("../../images/agent/harnesses.lock.json")
			if err != nil {
				t.Fatal(err)
			}
			return data, nil
		}
		if strings.Contains(argv, " export-file --kind auth ") {
			return []byte(`{"token":"refreshed"}`), nil
		}
		return []byte("shell-ok\n"), nil
	}
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	seedDestinations := make([]string, 0)
	adapter := fakeHarnessAdapter{seedDestinations: &seedDestinations}
	harnessService, err := NewHarnessService(lifecycle, repository, adapter)
	if err != nil {
		t.Fatal(err)
	}
	harnessService.agentImageReference = fixtureAgentImageReference
	service := &CloneService{lifecycle: lifecycle, harness: harnessService}
	execution, _ := clonePlanFixture(t, "named")
	imageDigest, ok := pinnedImageDigest(fixtureAgentImageReference)
	if !ok {
		t.Fatal("fixture agent image reference has no digest")
	}
	execution.Image = plan.ResolvedImage{Reference: fixtureAgentImageReference, InputDigest: imageDigest}
	execution.Agent = "codex"
	execution.Auth = []plan.ResolvedAuthGrant{{Harness: "codex", Profile: "default", Persistence: "global"}}
	result, err := service.runHarness(
		context.Background(),
		runtime.ResourceSnapshot{ImageDigest: "sha256:" + imageDigest},
		execution,
		CloneRunRequest{Agent: "codex", Prompt: "scoped clone"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.AuthPromotion.Digest == "" || result.AuthPromotion.Conflict {
		t.Fatalf("clone harness authentication promotion = %#v", result.AuthPromotion)
	}
	scopedSuffix := filepath.Join("sandboxes", string(execution.Project.ID), "named", "codex", "default")
	foundScopedRun := false
	for _, destination := range seedDestinations {
		if strings.Contains(destination, string(filepath.Separator)+"runs"+string(filepath.Separator)) && strings.HasSuffix(destination, scopedSuffix) {
			foundScopedRun = true
			break
		}
	}
	if !foundScopedRun {
		t.Fatalf("clone global authentication copy was not sandbox-addressable: %#v", seedDestinations)
	}
}

func TestCloneAdmissionLiveCapAndSameNameResume(t *testing.T) {
	live := state.Manifest{Sandbox: liveSandboxName, Mode: model.ModeLive, State: model.StateRunning}
	if _, err := validateCloneAdmission([]state.Manifest{live}, "api", 2); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("live exclusion error = %v", err)
	}
	first := state.Manifest{Sandbox: "api", Mode: model.ModeClone, State: model.StateRunning}
	if _, err := validateCloneAdmission([]state.Manifest{first}, "tests", 1); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("different-name cap error = %v", err)
	}
	if existing, err := validateCloneAdmission([]state.Manifest{first}, "tests", 2); err != nil || existing != nil {
		t.Fatalf("independent name admission = %#v, %v", existing, err)
	}
	existing, err := validateCloneAdmission([]state.Manifest{first}, "api", 1)
	if err != nil || existing == nil || existing.Sandbox != "api" {
		t.Fatalf("same-name ready resume = %#v, %v", existing, err)
	}
	stopped := first
	stopped.State = model.StateStopped
	if existing, err := validateCloneAdmission([]state.Manifest{stopped}, "api", 1); err != nil || existing == nil {
		t.Fatalf("same-name stopped resume = %#v, %v", existing, err)
	}
	if _, err := validateCloneAdmission([]state.Manifest{first, stopped}, "api", 2); model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("ambiguous same-name ownership error = %v", err)
	}

	execution, artifacts := clonePlanFixture(t, "api")
	runA, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000021")
	manifestA, _, err := plannedCloneManifest(execution, runA, time.Now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	execution.Sandbox.Name = "tests"
	runB, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000022")
	manifestB, _, err := plannedCloneManifest(execution, runB, time.Now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if manifestA.Resources[0].Name == manifestB.Resources[0].Name || manifestA.Resources[len(manifestA.Resources)-1].Name == manifestB.Resources[len(manifestB.Resources)-1].Name {
		t.Fatal("independent clone names share owned resources")
	}
}

func TestCloneResumeRejectsPlanSourceDriftAndUnfetchedResult(t *testing.T) {
	execution, artifacts := clonePlanFixture(t, "resume-contract")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000023")
	manifest, _, err := plannedCloneManifest(execution, runID, time.Now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	driftedPlan := execution
	driftedPlan.ExecutableHash = strings.Repeat("c", 64)
	if _, err := validateCloneResumeContract(driftedPlan, manifest); model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("plan drift error = %v", err)
	}
	driftedSource := execution
	driftedSource.Repositories = append([]plan.RepositoryPlan(nil), execution.Repositories...)
	driftedSource.Repositories[0].GuestPath = "/workspace/moved"
	if _, err := validateCloneResumeContract(driftedSource, manifest); model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("source drift error = %v", err)
	}
	manifest.Git[0].ResultCommit = cloneTestResult
	if err := cloneResumeResultGuard(manifest); model.ErrorCodeOf(err) != model.CodeDataLoss {
		t.Fatalf("unfetched prior result error = %v", err)
	}
	manifest.Git[0].FetchedCommit = cloneTestResult
	manifest.Git[0].FetchedHostRef = gitx.RefNamespace + string(manifest.Sandbox)
	if err := cloneResumeResultGuard(manifest); err != nil {
		t.Fatalf("fetched prior result rejected: %v", err)
	}
}

func TestCloneWorkspaceRestartsOnlyExactStoppedOwnedManifest(t *testing.T) {
	lifecycle, root, manifests, base, _ := lifecycleFixture(t)
	execution, artifacts := clonePlanFixture(t, "stopped-resume")
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	execution.Project = plan.ProjectIdentity{ID: projectID, CanonicalRoot: root}
	execution.Repositories[0].HostPath = root
	artifacts[0].Repository.HostPath = root
	artifacts[0].Repository.Identity = cleanupRepositoryIdentity(root, root)
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000024")
	manifest, indexes, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	workspace := &manifest.Resources[indexes.owner]
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	resource := base.create(workspace.Name, runtime.ResourceWorkspace, runtimeLabels(workspace.Labels))
	workspace.RuntimeID = string(resource.ID)
	workspace.Created = true
	if err := lifecycle.replace(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateRunning, "create", ""); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateStopped, "stop", ""); err != nil {
		t.Fatal(err)
	}
	lifecycle.guest = &lifecycleGuest{runtime: base}
	service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{}, git: &cloneGitStub{}, tempRoot: t.TempDir()}
	snapshot, err := service.cloneWorkspace(context.Background(), &manifest, cloneWorkspaceAccess{RestartStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != "running" || manifest.State != model.StateRunning {
		t.Fatalf("stopped resume = runtime %q manifest %q", snapshot.State, manifest.State)
	}
	if !reflect.DeepEqual(base.calls, []string{"create:workspace", "start", "guest:reconcile"}) {
		t.Fatalf("stopped restart calls = %#v", base.calls)
	}

	ambiguous := manifest
	ambiguous.State = model.StateStopped
	if _, err := service.cloneWorkspace(context.Background(), &ambiguous, cloneWorkspaceAccess{RestartStopped: true}); model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("manifest/runtime mismatch error = %v", err)
	}
	runtimeSnapshot := base.resources[resource.ID]
	runtimeSnapshot.Labels = nil
	base.resources[resource.ID] = runtimeSnapshot
	ambiguous.State = model.StateRunning
	if _, err := service.cloneWorkspace(context.Background(), &ambiguous, cloneWorkspaceAccess{}); model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("ownership mismatch error = %v", err)
	}
}

func TestGitFetchRestartsFailedUncapturedStoppedCloneWithoutLaunderingState(t *testing.T) {
	lifecycle, root, manifests, fake, manifest := pendingCleanupCaptureFixture(
		t, "failed-fetch", "01890f5c-7b00-7000-8000-000000000046", "workspace",
	)
	if err := lifecycle.transition(context.Background(), &manifest, model.StateFailed, "capture", "simulated crash"); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Stop(context.Background(), StopRequest{Root: root, Sandbox: "failed-fetch"}); err != nil {
		t.Fatal(err)
	}
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	harnessService, err := NewHarnessService(lifecycle, repository)
	if err != nil {
		t.Fatal(err)
	}
	git := &cloneGitStub{fetch: gitx.FetchResult{
		HostRef: gitx.RefNamespace + string(manifest.Sandbox),
		Commit:  cloneTestResult,
	}}
	clones, err := NewCloneService(CloneDependencies{
		Lifecycle: lifecycle, Harness: harnessService, Git: git, TempRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	fetched, err := clones.GitFetch(context.Background(), GitFetchRequest{Root: root, Sandbox: "failed-fetch"})
	if err != nil || len(fetched.Repositories) != 1 || fetched.Repositories[0].Commit != cloneTestResult {
		t.Fatalf("GitFetch() = %#v, %v", fetched, err)
	}
	stored, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if loadErr != nil || !found || stored.State != model.StateFailed || stored.UncapturedWork || !stored.Git[0].ResultFetched() {
		t.Fatalf("recovered failed clone: found=%t manifest=%#v err=%v", found, stored, loadErr)
	}
	workspace := fake.resources[runtime.ResourceID(manifest.Resources[0].RuntimeID)]
	if workspace.State != "running" {
		t.Fatalf("recovered workspace state = %q", workspace.State)
	}
}

func TestCloneStatusAndApplyRemainAvailableWhenStopped(t *testing.T) {
	lifecycle, root, manifests, _, _ := lifecycleFixture(t)
	execution, artifacts := clonePlanFixture(t, "stopped-git")
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	execution.Project = plan.ProjectIdentity{ID: projectID, CanonicalRoot: root}
	execution.Repositories[0].HostPath = root
	artifacts[0].Repository.HostPath = root
	artifacts[0].Repository.Identity = cleanupRepositoryIdentity(root, root)
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000025")
	manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	manifest.Git[0].ResultCommit = cloneTestResult
	manifest.Git[0].ResultBundleDigest = cloneTestDigest
	if err := lifecycle.replace(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateRunning, "create", ""); err != nil {
		t.Fatal(err)
	}
	manifest.Git[0].FetchedCommit = cloneTestResult
	manifest.Git[0].FetchedHostRef = gitx.RefNamespace + string(manifest.Sandbox)
	if err := lifecycle.replace(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateStopped, "stop", ""); err != nil {
		t.Fatal(err)
	}
	git := &cloneGitStub{
		status: gitx.Status{HostCommit: cloneTestCommit, HostTrackedClean: true, HostTrackedFingerprint: cloneTestFingerprint},
		apply:  gitx.ApplyResult{AppliedCommit: cloneTestResult},
	}
	service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{}, git: git, tempRoot: t.TempDir()}
	status, err := service.GitStatus(context.Background(), GitStatusRequest{Root: root, Sandbox: string(manifest.Sandbox)})
	if err != nil || len(status.Repositories) != 1 {
		t.Fatalf("stopped status = %#v, %v", status, err)
	}
	applied, err := service.GitApply(context.Background(), GitApplyRequest{Root: root, Sandbox: string(manifest.Sandbox)})
	if err != nil || len(applied.Repositories) != 1 {
		t.Fatalf("stopped apply = %#v, %v", applied, err)
	}
}

func TestCloneWorkspaceUsesOwnedVolumeAndNoHostRepositoryMount(t *testing.T) {
	execution, artifacts := clonePlanFixture(t, "isolated")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000023")
	manifest, indexes, err := plannedCloneManifest(execution, runID, time.Now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if indexes.workspace < 0 || manifest.Resources[indexes.workspace].Kind != string(runtime.ResourceVolume) || manifest.Resources[indexes.workspace].Persistent {
		t.Fatalf("workspace volume record = %#v", manifest.Resources[indexes.workspace])
	}
	volumes := map[string]string{cloneWorkspaceVolume: manifest.Resources[indexes.workspace].Name}
	spec, err := workspaceSpecForClone(execution, runtime.Image{}, manifest.Resources[len(manifest.Resources)-1], manifest.Resources[0].Name, volumes, nil, "1000:1000", runtime.HostPath(filepath.Join(t.TempDir(), "dsx-guest")), "")
	if err != nil {
		t.Fatal(err)
	}
	var workspaceMount bool
	for _, mount := range spec.Mounts {
		if mount.Source == execution.Repositories[0].HostPath {
			t.Fatalf("host repository was mounted: %#v", mount)
		}
		if mount.Target == "/workspace" && mount.Type == "volume" && mount.Source == volumes[cloneWorkspaceVolume] &&
			mount.Authority == runtime.MountAuthorityInternal {
			workspaceMount = true
		}
	}
	if !workspaceMount {
		t.Fatalf("owned workspace volume absent: %#v", spec.Mounts)
	}
	if spec.User != "0:0" || !reflect.DeepEqual(spec.Entrypoint, []string{
		DefaultGuestHelperPath, "serve",
		"--socket", DefaultGuestSocketPath,
		"--child-uid", "1000",
		"--child-gid", "1000",
		"--initialize-workspace", "/workspace",
	}) {
		t.Fatalf("owned-volume supervisor authority = user %q entrypoint %#v", spec.User, spec.Entrypoint)
	}
}

func TestCloneBootstrapSequencingNoHardlinksAndCleanup(t *testing.T) {
	lifecycle, _, _, base, _ := lifecycleFixture(t)
	fake := &cloneRecordingRuntime{lifecycleRuntime: base}
	lifecycle.runtime = fake
	git := &cloneGitStub{}
	service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{lifecycle: lifecycle}, git: git, tempRoot: t.TempDir()}
	execution, artifacts := clonePlanFixture(t, "sequence")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000024")
	manifest, _, err := plannedCloneManifest(execution, runID, time.Now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := runtime.ResourceSnapshot{Resource: runtime.Resource{ID: "workspace", Kind: runtime.ResourceWorkspace}}
	if err := service.bootstrap(context.Background(), snapshot, &manifest, artifacts); err != nil {
		t.Fatal(err)
	}
	commands := make([]string, 0, len(fake.specs))
	for _, spec := range fake.specs {
		commands = append(commands, strings.Join(spec.Argv, " "))
	}
	joined := strings.Join(commands, "\n")
	fetchNeedle := "fetch --no-tags --no-write-fetch-head --"
	if !strings.Contains(joined, fetchNeedle) || strings.Contains(joined, "clone --shared") {
		t.Fatalf("independent bundle import command absent:\n%s", joined)
	}
	verifyAt, initAt, fetchAt, checkoutAt, cleanupAt := strings.Index(joined, "bundle verify"), strings.Index(joined, "init --quiet /workspace"), strings.Index(joined, fetchNeedle), strings.Index(joined, "checkout -B dsx/sequence"), strings.LastIndex(joined, "/bin/rm -rf -- /tmp/dsx-source-")
	if !(verifyAt >= 0 && verifyAt < initAt && initAt < fetchAt && fetchAt < checkoutAt && checkoutAt < cleanupAt) {
		t.Fatalf("bootstrap sequence is unsafe:\n%s", joined)
	}
}

func TestCloneCompositePrepareRollbackRemovesOnlyPreparedBundles(t *testing.T) {
	execution, artifacts := clonePlanFixture(t, "composite")
	execution.Repositories = append(execution.Repositories, plan.RepositoryPlan{Name: "web", HostPath: execution.Project.CanonicalRoot, GuestPath: "/workspace/web"})
	preparedPath := filepath.Join(t.TempDir(), "prepared.bundle")
	calls := 0
	git := &cloneGitStub{prepare: func(_ context.Context, request gitx.SourceRequest) (gitx.SourceArtifact, error) {
		calls++
		if calls == 2 {
			return gitx.SourceArtifact{}, errors.New("second member failed")
		}
		artifact := artifacts[0]
		artifact.Repository = request.Repository
		artifact.BundlePath = preparedPath
		return artifact, nil
	}}
	service := &CloneService{git: git, tempRoot: t.TempDir()}
	if _, _, err := service.prepareSources(context.Background(), execution, "composite"); err == nil {
		t.Fatal("composite preparation unexpectedly succeeded")
	}
	if !reflect.DeepEqual(git.removed, []string{preparedPath}) {
		t.Fatalf("rollback removed = %#v", git.removed)
	}
}

func TestCloneCaptureResultIdentityTempCleanupAndUnfetchedGuard(t *testing.T) {
	lifecycle, _, manifests, base, _ := lifecycleFixture(t)
	fake := &cloneRecordingRuntime{lifecycleRuntime: base, diffCode: 1, resultCommit: cloneTestResult}
	lifecycle.runtime = fake
	git := &cloneGitStub{status: gitx.Status{HostCommit: cloneTestCommit, HostTrackedClean: true, HostTrackedFingerprint: cloneTestFingerprint}}
	tempRoot := t.TempDir()
	service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{lifecycle: lifecycle}, git: git, tempRoot: tempRoot}
	execution, artifacts := clonePlanFixture(t, "result")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000025")
	manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.markCapturePending(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	statuses, err := service.captureResults(context.Background(), runtime.ResourceSnapshot{}, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.finalizeCaptured(context.Background(), &manifest, nil); err != nil {
		t.Fatal(err)
	}
	if manifest.UncapturedWork || manifest.State != model.StateRunning {
		t.Fatalf("captured result uncertainty was not cleared: %#v", manifest)
	}
	if len(statuses) != 1 || manifest.Git[0].ResultCommit != cloneTestResult || manifest.Git[0].ResultBundleDigest == "" {
		t.Fatalf("captured result = %#v, statuses=%#v", manifest.Git[0], statuses)
	}
	if _, guardErr := cloneCleanupGuard(manifest); model.ErrorCodeOf(guardErr) != model.CodeDataLoss {
		t.Fatalf("unfetched guard error = %v", guardErr)
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("result temp leaked: %#v", entries)
	}
	var sawAdd, sawCommit, sawIdentity, sawBoundedProduction, sawBoundedExport, sawExportCleanup bool
	for _, spec := range fake.specs {
		command := strings.Join(spec.Argv, " ")
		sawAdd = sawAdd || strings.Contains(command, " add -A")
		sawCommit = sawCommit || strings.Contains(command, " commit --no-gpg-sign")
		sawBoundedProduction = sawBoundedProduction || strings.Contains(command, " produce-file --max-bytes 536870912 --path /tmp/dsx-run/01890f5c-7b00-7000-8000-000000000025/tmp/result-0.bundle --cwd /workspace -- /usr/bin/git ") &&
			strings.Contains(command, " bundle create --version=2 - refs/heads/dsx/result")
		sawBoundedExport = sawBoundedExport || strings.Contains(command, " export-file --kind result --max-bytes 536870913 --path /tmp/dsx-run/01890f5c-7b00-7000-8000-000000000025/tmp/result-0.bundle")
		sawExportCleanup = sawExportCleanup || strings.Contains(command, " remove-export-file --path /tmp/dsx-run/01890f5c-7b00-7000-8000-000000000025/tmp/result-0.bundle")
		environment := strings.Join(spec.Env, "\n")
		sawIdentity = sawIdentity || strings.Contains(environment, "GIT_AUTHOR_NAME="+cloneGitIdentityName) && strings.Contains(environment, "GIT_COMMITTER_DATE="+cloneGitTimestamp)
	}
	if !sawAdd || !sawCommit || !sawIdentity || !sawBoundedProduction || !sawBoundedExport || !sawExportCleanup {
		t.Fatalf("result sequence identity/production/export missing: add=%t commit=%t identity=%t production=%t export=%t cleanup=%t", sawAdd, sawCommit, sawIdentity, sawBoundedProduction, sawBoundedExport, sawExportCleanup)
	}
}

func TestCopyResultProducerFailurePreventsVerificationAndTransfer(t *testing.T) {
	lifecycle, _, _, base, _ := lifecycleFixture(t)
	injected := errors.New("producer exceeded cap")
	fake := &cloneRecordingRuntime{lifecycleRuntime: base, failErr: injected, failCommandContains: "produce-file"}
	lifecycle.runtime = fake
	service := &CloneService{
		lifecycle: lifecycle,
		harness:   &HarnessService{lifecycle: lifecycle},
		git:       &cloneGitStub{},
		tempRoot:  t.TempDir(),
	}
	execution, artifacts := clonePlanFixture(t, "producer-cap")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000035")
	manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	record := manifest.Git[0]
	record.ResultCommit = cloneTestResult
	if _, _, err := service.copyResult(context.Background(), runtime.ResourceSnapshot{}, record, runID, 0); !errors.Is(err, injected) {
		t.Fatalf("copyResult() error = %v, want producer failure", err)
	}
	sawProducer, sawLaterBundleOperation, sawTransfer := false, false, false
	for _, spec := range fake.specs {
		joined := strings.Join(spec.Argv, "\x00")
		if strings.Contains(joined, "\x00produce-file\x00") {
			sawProducer = true
			if strings.Contains(joined, "\x00/bin/sh\x00") ||
				!strings.Contains(joined, "\x00--max-bytes\x00536870912\x00") ||
				!strings.Contains(joined, "\x00bundle\x00create\x00--version=2\x00-\x00refs/heads/dsx/producer-cap") {
				t.Fatalf("result producer command is not structured and bounded: %#v", spec.Argv)
			}
			continue
		}
		sawLaterBundleOperation = sawLaterBundleOperation || strings.Contains(joined, "\x00bundle\x00verify\x00")
		sawTransfer = sawTransfer || strings.Contains(joined, "\x00export-file\x00")
	}
	if !sawProducer || sawLaterBundleOperation || sawTransfer {
		t.Fatalf("producer failure sequence: producer=%t later-bundle=%t transfer=%t specs=%#v", sawProducer, sawLaterBundleOperation, sawTransfer, fake.specs)
	}
}

func TestCloneNoopResultDoesNotTriggerUnfetchedGuard(t *testing.T) {
	lifecycle, _, manifests, base, _ := lifecycleFixture(t)
	fake := &cloneRecordingRuntime{lifecycleRuntime: base, diffCode: 0, resultCommit: cloneTestCommit}
	lifecycle.runtime = fake
	git := &cloneGitStub{status: gitx.Status{HostCommit: cloneTestCommit, HostTrackedClean: true, HostTrackedFingerprint: cloneTestFingerprint}}
	service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{lifecycle: lifecycle}, git: git, tempRoot: t.TempDir()}
	execution, artifacts := clonePlanFixture(t, "noop")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000026")
	manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.markCapturePending(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := service.captureResults(context.Background(), runtime.ResourceSnapshot{}, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := service.finalizeCaptured(context.Background(), &manifest, nil); err != nil {
		t.Fatal(err)
	}
	if manifest.UncapturedWork {
		t.Fatal("fully captured no-op retained uncertainty")
	}
	if manifest.Git[0].HasResultWork() {
		t.Fatalf("no-op run recorded result work: %#v", manifest.Git[0])
	}
	if preserved, err := cloneCleanupGuard(manifest); err != nil || len(preserved) != 0 {
		t.Fatalf("no-op cleanup guard = %#v, %v", preserved, err)
	}
}

func TestCloneHarnessFailureRetainsUncapturedWorkAfterRepositoryFinalization(t *testing.T) {
	lifecycle, _, manifests, base, _ := lifecycleFixture(t)
	fake := &cloneRecordingRuntime{lifecycleRuntime: base, diffCode: 0, resultCommit: cloneTestCommit}
	lifecycle.runtime = fake
	service := &CloneService{
		lifecycle: lifecycle,
		harness:   &HarnessService{},
		git:       &cloneGitStub{status: gitx.Status{HostCommit: cloneTestCommit, HostTrackedClean: true, HostTrackedFingerprint: cloneTestFingerprint}},
		tempRoot:  t.TempDir(),
	}
	execution, artifacts := clonePlanFixture(t, "hfail")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000032")
	manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.markCapturePending(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := service.captureResults(context.Background(), runtime.ResourceSnapshot{}, &manifest); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected harness failure")
	if err := service.finalizeCaptured(context.Background(), &manifest, injected); err != nil {
		t.Fatal(err)
	}
	if manifest.State != model.StateFailed || !manifest.UncapturedWork || manifest.Git[0].HasResultWork() {
		t.Fatalf("harness failure finalization = %#v", manifest)
	}
}

func TestCloneCommittedResultIsCapturedWithoutSyntheticCommit(t *testing.T) {
	lifecycle, _, manifests, base, _ := lifecycleFixture(t)
	fake := &cloneRecordingRuntime{lifecycleRuntime: base, diffCode: 0, resultCommit: cloneTestResult}
	lifecycle.runtime = fake
	git := &cloneGitStub{status: gitx.Status{HostCommit: cloneTestCommit, HostTrackedClean: true, HostTrackedFingerprint: cloneTestFingerprint}}
	service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{lifecycle: lifecycle}, git: git, tempRoot: t.TempDir()}
	execution, artifacts := clonePlanFixture(t, "committed")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000029")
	manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.markCapturePending(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := service.captureResults(context.Background(), runtime.ResourceSnapshot{}, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Git[0].ResultCommit != cloneTestResult || !manifest.Git[0].HasResultWork() {
		t.Fatalf("committed result was not captured: %#v", manifest.Git[0])
	}
	resultRef := "refs/heads/" + manifest.Git[0].ResultBranch
	updateFound, bundleFound := false, false
	for _, spec := range fake.specs {
		joined := strings.Join(spec.Argv, "\x00")
		if strings.Contains(joined, "\x00commit\x00--no-gpg-sign\x00") {
			t.Fatalf("committed result received a synthetic commit: %#v", spec.Argv)
		}
		if strings.Contains(joined, "\x00update-ref\x00") {
			updateFound = true
			if !strings.Contains(joined, "\x00update-ref\x00"+resultRef+"\x00") {
				t.Fatalf("result update used ambiguous ref: %#v", spec.Argv)
			}
		}
		if strings.Contains(joined, "\x00bundle\x00create\x00") {
			bundleFound = true
			if !strings.HasSuffix(joined, "\x00"+resultRef) {
				t.Fatalf("result bundle used ambiguous ref: %#v", spec.Argv)
			}
		}
	}
	if !updateFound || !bundleFound {
		t.Fatalf("full result ref operations missing: update=%t bundle=%t specs=%#v", updateFound, bundleFound, fake.specs)
	}
}

func TestCloneCapturePersistsCompletedMemberBeforeLaterFailure(t *testing.T) {
	lifecycle, _, manifests, base, _ := lifecycleFixture(t)
	injected := errors.New("second repository capture failed")
	fake := &cloneRecordingRuntime{
		lifecycleRuntime:     base,
		diffCode:             1,
		resultCommit:         cloneTestResult,
		failWorkingDirectory: "/workspace/web",
		failErr:              injected,
	}
	lifecycle.runtime = fake
	service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{lifecycle: lifecycle}, git: &cloneGitStub{}, tempRoot: t.TempDir()}
	execution, artifacts := clonePlanFixture(t, "partial")
	secondHost := filepath.Join(execution.Project.CanonicalRoot, "web")
	execution.Repositories = append(execution.Repositories, plan.RepositoryPlan{Name: "web", HostPath: secondHost, GuestPath: "/workspace/web"})
	second := artifacts[0]
	second.Repository = gitx.Repository{Name: "web", HostPath: secondHost, GuestPath: "/workspace/web", Identity: cleanupRepositoryIdentity(execution.Project.CanonicalRoot, secondHost)}
	artifacts = append(artifacts, second)
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000030")
	manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.markCapturePending(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := service.captureResults(context.Background(), runtime.ResourceSnapshot{}, &manifest); !errors.Is(err, injected) {
		t.Fatalf("capture error = %v, want injected failure", err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateFailed, "capture", injected.Error()); err != nil {
		t.Fatal(err)
	}
	stored, found, err := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if err != nil || !found {
		t.Fatalf("load manifest = %v, found=%t", err, found)
	}
	if !stored.Git[0].HasResultWork() || stored.Git[1].HasResultWork() {
		t.Fatalf("partial durable capture = %#v", stored.Git)
	}
	if stored.State != model.StateFailed || !stored.UncapturedWork {
		t.Fatalf("partial capture uncertainty = state %q uncaptured=%t", stored.State, stored.UncapturedWork)
	}
}

func TestCloneCaptureBoundaryFailuresRetainDurableUncertainty(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		export     bool
		hostVerify bool
		status     bool
		manifest   bool
	}{
		{name: "stage", command: " add -A"},
		{name: "inspect staged", command: " diff --cached"},
		{name: "commit", command: " commit --no-gpg-sign"},
		{name: "resolve head", command: " rev-parse --verify"},
		{name: "source ancestry", command: " merge-base --is-ancestor"},
		{name: "pin result branch", command: " update-ref"},
		{name: "create guest bundle", command: " bundle create"},
		{name: "verify guest bundle", command: " bundle verify"},
		{name: "export bundle", export: true},
		{name: "verify host bundle", hostVerify: true},
		{name: "persist member manifest", manifest: true},
		{name: "collect status", status: true},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lifecycle, _, manifests, base, _ := lifecycleFixture(t)
			injected := errors.New("injected " + test.name + " failure")
			fake := &cloneRecordingRuntime{lifecycleRuntime: base, diffCode: 1, resultCommit: cloneTestResult}
			if test.command != "" {
				fake.failErr = injected
				fake.failCommandContains = test.command
			}
			if test.export {
				fake.exportErr = injected
			}
			lifecycle.runtime = fake
			git := &cloneGitStub{}
			if test.hostVerify {
				git.verifyErr = injected
			}
			if test.status {
				git.statusErr = injected
			}
			service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{lifecycle: lifecycle}, git: git, tempRoot: t.TempDir()}
			execution, artifacts := clonePlanFixture(t, fmt.Sprintf("b%02d", index))
			runID, err := model.ParseRunID(fmt.Sprintf("01890f5c-7b00-7000-8000-%012d", index+40))
			if err != nil {
				t.Fatal(err)
			}
			manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
			if err != nil {
				t.Fatal(err)
			}
			if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
				t.Fatal(err)
			}
			if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
				t.Fatal(err)
			}
			if err := service.markCapturePending(context.Background(), &manifest); err != nil {
				t.Fatal(err)
			}
			if test.manifest {
				lifecycle.manifests = &failingCloneManifestRepository{ManifestRepository: manifests, replaceErr: injected, failures: 1}
			}
			if _, err := service.captureResults(context.Background(), runtime.ResourceSnapshot{}, &manifest); !errors.Is(err, injected) {
				t.Fatalf("capture error = %v, want injected failure", err)
			}
			if err := lifecycle.transition(context.Background(), &manifest, model.StateFailed, "capture", injected.Error()); err != nil {
				t.Fatal(err)
			}
			stored, found, err := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
			if err != nil || !found {
				t.Fatalf("load failed capture = %v, found=%t", err, found)
			}
			if stored.State != model.StateFailed || !stored.UncapturedWork {
				t.Fatalf("capture boundary lost uncertainty: state=%q uncaptured=%t", stored.State, stored.UncapturedWork)
			}
			if test.status || test.manifest {
				if stored.Git[0].ResultCommit != cloneTestResult {
					t.Fatalf("finalized member was not durable at %s boundary: %#v", test.name, stored.Git[0])
				}
			}
		})
	}
}

func TestCloneFinalManifestCASFailureRetainsDurableUncertaintyAndResults(t *testing.T) {
	lifecycle, root, manifests, base, _ := lifecycleFixture(t)
	fake := &cloneRecordingRuntime{lifecycleRuntime: base, diffCode: 1, resultCommit: cloneTestResult}
	lifecycle.runtime = fake
	git := &cloneGitStub{
		status: gitx.Status{HostCommit: cloneTestCommit, HostTrackedClean: true, HostTrackedFingerprint: cloneTestFingerprint},
		fetch:  gitx.FetchResult{HostRef: gitx.RefNamespace + "cas-failure", Commit: cloneTestResult},
	}
	service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{lifecycle: lifecycle}, git: git, tempRoot: t.TempDir()}
	execution, artifacts := clonePlanFixture(t, "cas-failure")
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	execution.Project = plan.ProjectIdentity{ID: projectID, CanonicalRoot: root}
	execution.Repositories[0].HostPath = root
	artifacts[0].Repository.HostPath = root
	artifacts[0].Repository.Identity = cleanupRepositoryIdentity(root, root)
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000031")
	manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	workspace := &manifest.Resources[len(manifest.Resources)-1]
	workspace.Created = true
	workspace.RuntimeID = workspace.ExpectedID
	base.resources[runtime.ResourceID(workspace.ExpectedID)] = runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(workspace.ExpectedID), Name: workspace.Name, Kind: runtime.ResourceWorkspace},
		State:    "running",
		Labels:   runtimeLabels(workspace.Labels),
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.markCapturePending(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := service.captureResults(context.Background(), runtime.ResourceSnapshot{}, &manifest); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected final manifest CAS failure")
	lifecycle.manifests = &failingCloneManifestRepository{ManifestRepository: manifests, replaceErr: injected, failures: 1}
	if err := service.finalizeCaptured(context.Background(), &manifest, nil); !errors.Is(err, injected) {
		t.Fatalf("finalize error = %v, want injected CAS failure", err)
	}
	if manifest.State != model.StateCreating || !manifest.UncapturedWork {
		t.Fatalf("in-memory manifest lost durable uncertainty: state=%q uncaptured=%t", manifest.State, manifest.UncapturedWork)
	}
	stored, found, err := manifests.LoadManifest(context.Background(), projectID, manifest.Sandbox, runID)
	if err != nil || !found {
		t.Fatalf("load durable manifest = %v, found=%t", err, found)
	}
	if stored.State != model.StateCreating || !stored.UncapturedWork || stored.Git[0].ResultCommit != cloneTestResult {
		t.Fatalf("durable CAS residue = %#v", stored)
	}
	status, err := service.GitStatus(context.Background(), GitStatusRequest{Root: root, Sandbox: string(manifest.Sandbox)})
	if err != nil || len(status.Repositories) != 1 || status.Repositories[0].ResultCommit != cloneTestResult {
		t.Fatalf("status on uncertain capture = %#v, %v", status, err)
	}
	fetched, err := service.GitFetch(context.Background(), GitFetchRequest{Root: root, Sandbox: string(manifest.Sandbox)})
	if err != nil || len(fetched.Repositories) != 1 || fetched.Repositories[0].Commit != cloneTestResult {
		t.Fatalf("fetch on uncertain capture = %#v, %v", fetched, err)
	}
	stored, found, err = manifests.LoadManifest(context.Background(), projectID, manifest.Sandbox, runID)
	if err != nil || !found {
		t.Fatalf("reload fetched uncertain manifest = %v, found=%t", err, found)
	}
	if stored.UncapturedWork || stored.State != model.StateRunning || !stored.Git[0].ResultFetched() {
		t.Fatalf("fetch did not durably recover capture uncertainty: %#v", stored)
	}
	if _, guardErr := cloneCleanupGuard(stored); guardErr != nil {
		t.Fatalf("fetched recovered result remained cleanup-blocked: %v", guardErr)
	}
	if _, exists := base.resources[runtime.ResourceID(workspace.ExpectedID)]; !exists {
		t.Fatal("capture finalization failure removed the clone workspace")
	}
}

func TestCloneGitFetchApplyUpdatesManifestAndReleasesGuard(t *testing.T) {
	lifecycle, root, manifests, base, _ := lifecycleFixture(t)
	fake := &cloneRecordingRuntime{lifecycleRuntime: base, resultCommit: cloneTestResult}
	lifecycle.runtime = fake
	git := &cloneGitStub{
		status: gitx.Status{
			HostCommit: cloneTestCommit, HostTrackedClean: true,
			HostTrackedFingerprint: cloneTestFingerprint, Fetched: true, FetchedCommit: cloneTestResult,
		},
		fetch: gitx.FetchResult{HostRef: gitx.RefNamespace + "transfer", Commit: cloneTestResult},
		apply: gitx.ApplyResult{AppliedCommit: cloneTestResult, Paths: []string{"tracked.txt"}},
	}
	tempRoot := t.TempDir()
	service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{lifecycle: lifecycle}, git: git, tempRoot: tempRoot}
	execution, artifacts := clonePlanFixture(t, "transfer")
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	execution.Project = plan.ProjectIdentity{ID: projectID, CanonicalRoot: root}
	execution.Repositories[0].HostPath = root
	artifacts[0].Repository.HostPath = root
	artifacts[0].Repository.Identity = cleanupRepositoryIdentity(root, root)
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000027")
	manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	manifest.Git[0].ResultCommit = cloneTestResult
	manifest.Git[0].ResultBundleDigest = cloneTestDigest
	workspace := &manifest.Resources[len(manifest.Resources)-1]
	workspace.Created = true
	workspace.RuntimeID = workspace.ExpectedID
	base.resources[runtime.ResourceID(workspace.ExpectedID)] = runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(workspace.ExpectedID), Name: workspace.Name, Kind: runtime.ResourceWorkspace},
		State:    "running", Labels: runtimeLabels(workspace.Labels),
	}
	if err := lifecycle.replace(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateRunning, "create", ""); err != nil {
		t.Fatal(err)
	}
	for operation, invoke := range map[string]func() error{
		"status": func() error {
			_, err := service.GitStatus(context.Background(), GitStatusRequest{Root: root, Sandbox: "transfer", Repository: "missing"})
			return err
		},
		"diff": func() error {
			_, err := service.GitDiff(context.Background(), GitDiffRequest{Root: root, Sandbox: "transfer", Repository: "missing"})
			return err
		},
		"fetch": func() error {
			_, err := service.GitFetch(context.Background(), GitFetchRequest{Root: root, Sandbox: "transfer", Repository: "missing"})
			return err
		},
		"apply": func() error {
			_, err := service.GitApply(context.Background(), GitApplyRequest{Root: root, Sandbox: "transfer", Repository: "missing"})
			return err
		},
	} {
		if err := invoke(); model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatalf("%s invalid selector error = %v", operation, err)
		}
	}
	if len(git.statusCalls) != 0 || len(git.diffCalls) != 0 || len(git.fetchCalls) != 0 || len(git.applyCalls) != 0 {
		t.Fatalf("invalid selector reached Git service: status=%d diff=%d fetch=%d apply=%d", len(git.statusCalls), len(git.diffCalls), len(git.fetchCalls), len(git.applyCalls))
	}
	freshStatus, err := service.GitStatus(context.Background(), GitStatusRequest{Root: root, Sandbox: "transfer", Repository: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if len(freshStatus.Repositories) != 1 || freshStatus.Repositories[0].Fetched || freshStatus.Repositories[0].FetchedCommit != "" {
		t.Fatalf("fresh status = %#v", freshStatus)
	}
	freshDiff, err := service.GitDiff(context.Background(), GitDiffRequest{Root: root, Sandbox: "transfer", Repository: "workspace", MaxBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if len(freshDiff.Diffs) != 1 || string(freshDiff.Diffs[0].Patch) != "diff" || len(git.diffCalls) != 1 || git.diffCalls[0].Bundle == nil {
		t.Fatalf("fresh diff = %#v, calls = %#v", freshDiff, git.diffCalls)
	}
	if git.diffCalls[0].Bundle.Ref != "refs/heads/dsx/transfer" || git.diffCalls[0].HeadCommit != cloneTestResult {
		t.Fatalf("fresh diff bundle request = %#v", git.diffCalls[0])
	}
	if _, err := os.Lstat(git.diffCalls[0].Bundle.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh diff bundle temp remains: %v", err)
	}
	unfetched, err := manifests.ListProjectManifests(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfetched) != 1 || unfetched[0].Git[0].ResultFetched() || unfetched[0].Git[0].FetchedHostRef != "" {
		t.Fatalf("diff mutated fetched manifest state: %#v", unfetched)
	}
	fetched, err := service.GitFetch(context.Background(), GitFetchRequest{Root: root, Sandbox: "transfer", Repository: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched.Repositories) != 1 || fetched.Repositories[0].Commit != cloneTestResult {
		t.Fatalf("fetch result = %#v", fetched)
	}
	if len(git.fetchCalls) != 1 || git.fetchCalls[0].Repository.Name != "workspace" || git.fetchCalls[0].ExpectedCommit != cloneTestResult {
		t.Fatalf("fetch selector request = %#v", git.fetchCalls)
	}
	stored, err := manifests.ListProjectManifests(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || !stored[0].Git[0].ResultFetched() || stored[0].Git[0].FetchedHostRef != gitx.RefNamespace+"transfer" {
		t.Fatalf("fetched manifest = %#v", stored)
	}
	if preserved, guardErr := cloneCleanupGuard(stored[0]); guardErr != nil || len(preserved) != 0 {
		t.Fatalf("fetched cleanup guard = %#v, %v", preserved, guardErr)
	}
	applied, err := service.GitApply(context.Background(), GitApplyRequest{Root: root, Sandbox: "transfer", Repository: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Repositories) != 1 || applied.Repositories[0].AppliedCommit != cloneTestResult {
		t.Fatalf("apply result = %#v", applied)
	}
	if len(git.applyCalls) != 1 || git.applyCalls[0].Repository.Name != "workspace" || git.applyCalls[0].ExpectedCommit != cloneTestResult {
		t.Fatalf("apply selector request = %#v", git.applyCalls)
	}
	if err := lifecycle.transition(context.Background(), &stored[0], model.StateStopped, "stop", ""); err != nil {
		t.Fatal(err)
	}
	stoppedDiff, err := service.GitDiff(context.Background(), GitDiffRequest{Root: root, Sandbox: "transfer", Repository: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stoppedDiff.Diffs) != 1 || len(git.diffCalls) != 2 || git.diffCalls[1].Bundle != nil {
		t.Fatalf("stopped fetched diff = %#v, calls = %#v", stoppedDiff, git.diffCalls)
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("fetch result temp leaked: %#v", entries)
	}
}

func TestCloneRealTempGitServiceFetchDiffApplyCorpus(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	hostRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(hostRoot, "host")
	runGit(t, gitPath, "", nil, "init", "-b", "main", host)
	runGit(t, gitPath, host, nil, "config", "user.name", "Fixture")
	runGit(t, gitPath, host, nil, "config", "user.email", "fixture@example.invalid")
	writeTestFile(t, filepath.Join(host, "tracked.txt"), []byte("before\n"), 0o644)
	writeTestFile(t, filepath.Join(host, "deleted.txt"), []byte("delete me\n"), 0o644)
	writeTestFile(t, filepath.Join(host, "rename-old.txt"), []byte("rename me\n"), 0o644)
	runGit(t, gitPath, host, nil, "add", "-A")
	runGit(t, gitPath, host, nil, "commit", "-m", "source")

	hostGit, err := gitx.NewService(gitx.OSRunner{}, gitPath)
	if err != nil {
		t.Fatal(err)
	}
	tempRoot := t.TempDir()
	source, err := hostGit.PrepareSource(context.Background(), gitx.SourceRequest{Repository: gitx.Repository{Name: "workspace", HostPath: host, GuestPath: "/workspace"}, ApprovedRoot: host, Sandbox: "smoke", TempRoot: tempRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer hostGit.RemoveArtifact(source.BundlePath)
	guest := filepath.Join(t.TempDir(), "guest")
	runGit(t, gitPath, "", nil, "init", "--quiet", guest)
	runGit(t, gitPath, guest, nil, "fetch", "--no-tags", "--no-write-fetch-head", "--", source.BundlePath, source.BundleRef)
	runGit(t, gitPath, guest, nil, "checkout", "-B", "dsx/smoke", source.SourceCommit)
	writeTestFile(t, filepath.Join(guest, "tracked.txt"), []byte("after\n"), 0o644)
	writeTestFile(t, filepath.Join(guest, "new.txt"), []byte("new\n"), 0o644)
	writeTestFile(t, filepath.Join(guest, "binary.bin"), []byte{0, 1, 2, 0xff, 0, 3}, 0o644)
	if err := os.Remove(filepath.Join(guest, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(guest, "rename-old.txt"), filepath.Join(guest, "rename-new.txt")); err != nil {
		t.Fatal(err)
	}
	runGit(t, gitPath, guest, nil, "add", "-A")
	runGit(t, gitPath, guest, cloneGitEnvironment(), "commit", "--no-gpg-sign", "-m", "DSX result for smoke")
	resultCommit := strings.TrimSpace(runGit(t, gitPath, guest, nil, "rev-parse", "HEAD"))
	identity := runGit(t, gitPath, guest, nil, "show", "-s", "--format=%an%x00%ae%x00%cn%x00%ce%x00%aI", "HEAD")
	if !strings.Contains(identity, cloneGitIdentityName+"\x00"+cloneGitIdentityEmail+"\x00"+cloneGitIdentityName+"\x00"+cloneGitIdentityEmail+"\x002000-01-01T00:00:00Z") {
		t.Fatalf("generated identity = %q", identity)
	}
	bundle := filepath.Join(t.TempDir(), "result.bundle")
	runGit(t, gitPath, guest, nil, "bundle", "create", bundle, "refs/heads/dsx/smoke")
	if err := os.Chmod(bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(mustReadFile(t, bundle))
	fetched, err := hostGit.FetchResult(context.Background(), gitx.FetchRequest{Repository: source.Repository, Sandbox: "smoke", BundlePath: bundle, Digest: hex.EncodeToString(digest[:]), ExpectedCommit: resultCommit})
	if err != nil {
		t.Fatal(err)
	}
	if fetched.HostRef != gitx.RefNamespace+"smoke" || fetched.Commit != resultCommit {
		t.Fatalf("fetched = %#v, result=%s", fetched, resultCommit)
	}
	diff, err := hostGit.Diff(context.Background(), gitx.DiffRequest{Repository: source.Repository, BaseCommit: source.SourceCommit, HeadCommit: resultCommit})
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"binary.bin", "new.txt", "deleted.txt", "rename-new.txt"} {
		if !bytes.Contains(diff.Patch, []byte(marker)) {
			t.Fatalf("diff omitted %s:\n%s", marker, diff.Patch)
		}
	}
	transaction, err := hostGit.PrepareApply(context.Background(), gitx.ApplyRequest{
		Repository: source.Repository, SourceCommit: source.SourceCommit, TrackedFingerprint: source.TrackedFingerprint,
		FetchedRef: fetched.HostRef, ExpectedCommit: resultCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := transaction.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Paths) < 5 || !reflect.DeepEqual(mustReadFile(t, filepath.Join(host, "binary.bin")), []byte{0, 1, 2, 0xff, 0, 3}) {
		t.Fatalf("apply result = %#v", applied)
	}
	if _, err := os.Stat(filepath.Join(host, "deleted.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted path survived apply: %v", err)
	}
	if string(mustReadFile(t, filepath.Join(host, "rename-new.txt"))) != "rename me\n" {
		t.Fatal("rename result was not applied")
	}
}

func TestClonePartialCreationRollbackSkipsPrematureCapture(t *testing.T) {
	lifecycle, _, manifests, fake, _ := lifecycleFixture(t)
	execution, artifacts := clonePlanFixture(t, "partial-create")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000061")
	manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.createResource(context.Background(), &manifest, 0, func(record state.ResourceRecord) (runtime.Resource, error) {
		return fake.CreateNetwork(context.Background(), runtime.NetworkSpec{Name: record.Name, Labels: runtimeLabels(record.Labels)})
	}); err != nil {
		t.Fatal(err)
	}

	if err := lifecycle.rollbackCreate(context.Background(), &manifest); err != nil {
		t.Fatalf("partial clone rollback failed: %v", err)
	}
	if len(fake.resources) != 0 {
		t.Fatalf("partial clone resources survived rollback: %#v", fake.resources)
	}
	if remaining, err := manifests.ListProjectManifests(context.Background(), manifest.ProjectID); err != nil || len(remaining) != 0 {
		t.Fatalf("partial clone manifests = %#v, %v", remaining, err)
	}
}

func TestClonePersistentPrivateAndPublicationLeaseSurvivesHarnessReturn(t *testing.T) {
	lifecycle, root, manifests, fake, _ := lifecycleFixture(t)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLifecycleConfig(t, root, `,"ports":[{"name":"web","guest":3000,"host":43100,"bind":"127.0.0.1","protocol":"tcp"}],"network":{"internet":false,"hostGrants":[{"name":"team-db","destination":"10.40.0.9","port":5432}]}`)
	inspected, err := lifecycle.inspection.Inspect(context.Background(), InspectRequest{Root: root, Mode: string(model.ModeClone), SandboxName: "persistent-bridges"})
	if err != nil {
		t.Fatal(err)
	}
	execution := inspected.Plan
	artifacts := cloneArtifactsForExecution(t, execution)
	capabilities, err := fake.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	capabilities.FixedPublication = false
	fake.capabilities = &capabilities
	publication, err := ports.Plan(execution.Ports, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	defer publication.Abort()
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000062")
	manifest, indexes, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	resolver := &hostBridgeResolver{}
	lifecycle.hostBridges = hostBridgeRuntime{
		resolver: resolver,
		routeSource: func(context.Context, netip.Addr) (netip.Addr, error) {
			return netip.MustParseAddr("192.168.64.1"), nil
		},
		startTCP: func(context.Context, bridge.TCPGrant) (hostTCPRelay, error) {
			return nil, errors.New("clone must not start a foreground relay")
		},
		lease: time.Hour,
	}
	manager := &hostBridgeLeaseManager{environment: map[string]string{
		"DSX_BRIDGE_TEAM_DB_HOST": "192.168.64.1",
		"DSX_BRIDGE_TEAM_DB_PORT": "49152",
	}}
	lifecycle.bridgeLeases = manager
	service := &CloneService{lifecycle: lifecycle, git: &cloneGitStub{}}
	var session *hostBridgeSession
	if _, err := service.createClone(context.Background(), StartRequest{Root: root, Mode: model.ModeClone, Sandbox: "persistent-bridges"}, execution, artifacts, indexes, publication, &manifest, &session); err != nil {
		t.Fatal(err)
	}
	if session == nil || len(manager.ensureSpecs) != 2 {
		t.Fatalf("clone persistent lease session/specs = %#v %#v", session, manager.ensureSpecs)
	}
	modes := map[bridge.RelayMode]bool{}
	for _, spec := range manager.ensureSpecs {
		modes[spec.Mode] = true
	}
	if !modes[bridge.RelayModePrivateHost] || !modes[bridge.RelayModePublication] {
		t.Fatalf("clone lease did not combine private and publication relays: %#v", manager.ensureSpecs)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if len(manager.stopIdentities) != 0 {
		t.Fatalf("harness return stopped clone persistent lease: %#v", manager.stopIdentities)
	}
}

func TestCloneResumePreparationFailureRollsBackRestartedWorkspaceAndLease(t *testing.T) {
	lifecycle, root, manifests, fake, _ := lifecycleFixture(t)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLifecycleConfig(t, root, `,"network":{"internet":false,"hostGrants":[{"name":"team-db","destination":"10.40.0.9","port":5432}]},"processes":{"web":{"argv":["/bin/true"],"required":true}}`)
	inspected, err := lifecycle.inspection.Inspect(context.Background(), InspectRequest{Root: root, Mode: string(model.ModeClone), SandboxName: "resume-rollback"})
	if err != nil {
		t.Fatal(err)
	}
	execution := inspected.Plan
	artifacts := cloneArtifactsForExecution(t, execution)
	publication, err := ports.Plan(execution.Ports, runtime.Capabilities{FixedPublication: true})
	if err != nil {
		t.Fatal(err)
	}
	defer publication.Abort()
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000063")
	manifest, indexes, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	guest := &lifecycleGuest{runtime: fake, status: guestproto.StatusResult{
		Generation: 1,
		Processes:  []guestproto.ProcessStatus{{ID: "web", Generation: 1, State: guestproto.StateReady, Ready: true, Required: true}},
	}}
	lifecycle.guest = guest
	lifecycle.hostBridges = hostBridgeRuntime{
		resolver: &hostBridgeResolver{},
		routeSource: func(context.Context, netip.Addr) (netip.Addr, error) {
			return netip.MustParseAddr("192.168.64.1"), nil
		},
		startTCP: func(context.Context, bridge.TCPGrant) (hostTCPRelay, error) {
			return nil, errors.New("clone must not start a foreground relay")
		},
		lease: time.Hour,
	}
	manager := &hostBridgeLeaseManager{environment: map[string]string{
		"DSX_BRIDGE_TEAM_DB_HOST": "192.168.64.1",
		"DSX_BRIDGE_TEAM_DB_PORT": "49152",
	}}
	lifecycle.bridgeLeases = manager
	service := &CloneService{lifecycle: lifecycle, git: &cloneGitStub{}}
	var initialSession *hostBridgeSession
	workspace, err := service.createClone(context.Background(), StartRequest{Root: root, Mode: model.ModeClone, Sandbox: "resume-rollback"}, execution, artifacts, indexes, publication, &manifest, &initialSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateRunning, "create", ""); err != nil {
		t.Fatal(err)
	}
	if err := fake.Stop(context.Background(), workspace, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"}); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateStopped, "stop", ""); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.stopPersistentHostBridges(context.Background(), bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}); err != nil {
		t.Fatal(err)
	}
	manager.stopIdentities = nil

	guest.startErr = errors.New("injected resumed guest failure")
	var resumedSession *hostBridgeSession
	if _, _, _, err := service.prepareCloneResume(context.Background(), execution, &manifest, &resumedSession); err == nil {
		t.Fatal("clone resume preparation succeeded despite guest failure")
	}
	identity := bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}
	if !reflect.DeepEqual(manager.stopIdentities, []bridge.LeaseIdentity{identity}) {
		t.Fatalf("clone resume rollback lease stops = %#v", manager.stopIdentities)
	}
	if got := fake.resources[workspace.ID].State; got != "stopped" {
		t.Fatalf("restarted clone workspace state = %q", got)
	}
	stored, found, err := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if err != nil || !found || stored.State != model.StateStopped {
		t.Fatalf("rolled-back clone manifest: found=%t state=%q err=%v", found, stored.State, err)
	}
}

func cloneArtifactsForExecution(t *testing.T, execution plan.ExecutionPlan) []gitx.SourceArtifact {
	t.Helper()
	if len(execution.Repositories) == 0 {
		t.Fatal("clone inspection returned no repositories")
	}
	artifacts := make([]gitx.SourceArtifact, len(execution.Repositories))
	for index, repository := range execution.Repositories {
		artifacts[index] = gitx.SourceArtifact{
			Repository: gitx.Repository{
				Name: repository.Name, HostPath: repository.HostPath, GuestPath: repository.GuestPath,
				Identity: cleanupRepositoryIdentity(execution.Project.CanonicalRoot, repository.HostPath),
			},
			SourceRef: "refs/heads/main", SourceCommit: cloneTestCommit, TrackedFingerprint: cloneTestFingerprint,
			BundlePath:   filepath.Join(execution.Project.CanonicalRoot, fmt.Sprintf("source-%d.bundle", index)),
			BundleDigest: cloneTestDigest, BundleRef: fmt.Sprintf("refs/dsx/private/source/%032d", index),
		}
	}
	return artifacts
}

func runGit(t *testing.T, executable, directory string, environment map[string]string, arguments ...string) string {
	t.Helper()
	command := exec.Command(executable, arguments...)
	command.Dir = directory
	command.Env = os.Environ()
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}

func writeTestFile(t *testing.T, name string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, data, mode); err != nil {
		t.Fatal(err)
	}
}

func mustReadFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
