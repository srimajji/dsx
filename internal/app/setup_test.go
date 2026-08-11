package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	agentimage "github.com/srimajji/dsx/images/agent"
	"github.com/srimajji/dsx/internal/config"
	projectinspect "github.com/srimajji/dsx/internal/inspect"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

type setupSaveResult struct {
	err               error
	mutateBeforeError bool
}

type setupApprovalRepository struct {
	record      state.ApprovalRecord
	found       bool
	saves       int
	deletes     int
	loadErr     error
	err         error
	deleteErr   error
	saveResults []setupSaveResult
}

func (repository *setupApprovalRepository) LoadApproval(context.Context, model.ProjectID) (state.ApprovalRecord, bool, error) {
	return repository.record, repository.found, repository.loadErr
}

func (repository *setupApprovalRepository) SaveApproval(_ context.Context, record state.ApprovalRecord) error {
	repository.saves++
	if repository.saves <= len(repository.saveResults) {
		result := repository.saveResults[repository.saves-1]
		if result.err == nil || result.mutateBeforeError {
			repository.record = record
			repository.found = true
		}
		return result.err
	}
	if repository.err != nil {
		return repository.err
	}
	repository.record = record
	repository.found = true
	return nil
}

func (repository *setupApprovalRepository) DeleteApproval(context.Context, model.ProjectID) error {
	repository.deletes++
	if repository.deleteErr != nil {
		return repository.deleteErr
	}
	repository.record = state.ApprovalRecord{}
	repository.found = false
	return nil
}

type setupInventory struct{ count int }

func (inventory setupInventory) CountOwnedResources(context.Context, model.ProjectID) (int, error) {
	return inventory.count, nil
}

type setupImagePreparer struct {
	calls     int
	execution plan.ExecutionPlan
	err       error
}

func (preparer *setupImagePreparer) PrepareStandardImage(_ context.Context, execution plan.ExecutionPlan) error {
	preparer.calls++
	preparer.execution = execution
	return preparer.err
}

type setupContainerSystem struct {
	calls       int
	statusCalls int
	startCalls  int
	status      runtime.SystemStatus
	err         error
}

func (system *setupContainerSystem) CheckSystemStatus(context.Context) error {
	system.calls++
	return system.err
}

func (system *setupContainerSystem) Status(context.Context) (runtime.SystemStatus, error) {
	system.statusCalls++
	if system.status.State == "" {
		return runtime.SystemStatus{State: runtime.SystemStateRunning}, nil
	}
	return system.status, nil
}

func (system *setupContainerSystem) StartSystem(context.Context) error {
	system.startCalls++
	return system.err
}

func setupTestService(t *testing.T, root string, approvals state.ApprovalRepository, inventory OwnedResourceInventory) *SetupService {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "Containerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection := NewInspectionServiceWithDependencies(InspectionDependencies{
		InspectProject: func(string) (projectinspect.Facts, error) {
			return projectinspect.Facts{WorkspaceRoot: root, Containerfiles: []string{"Containerfile"}}, nil
		},
		Resolver: plan.NewResolver(),
	})
	return NewSetupServiceWithDependencies(SetupDependencies{
		Inspection:      inspection,
		Approvals:       approvals,
		Inventory:       inventory,
		ContainerSystem: &setupContainerSystem{},
		Now:             func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) },
		DSXVersion:      "test",
	})
}

func TestSetupLauncherDashboardSelection(t *testing.T) {
	root := t.TempDir()
	service := setupTestService(t, root, &setupApprovalRepository{}, setupInventory{})
	stateResult, err := service.BareState(context.Background(), BareStateRequest{Root: root})
	if err != nil || stateResult.Screen != BareSetup {
		t.Fatalf("unconfigured state = %#v, err = %v", stateResult, err)
	}
	if err := os.Mkdir(filepath.Join(root, ".dsx"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, projectConfigPath), []byte(`{"schemaVersion":1,"workspace":{"root":"."},"image":{"standard":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stateResult, err = service.BareState(context.Background(), BareStateRequest{Root: root})
	if err != nil || stateResult.Screen != BareLauncher {
		t.Fatalf("configured state = %#v, err = %v", stateResult, err)
	}
	service.inventory = setupInventory{count: 2}
	stateResult, err = service.BareState(context.Background(), BareStateRequest{Root: root})
	if err != nil || stateResult.Screen != BareDashboard || stateResult.OwnedResources != 2 {
		t.Fatalf("resource state = %#v, err = %v", stateResult, err)
	}
}
func TestBareStateReportsContainerSystemAndStartsItExplicitly(t *testing.T) {
	root := t.TempDir()
	service := setupTestService(t, root, &setupApprovalRepository{}, setupInventory{})
	controller := &setupContainerSystem{status: runtime.SystemStatus{
		State: runtime.SystemStateStopped, Remediation: "Run `container system start` to continue.",
	}}
	service.containerSystem = controller

	stateResult, err := service.BareState(context.Background(), BareStateRequest{Root: root})
	if err != nil || stateResult.ContainerSystem.State != runtime.SystemStateStopped {
		t.Fatalf("BareState() = %#v, %v", stateResult, err)
	}
	if controller.statusCalls != 1 || controller.startCalls != 0 {
		t.Fatalf("read-only state calls: status=%d start=%d", controller.statusCalls, controller.startCalls)
	}
	if err := service.StartContainerSystem(context.Background()); err != nil {
		t.Fatal(err)
	}
	if controller.statusCalls != 2 || controller.startCalls != 1 || controller.calls != 1 {
		t.Fatalf("explicit start calls: status=%d start=%d check=%d", controller.statusCalls, controller.startCalls, controller.calls)
	}
}

func TestApproveExistingPersistsReviewedHashWithoutRewritingConfiguration(t *testing.T) {
	root := t.TempDir()
	approvals := &setupApprovalRepository{}
	service := setupTestService(t, root, approvals, setupInventory{})
	containerSystem := &setupContainerSystem{}
	service.containerSystem = containerSystem
	generated, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	configDirectory := filepath.Join(root, ".dsx")
	if err := os.Mkdir(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDirectory, "config.jsonc")
	if err := os.WriteFile(configPath, generated.RenderedConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewExisting(context.Background(), BareStateRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ApproveExisting(context.Background(), InitializeRequest{
		Root:                           root,
		ExpectedHash:                   preview.Hash,
		ExpectedConfigDigest:           preview.ConfigContentDigest,
		ExpectedImportedContentDigests: append([]state.ContentDigest(nil), preview.ImportedContentDigests...),
		ExpectedProjectState:           preview.ProjectState,
		Confirmed:                      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created || result.Hash != preview.Hash || !reflect.DeepEqual(before, after) {
		t.Fatalf("result = %#v, configuration changed = %t", result, !reflect.DeepEqual(before, after))
	}
	if approvals.saves != 1 || !approvals.found || approvals.record.Hash != preview.Hash {
		t.Fatalf("approval repository = %#v", approvals)
	}
	if containerSystem.calls != 1 {
		t.Fatalf("container status checks = %d, want 1", containerSystem.calls)
	}
}

func TestSetupCancelCreatesNothing(t *testing.T) {
	root := t.TempDir()
	repository := &setupApprovalRepository{}
	service := setupTestService(t, root, repository, nil)
	_, err := service.Initialize(context.Background(), InitializeRequest{Root: root, Confirmed: false})
	if err == nil {
		t.Fatal("Initialize without confirmation succeeded")
	}
	if _, statErr := os.Stat(filepath.Join(root, ".dsx")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("setup cancellation created .dsx: %v", statErr)
	}
	if repository.saves != 0 {
		t.Fatalf("approval saves = %d, want 0", repository.saves)
	}
}
func TestSetupWithoutDetectedImageSelectsManagedStandard(t *testing.T) {
	root := t.TempDir()
	service := NewSetupServiceWithDependencies(SetupDependencies{
		Inspection: NewInspectionServiceWithDependencies(InspectionDependencies{
			InspectProject: func(string) (projectinspect.Facts, error) {
				return projectinspect.Facts{WorkspaceRoot: root}, nil
			},
			Resolver: plan.NewResolver(),
		}),
		Approvals: &setupApprovalRepository{},
	})
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Config.Image.Standard || preview.Config.Image.Ref != "" || preview.Config.Image.Build != nil {
		t.Fatalf("standard image suggestion = %#v", preview.Config.Image)
	}
	if preview.SelectedImageOption != "standard" || preview.Plan.Image.InputDigest != agentimage.InputDigest() || preview.Hash == "" {
		t.Fatalf("standard image preview = %#v", preview)
	}
	if len(preview.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", preview.Diagnostics)
	}
}

func TestStageStandardImageMatchesEmbeddedAuthority(t *testing.T) {
	projectRoot := t.TempDir()
	stageRoot, digest, err := stageStandardImage(context.Background(), projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stageRoot)
	if digest != agentimage.InputDigest() {
		t.Fatalf("staged digest = %q, want %q", digest, agentimage.InputDigest())
	}
	for _, name := range []string{agentimage.BuildFile, "harnesses.lock.json"} {
		info, statErr := os.Stat(filepath.Join(stageRoot, name))
		if statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("staged asset %q: info=%#v err=%v", name, info, statErr)
		}
	}
}

func TestSetupListsStandardAndDetectedImageOptions(t *testing.T) {
	standard := "ghcr.io/example/dsx-agent@sha256:" + strings.Repeat("a", 64)
	facts := projectinspect.Facts{
		Containerfiles: []string{"Dockerfile", ".devcontainer/Dockerfile"},
	}
	options := setupImageOptions(facts, standard)
	if len(options) != 4 {
		t.Fatalf("image options = %#v", options)
	}
	document, selected := suggestConfig(facts, options)
	if selected != "standard" || document.Image.Ref != standard {
		t.Fatalf("selected option = %q, image = %#v", selected, document.Image)
	}
	if document.Resources.CPUs != DefaultWorkspaceCPUs || document.Resources.Memory != DefaultWorkspaceMemory {
		t.Fatalf("suggested resources = %#v", document.Resources)
	}
	for index, id := range []string{"standard", "dockerfile:Dockerfile", "dockerfile:.devcontainer/Dockerfile", "custom"} {
		if options[index].ID != id {
			t.Fatalf("option %d = %#v, want %q", index, options[index], id)
		}
	}
}

func TestSetupWritesNamespacedHomeConfigAndRejectsSharedAmbiguity(t *testing.T) {
	root := t.TempDir()
	configRoot := filepath.Join(t.TempDir(), ".dsx")
	if err := os.WriteFile(filepath.Join(root, "Containerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection := NewInspectionServiceWithDependencies(InspectionDependencies{
		InspectProject: func(string) (projectinspect.Facts, error) {
			return projectinspect.Facts{WorkspaceRoot: root, Containerfiles: []string{"Containerfile"}}, nil
		},
		Resolver:   plan.NewResolver(),
		ConfigRoot: configRoot,
	})
	repository := &setupApprovalRepository{}
	service := NewSetupServiceWithDependencies(SetupDependencies{
		Inspection: inspection, Approvals: repository, ContainerSystem: &setupContainerSystem{},
	})
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Initialize(context.Background(), InitializeRequest{
		Root: root, ExpectedHash: preview.Hash, ExpectedConfigDigest: preview.ConfigContentDigest,
		ExpectedProjectState: preview.ProjectState, Confirmed: true, RenderedConfig: preview.RenderedConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(configRoot, "projects", projectConfigNamespace(filepath.Base(root))+"-"+string(projectID), "config.jsonc")
	if result.ConfigPath != want {
		t.Fatalf("config path = %q, want %q", result.ConfigPath, want)
	}
	if _, err := os.Stat(filepath.Join(root, projectConfigPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup wrote repository config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".dsx"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, projectConfigPath), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.BareState(context.Background(), BareStateRequest{Root: root}); model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("ambiguous configs error = %v", err)
	}
}

func TestSetupOffersNestedImageRecipesWithoutReadingDevContainerConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "fixtures", "example", ".devcontainer"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fixtures", "example", "Containerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	devcontainer := []byte(`{"build":{"context":"..","dockerfile":"../Containerfile"}}`)
	if err := os.WriteFile(filepath.Join(root, "fixtures", "example", ".devcontainer", "devcontainer.json"), devcontainer, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(root, "release-artifact")
	if err := os.WriteFile(artifact, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(artifact, projectinspect.MaxFileBytes+1); err != nil {
		t.Fatal(err)
	}

	service := NewSetupServiceWithDependencies(SetupDependencies{})
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Config.Image.Standard || preview.Config.Image.Ref != "" || preview.Config.Image.Build != nil {
		t.Fatalf("nested container recipe affected standard image selection: %#v", preview.Config.Image)
	}
	if len(preview.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", preview.Diagnostics)
	}
	if len(preview.ImageOptions) != 3 || preview.ImageOptions[1].ID != "dockerfile:fixtures/example/Containerfile" {
		t.Fatalf("nested container recipe options = %#v", preview.ImageOptions)
	}
}

func TestSetupFinalConfirmationWritesConfigAndApproval(t *testing.T) {
	root := t.TempDir()
	repository := &setupApprovalRepository{}
	service := setupTestService(t, root, repository, nil)
	preparer := &setupImagePreparer{}
	service.imagePreparer = preparer
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Initialize(context.Background(), InitializeRequest{
		Root:                 root,
		ExpectedHash:         preview.Hash,
		ExpectedConfigDigest: preview.ConfigContentDigest,
		ExpectedProjectState: preview.ProjectState,
		Confirmed:            true,
		RenderedConfig:       preview.RenderedConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || repository.saves != 1 || repository.record.Hash != preview.Hash {
		t.Fatalf("result = %#v, saves = %d, approval = %#v", result, repository.saves, repository.record)
	}
	if preparer.calls != 1 || !preparer.execution.Image.Standard || preparer.execution.ExecutableHash != preview.Hash {
		t.Fatalf("standard image preparation: calls=%d execution=%#v", preparer.calls, preparer.execution)
	}
	info, err := os.Stat(result.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %04o, want 0600", info.Mode().Perm())
	}
	file, err := os.Open(result.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	_, diagnostics := config.Parse(projectConfigPath, file)
	_ = file.Close()
	if diagnosticsHaveErrors(diagnostics) {
		t.Fatalf("written configuration diagnostics = %#v", diagnostics)
	}
}
func TestSetupChecksContainerSystemBeforePersisting(t *testing.T) {
	root := t.TempDir()
	repository := &setupApprovalRepository{}
	service := setupTestService(t, root, repository, nil)
	checkFailure := errors.New("container API service is stopped; run `container system start` and retry")
	containerSystem := &setupContainerSystem{err: checkFailure}
	imagePreparer := &setupImagePreparer{}
	service.containerSystem = containerSystem
	service.imagePreparer = imagePreparer
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.Initialize(context.Background(), confirmedInitializeRequest(root, preview))
	if model.ErrorCodeOf(err) != model.CodeUnavailable || !errors.Is(err, checkFailure) {
		t.Fatalf("Initialize() = %#v, %v; want unavailable container status error", result, err)
	}
	if containerSystem.calls != 1 {
		t.Fatalf("container status checks = %d, want 1", containerSystem.calls)
	}
	if repository.saves != 0 || imagePreparer.calls != 0 || result.ConfigPath != "" || result.Created {
		t.Fatalf("preflight failure mutated setup: result=%#v saves=%d image prepares=%d", result, repository.saves, imagePreparer.calls)
	}
}

func TestSetupStandardBuildFailureKeepsReviewedConfigurationForRetry(t *testing.T) {
	root := t.TempDir()
	repository := &setupApprovalRepository{}
	service := setupTestService(t, root, repository, nil)
	service.imagePreparer = &setupImagePreparer{err: errors.New("builder unavailable")}
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Initialize(context.Background(), InitializeRequest{
		Root:                 root,
		ExpectedHash:         preview.Hash,
		ExpectedConfigDigest: preview.ConfigContentDigest,
		ExpectedProjectState: preview.ProjectState,
		Confirmed:            true,
		RenderedConfig:       preview.RenderedConfig,
	})
	if model.ErrorCodeOf(err) != model.CodeUnavailable || !result.Created {
		t.Fatalf("Initialize() = %#v, %v", result, err)
	}
	if _, statErr := os.Stat(result.ConfigPath); statErr != nil {
		t.Fatalf("saved configuration after build failure: %v", statErr)
	}
	if repository.saves != 1 || repository.record.Hash != preview.Hash {
		t.Fatalf("saved approval after build failure = %#v", repository.record)
	}
}

func TestSetupChangedPreviewRefusesWithoutPartialFile(t *testing.T) {
	root := t.TempDir()
	lockfile := filepath.Join(root, "go.sum")
	if err := os.WriteFile(lockfile, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Containerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository := &setupApprovalRepository{}
	inspection := NewInspectionServiceWithDependencies(InspectionDependencies{
		InspectProject: func(string) (projectinspect.Facts, error) {
			return projectinspect.Facts{WorkspaceRoot: root, Containerfiles: []string{"Containerfile"}, Lockfiles: []projectinspect.Lockfile{{Path: "go.sum", Ecosystem: "go"}}}, nil
		},
		Resolver: plan.NewResolver(),
	})
	service := NewSetupServiceWithDependencies(SetupDependencies{Inspection: inspection, Approvals: repository, DSXVersion: "test"})
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockfile, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = service.Initialize(context.Background(), InitializeRequest{
		Root:                 root,
		ExpectedHash:         preview.Hash,
		ExpectedConfigDigest: preview.ConfigContentDigest,
		ExpectedProjectState: preview.ProjectState,
		Confirmed:            true,
		RenderedConfig:       preview.RenderedConfig,
	})
	if err == nil {
		t.Fatal("Initialize accepted changed project state")
	}
	if _, statErr := os.Stat(filepath.Join(root, projectConfigPath)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("changed preview left config: %v", statErr)
	}
	if repository.saves != 0 {
		t.Fatalf("approval saves = %d, want 0", repository.saves)
	}
}

func TestSetupApprovalFailureRollsBackConfig(t *testing.T) {
	root := t.TempDir()
	repository := &setupApprovalRepository{err: errors.New("state unavailable")}
	service := setupTestService(t, root, repository, nil)
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Initialize(context.Background(), InitializeRequest{
		Root:                 root,
		ExpectedHash:         preview.Hash,
		ExpectedConfigDigest: preview.ConfigContentDigest,
		ExpectedProjectState: preview.ProjectState,
		Confirmed:            true,
		RenderedConfig:       preview.RenderedConfig,
	})
	if err == nil {
		t.Fatal("Initialize succeeded when approval persistence failed")
	}
	if _, statErr := os.Stat(filepath.Join(root, projectConfigPath)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("approval failure left config: %v", statErr)
	}
}
func TestSetupApprovalRollbackLoadFailureWritesNothing(t *testing.T) {
	root := t.TempDir()
	loadFailure := errors.New("approval load failed")
	repository := &setupApprovalRepository{loadErr: loadFailure}
	service := setupTestService(t, root, repository, nil)
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Initialize(context.Background(), confirmedInitializeRequest(root, preview))
	if !errors.Is(err, loadFailure) {
		t.Fatalf("Initialize() error = %v, want load failure", err)
	}
	if repository.saves != 0 || repository.deletes != 0 {
		t.Fatalf("load failure changed approval: saves=%d deletes=%d", repository.saves, repository.deletes)
	}
	assertSetupConfigMissing(t, root)
}

func TestSetupNewApprovalRollbackAfterPostRenameFailure(t *testing.T) {
	root := t.TempDir()
	syncFailure := errors.New("post-rename directory sync failed")
	repository := &setupApprovalRepository{
		saveResults: []setupSaveResult{{err: syncFailure, mutateBeforeError: true}},
	}
	service := setupTestService(t, root, repository, nil)
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Initialize(context.Background(), confirmedInitializeRequest(root, preview))
	if !errors.Is(err, syncFailure) {
		t.Fatalf("Initialize() error = %v, want sync failure", err)
	}
	if repository.found || repository.deletes != 1 {
		t.Fatalf("approval rollback left record: found=%v deletes=%d record=%#v", repository.found, repository.deletes, repository.record)
	}
	assertSetupConfigMissing(t, root)
}

func TestSetupExistingApprovalRollbackRestoresPriorRecord(t *testing.T) {
	root := t.TempDir()
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	prior := state.ApprovalRecord{
		Version:                state.ApprovalRecordVersion,
		ProjectID:              projectID,
		Hash:                   strings.Repeat("a", 64),
		ApprovedAt:             time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC),
		DSXVersion:             "prior",
		ConfigContentDigest:    strings.Repeat("b", 64),
		ImportedContentDigests: []state.ContentDigest{{Path: "prior.json", Digest: strings.Repeat("c", 64)}},
	}
	syncFailure := errors.New("replacement directory sync failed")
	repository := &setupApprovalRepository{
		record:      prior,
		found:       true,
		saveResults: []setupSaveResult{{err: syncFailure, mutateBeforeError: true}},
	}
	service := setupTestService(t, root, repository, nil)
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Initialize(context.Background(), confirmedInitializeRequest(root, preview))
	if !errors.Is(err, syncFailure) {
		t.Fatalf("Initialize() error = %v, want sync failure", err)
	}
	if repository.saves != 2 || !repository.found || !reflect.DeepEqual(repository.record, prior) {
		t.Fatalf("prior approval not restored byte-equivalently: saves=%d found=%v record=%#v want=%#v", repository.saves, repository.found, repository.record, prior)
	}
	assertSetupConfigMissing(t, root)
}

func TestSetupApprovalRollbackFailureReportsRecoverableResidue(t *testing.T) {
	root := t.TempDir()
	syncFailure := errors.New("post-rename directory sync failed")
	deleteFailure := errors.New("approval delete rollback failed")
	repository := &setupApprovalRepository{
		deleteErr:   deleteFailure,
		saveResults: []setupSaveResult{{err: syncFailure, mutateBeforeError: true}},
	}
	service := setupTestService(t, root, repository, nil)
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Initialize(context.Background(), confirmedInitializeRequest(root, preview))
	if err == nil || !errors.Is(err, syncFailure) || !errors.Is(err, deleteFailure) || !strings.Contains(err.Error(), "recoverable residue") {
		t.Fatalf("Initialize() error = %v, want combined save/delete residue error", err)
	}
	if !repository.found {
		t.Fatal("rollback failure did not preserve simulated recoverable approval residue")
	}
	assertSetupConfigMissing(t, root)
}

func confirmedInitializeRequest(root string, preview SetupPreview) InitializeRequest {
	return InitializeRequest{
		Root:                           root,
		ExpectedHash:                   preview.Hash,
		ExpectedConfigDigest:           preview.ConfigContentDigest,
		ExpectedProjectState:           preview.ProjectState,
		ExpectedImportedContentDigests: append([]state.ContentDigest(nil), preview.ImportedContentDigests...),
		Confirmed:                      true,
		RenderedConfig:                 preview.RenderedConfig,
	}
}

func TestUpdateExistingReplacesOnlyReviewedDynamicLoopbackPorts(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".dsx"), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"schemaVersion":1,"workspace":{"root":"."},"image":{"ref":"ghcr.io/example/dev@sha256:` + strings.Repeat("a", 64) + `"},"agents":{"default":"codex","allowed":["codex"]}}`)
	configPath := filepath.Join(root, projectConfigPath)
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	repository := &setupApprovalRepository{}
	service := setupTestService(t, root, repository, nil)
	current, err := service.PreviewExisting(context.Background(), BareStateRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	document := current.Config
	document.Ports = []config.PortConfig{{
		Name: "port-3000", Guest: 3000, Host: config.HostPort{Dynamic: true}, Bind: "127.0.0.1", Protocol: "tcp",
	}}
	candidate, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root, Config: document})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.UpdateExisting(context.Background(), InitializeRequest{
		Root: root, ExpectedHash: candidate.Hash, ExpectedConfigDigest: candidate.ConfigContentDigest,
		ExpectedProjectState: candidate.ProjectState, ReplacesConfigDigest: current.ConfigContentDigest,
		Confirmed: true, RenderedConfig: candidate.RenderedConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Hash != candidate.Hash || repository.record.Hash != candidate.Hash {
		t.Fatalf("update result=%#v approval=%#v", result, repository.record)
	}
	updated, err := service.PreviewExisting(context.Background(), BareStateRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Config.Ports) != 1 || updated.Config.Ports[0].Guest != 3000 || !updated.Config.Ports[0].Host.Dynamic {
		t.Fatalf("updated ports = %#v", updated.Config.Ports)
	}

	changedDocument := updated.Config
	changedDocument.Image.Ref = "ghcr.io/example/other@sha256:" + strings.Repeat("b", 64)
	changedCandidate, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root, Config: changedDocument})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.UpdateExisting(context.Background(), InitializeRequest{
		Root: root, ExpectedHash: changedCandidate.Hash, ExpectedConfigDigest: changedCandidate.ConfigContentDigest,
		ExpectedProjectState: changedCandidate.ProjectState, ReplacesConfigDigest: updated.ConfigContentDigest,
		Confirmed: true, RenderedConfig: changedCandidate.RenderedConfig,
	})
	if model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("non-port update error = %v, want invalid input", err)
	}
}

func assertSetupConfigMissing(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, projectConfigPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup failure left configuration: %v", err)
	}
}
