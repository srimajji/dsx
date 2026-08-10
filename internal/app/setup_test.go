package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/config"
	projectinspect "github.com/srimajji/dsx/internal/inspect"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
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
		Inspection: inspection,
		Approvals:  approvals,
		Inventory:  inventory,
		Now:        func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) },
		DSXVersion: "test",
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
	if err := os.WriteFile(filepath.Join(root, projectConfigPath), []byte("{}"), 0o600); err != nil {
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

func TestApproveExistingPersistsReviewedHashWithoutRewritingConfiguration(t *testing.T) {
	root := t.TempDir()
	approvals := &setupApprovalRepository{}
	service := setupTestService(t, root, approvals, setupInventory{})
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
func TestSetupWithoutDetectedImageRequiresExplicitInput(t *testing.T) {
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
	if preview.Hash != "" || preview.Config.Image.Ref != "" || preview.Config.Image.Build != nil {
		t.Fatalf("setup invented an image or executable plan: %#v", preview)
	}
	if len(preview.Diagnostics) != 1 || preview.Diagnostics[0].Code != "image_required" {
		t.Fatalf("diagnostics = %#v", preview.Diagnostics)
	}
}

func TestSetupIgnoresNestedImageRecipesWhenSuggestingProjectImage(t *testing.T) {
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
	if preview.Config.Image.Ref != "" || preview.Config.Image.Build != nil {
		t.Fatalf("nested container recipe selected as project image: %#v", preview.Config.Image)
	}
	if len(preview.Diagnostics) != 1 || preview.Diagnostics[0].Code != "image_required" {
		t.Fatalf("diagnostics = %#v", preview.Diagnostics)
	}
}

func TestSetupFinalConfirmationWritesConfigAndApproval(t *testing.T) {
	root := t.TempDir()
	repository := &setupApprovalRepository{}
	service := setupTestService(t, root, repository, nil)
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

func TestImportedDigestPersistenceAndConfirmation(t *testing.T) {
	root := t.TempDir()
	importPath := ".devcontainer/devcontainer.json"
	importBytes := []byte(`{"image":"alpine@sha256:` + strings.Repeat("d", 64) + `"}`)
	if err := os.MkdirAll(filepath.Join(root, ".devcontainer"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(importPath)), importBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	image := "alpine@sha256:" + strings.Repeat("d", 64)
	inspection := NewInspectionService(plan.NewResolver())
	repository := &setupApprovalRepository{}
	service := NewSetupServiceWithDependencies(SetupDependencies{
		Inspection: inspection,
		Approvals:  repository,
		Now:        func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) },
		DSXVersion: "test",
	})
	document := config.ConfigDocument{
		SchemaVersion: 1,
		Imports: config.ImportConfig{Devcontainer: &config.DevcontainerImport{
			Path: importPath, Fields: []string{"image"},
		}},
		Workspace: config.WorkspaceConfig{Root: "."},
		Image:     config.ImageConfig{Ref: image},
		Agents:    config.AgentConfig{Default: "codex", Allowed: []string{"codex"}},
	}
	preview, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: root, Config: document})
	if err != nil {
		t.Fatal(err)
	}
	expectedDigest := sha256.Sum256(importBytes)
	wantDigests := []state.ContentDigest{{Path: importPath, Digest: fmt.Sprintf("%x", expectedDigest)}}
	if !reflect.DeepEqual(preview.ImportedContentDigests, wantDigests) {
		t.Fatalf("preview imported digests = %#v, want %#v", preview.ImportedContentDigests, wantDigests)
	}
	request := confirmedInitializeRequest(root, preview)
	request.ExpectedImportedContentDigests = nil
	if _, err := service.Initialize(context.Background(), request); model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("Initialize() with empty reviewed imports error = %v, want unapproved", err)
	}
	request.ExpectedImportedContentDigests = []state.ContentDigest{{Path: importPath, Digest: ""}}
	if _, err := service.Initialize(context.Background(), request); model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("Initialize() with malformed reviewed import error = %v, want unapproved", err)
	}
	if repository.saves != 0 {
		t.Fatalf("rejected imported digests saved approval %d times", repository.saves)
	}
	request.ExpectedImportedContentDigests = append([]state.ContentDigest(nil), preview.ImportedContentDigests...)
	if _, err := service.Initialize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(repository.record.ImportedContentDigests, wantDigests) {
		t.Fatalf("saved imported digests = %#v, want %#v", repository.record.ImportedContentDigests, wantDigests)
	}
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

func assertSetupConfigMissing(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, projectConfigPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup failure left configuration: %v", err)
	}
}
