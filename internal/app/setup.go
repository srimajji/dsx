package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/srimajji/dsx/internal/buildinfo"
	"github.com/srimajji/dsx/internal/config"
	projectinspect "github.com/srimajji/dsx/internal/inspect"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/state"
)

// OwnedResourceInventory is the read-only project state port used to choose a
// bare-command screen. Implementations read DSX manifests; they must not query
// or mutate the runtime.
type OwnedResourceInventory interface {
	CountOwnedResources(context.Context, model.ProjectID) (int, error)
}

type SetupDependencies struct {
	Inspection *InspectionService
	Approvals  state.ApprovalRepository
	Inventory  OwnedResourceInventory
	Now        func() time.Time
	DSXVersion string
}

// SetupService owns the final-confirmation transaction. PreviewSetup is
// read-only; Initialize is the only setup path allowed to write configuration
// and approval state.
type SetupService struct {
	inspection *InspectionService
	approvals  state.ApprovalRepository
	inventory  OwnedResourceInventory
	now        func() time.Time
	version    string
}

func NewSetupService(inspection *InspectionService, approvals state.ApprovalRepository, inventory OwnedResourceInventory) *SetupService {
	return NewSetupServiceWithDependencies(SetupDependencies{Inspection: inspection, Approvals: approvals, Inventory: inventory})
}

func NewSetupServiceWithDependencies(dependencies SetupDependencies) *SetupService {
	if dependencies.Inspection == nil {
		dependencies.Inspection = NewInspectionService(plan.NewResolver())
	}
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	if dependencies.DSXVersion == "" {
		dependencies.DSXVersion = buildinfo.Current().Version
	}
	if dependencies.DSXVersion == "" {
		dependencies.DSXVersion = "development"
	}
	return &SetupService{
		inspection: dependencies.Inspection,
		approvals:  dependencies.Approvals,
		inventory:  dependencies.Inventory,
		now:        dependencies.Now,
		version:    dependencies.DSXVersion,
	}
}

func (service *SetupService) BareState(ctx context.Context, request BareStateRequest) (BareState, error) {
	if ctx == nil {
		return BareState{}, model.NewError(model.CodeInvalidInput, "bare state: context is nil", nil)
	}
	if service == nil || service.inspection == nil || service.inspection.inspectProject == nil {
		return BareState{}, model.NewError(model.CodeInternal, "bare state service is not configured", nil)
	}
	facts, err := service.inspection.inspectProject(request.Root)
	if err != nil {
		return BareState{}, model.Wrap(model.CodeInvalidInput, "inspect project state", err)
	}
	mapped := mapProjectFacts(facts)
	configPath := filepath.Join(facts.WorkspaceRoot, filepath.FromSlash(projectConfigPath))
	info, statErr := os.Lstat(configPath)
	configExists := false
	switch {
	case errors.Is(statErr, os.ErrNotExist):
	case statErr != nil:
		return BareState{}, model.Wrap(model.CodeInvalidInput, "inspect project configuration", statErr)
	case info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
		return BareState{}, model.NewError(model.CodeInvalidInput, "project configuration must be a regular non-symlink file", nil)
	default:
		configExists = true
		mapped.ConfigExists = true
		mapped.ConfigPath = projectConfigPath
	}
	projectID, err := model.NewProjectID(facts.WorkspaceRoot)
	if err != nil {
		return BareState{}, model.Wrap(model.CodeInvalidInput, "derive project identity", err)
	}
	owned := 0
	if service.inventory != nil {
		owned, err = service.inventory.CountOwnedResources(ctx, projectID)
		if err != nil {
			return BareState{}, model.Wrap(model.CodeUnavailable, "read owned project resources", err)
		}
		if owned < 0 {
			return BareState{}, model.NewError(model.CodeInternal, "owned resource count is negative", nil)
		}
	}
	screen := BareSetup
	if owned > 0 {
		screen = BareDashboard
	} else if configExists {
		screen = BareLauncher
	}
	return BareState{Screen: screen, ConfigExists: configExists, OwnedResources: owned, Facts: mapped}, nil
}

func (service *SetupService) PreviewSetup(ctx context.Context, request SetupPreviewRequest) (SetupPreview, error) {
	if ctx == nil {
		return SetupPreview{}, model.NewError(model.CodeInvalidInput, "setup preview: context is nil", nil)
	}
	if service == nil || service.inspection == nil || service.inspection.inspectProject == nil || service.inspection.resolver == nil {
		return SetupPreview{}, model.NewError(model.CodeInternal, "setup service is not configured", nil)
	}
	facts, err := service.inspection.inspectProject(request.Root)
	if err != nil {
		return SetupPreview{}, model.Wrap(model.CodeInvalidInput, "inspect setup project", err)
	}
	preview := SetupPreview{Facts: mapProjectFacts(facts)}
	preview.Diagnostics = append(preview.Diagnostics, mapInspectDiagnostics(facts.Diagnostics)...)
	if diagnosticsHaveErrors(preview.Diagnostics) {
		sortDiagnostics(preview.Diagnostics)
		return preview, invalidDiagnosticsError(preview.Diagnostics)
	}

	generatedSuggestion := len(request.RenderedConfig) == 0 && request.Config.SchemaVersion == 0
	document := request.Config
	rendered := append([]byte(nil), request.RenderedConfig...)
	if len(rendered) == 0 {
		if generatedSuggestion {
			document = suggestConfig(facts)
		}
		rendered, err = json.MarshalIndent(document, "", "  ")
		if err != nil {
			return preview, model.Wrap(model.CodeInvalidInput, "render setup configuration", err)
		}
		rendered = append(rendered, '\n')
	}
	if generatedSuggestion && renderedImageUnset(document.Image) {
		preview.Config = document
		preview.RenderedConfig = rendered
		preview.Diagnostics = append(preview.Diagnostics, config.Diagnostic{
			Severity: "warning",
			Code:     "image_required",
			Path:     "/image",
			Message:  "no project image or build was detected; enter a digest-pinned image before review",
		})
		preview.ProjectState, err = projectStateDigest(facts)
		if err != nil {
			return preview, model.Wrap(model.CodeInvalidInput, "snapshot setup project", err)
		}
		sortDiagnostics(preview.Diagnostics)
		return preview, nil
	}
	validated, validationDiagnostics := config.ParseBytes(projectConfigPath, rendered)
	preview.Diagnostics = append(preview.Diagnostics, validationDiagnostics...)
	if diagnosticsHaveErrors(preview.Diagnostics) {
		sortDiagnostics(preview.Diagnostics)
		return preview, invalidDiagnosticsError(preview.Diagnostics)
	}
	preview.Config = validated.Document
	preview.RenderedConfig = rendered
	preview.ConfigContentDigest = validated.ContentDigest

	imported, importDiagnostics := selectedDevcontainerImports(validated, facts)
	preview.Diagnostics = append(preview.Diagnostics, importDiagnostics...)
	if diagnosticsHaveErrors(preview.Diagnostics) {
		sortDiagnostics(preview.Diagnostics)
		return preview, invalidDiagnosticsError(preview.Diagnostics)
	}
	authority, err := collectAuthorityInputs(facts.WorkspaceRoot, validated, imported, facts, service.inspection.resolveHostMount)
	if err != nil {
		return preview, model.Wrap(model.CodeInvalidInput, "resolve setup authority inputs", err)
	}
	preview.ImportedContentDigests = make([]state.ContentDigest, len(authority.ImportedContent))
	for index, content := range authority.ImportedContent {
		preview.ImportedContentDigests[index] = state.ContentDigest{Path: content.Path, Digest: content.Digest}
	}
	projectID, err := model.NewProjectID(facts.WorkspaceRoot)
	if err != nil {
		return preview, model.Wrap(model.CodeInvalidInput, "derive project identity", err)
	}
	resolved, resolveDiagnostics, err := service.inspection.resolver.Resolve(ctx, plan.ResolveInput{
		Config:  validated,
		Project: plan.ProjectIdentity{ID: projectID, CanonicalRoot: facts.WorkspaceRoot},
		Sandbox: plan.SandboxIdentity{Name: model.SandboxName("main")},
		Mode:    model.ModeLive,
		Ownership: plan.OwnershipPlan{
			Labels:       []plan.KeyValue{{Key: "dsx.project", Value: string(projectID)}, {Key: "dsx.sandbox", Value: "main"}},
			ResourceName: "dsx-" + string(projectID) + "-main",
		},
		Imported:  imported,
		Defaults:  plan.DefaultValues{Agent: "codex", Internet: true, CPUs: 2, MemoryBytes: 2 << 30, MaxConcurrentClones: 1},
		Authority: authority,
	})
	preview.Diagnostics = append(preview.Diagnostics, resolveDiagnostics...)
	if err != nil {
		return preview, model.Wrap(model.CodeInvalidInput, "resolve setup plan", err)
	}
	preview.Plan = resolved
	preview.Hash = resolved.ExecutableHash
	preview.SelectedCapabilities = selectedCapabilities(resolved)
	preview.ProjectState, err = projectStateDigest(facts)
	if err != nil {
		return preview, model.Wrap(model.CodeInvalidInput, "snapshot setup project", err)
	}
	sortDiagnostics(preview.Diagnostics)
	return preview, nil
}

// PreviewExisting reads and resolves the committed project configuration
// without synthesizing setup defaults or writing approval state.
func (service *SetupService) PreviewExisting(ctx context.Context, request BareStateRequest) (SetupPreview, error) {
	if ctx == nil {
		return SetupPreview{}, model.NewError(model.CodeInvalidInput, "existing configuration preview: context is nil", nil)
	}
	if service == nil || service.inspection == nil {
		return SetupPreview{}, model.NewError(model.CodeInternal, "existing configuration preview service is not configured", nil)
	}
	inspected, err := service.inspection.Inspect(ctx, InspectRequest{
		Root: request.Root, Mode: string(model.ModeLive), SandboxName: string(liveSandboxName),
	})
	if err != nil {
		return SetupPreview{}, err
	}
	if !inspected.Facts.ConfigExists {
		return SetupPreview{}, model.NewError(model.CodeInvalidInput, "project configuration does not exist", nil)
	}
	configPath := filepath.Join(inspected.Facts.CanonicalRoot, filepath.FromSlash(projectConfigPath))
	file, err := os.Open(configPath)
	if err != nil {
		return SetupPreview{}, model.Wrap(model.CodeInvalidInput, "open existing project configuration", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return SetupPreview{}, model.Wrap(model.CodeInvalidInput, "inspect existing project configuration", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > config.MaxConfigBytes {
		return SetupPreview{}, model.NewError(model.CodeInvalidInput, "existing project configuration must be a bounded regular file", nil)
	}
	rendered, err := io.ReadAll(io.LimitReader(file, config.MaxConfigBytes+1))
	if err != nil {
		return SetupPreview{}, model.Wrap(model.CodeInvalidInput, "read existing project configuration", err)
	}
	if int64(len(rendered)) > config.MaxConfigBytes {
		return SetupPreview{}, model.NewError(model.CodeInvalidInput, "existing project configuration exceeds the size limit", nil)
	}
	preview, err := service.PreviewSetup(ctx, SetupPreviewRequest{Root: request.Root, RenderedConfig: rendered})
	if err != nil {
		return SetupPreview{}, err
	}
	if preview.Hash != inspected.Plan.ExecutableHash {
		return SetupPreview{}, model.NewError(model.CodeUnapproved, "project configuration changed during launcher review", nil)
	}
	preview.Facts = inspected.Facts
	return preview, nil
}

// PreviewClone builds the exact named-clone projection while retaining the
// current configuration and project-state evidence used by the review UI.
func (service *SetupService) PreviewClone(ctx context.Context, request ClonePreviewRequest) (SetupPreview, error) {
	base, err := service.PreviewExisting(ctx, BareStateRequest{Root: request.Root})
	if err != nil {
		return SetupPreview{}, err
	}
	if service == nil || service.inspection == nil {
		return SetupPreview{}, model.NewError(model.CodeInternal, "clone preview service is not configured", nil)
	}
	var browser *bool
	if request.Browser {
		enabled := true
		browser = &enabled
	}
	inspected, err := service.inspection.Inspect(ctx, InspectRequest{
		Root: request.Root, Mode: string(model.ModeClone), SandboxName: request.Sandbox,
		CLIOverrides: CLIOverrides{Agent: request.Agent, Browser: browser},
	})
	if err != nil {
		return SetupPreview{}, err
	}
	base.Facts = inspected.Facts
	base.Diagnostics = inspected.Diagnostics
	base.Plan = inspected.Plan
	base.Hash = inspected.Plan.ExecutableHash
	base.SelectedCapabilities = selectedCapabilities(inspected.Plan)
	sortDiagnostics(base.Diagnostics)
	return base, nil
}

func (service *SetupService) Initialize(ctx context.Context, request InitializeRequest) (InitializeResult, error) {
	if ctx == nil {
		return InitializeResult{}, model.NewError(model.CodeInvalidInput, "initialize: context is nil", nil)
	}
	if !request.Confirmed {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "setup requires explicit final confirmation", nil)
	}
	if service == nil || service.approvals == nil {
		return InitializeResult{}, model.NewError(model.CodeInternal, "setup approval repository is not configured", nil)
	}
	preview, err := service.PreviewSetup(ctx, SetupPreviewRequest{Root: request.Root, Config: request.Config, RenderedConfig: request.RenderedConfig})
	if err != nil {
		return InitializeResult{}, err
	}
	if request.ExpectedHash == "" || request.ExpectedHash != preview.Hash {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "setup executable hash changed since preview", nil)
	}
	if request.ExpectedConfigDigest == "" || request.ExpectedConfigDigest != preview.ConfigContentDigest {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "setup rendered configuration changed since preview", nil)
	}
	if request.ExpectedProjectState == "" || request.ExpectedProjectState != preview.ProjectState {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "project state changed since setup preview", nil)
	}
	if !contentDigestsEqual(request.ExpectedImportedContentDigests, preview.ImportedContentDigests) {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "setup imported content changed since preview", nil)
	}
	projectID, err := model.NewProjectID(preview.Facts.CanonicalRoot)
	if err != nil {
		return InitializeResult{}, model.Wrap(model.CodeInvalidInput, "derive project identity", err)
	}
	priorApproval, priorApprovalFound, err := service.approvals.LoadApproval(ctx, projectID)
	if err != nil {
		return InitializeResult{}, model.Wrap(model.CodeInternal, "load prior setup approval", err)
	}
	record := state.ApprovalRecord{
		Version:                state.ApprovalRecordVersion,
		ProjectID:              projectID,
		Hash:                   preview.Hash,
		ApprovedAt:             service.now().UTC(),
		DSXVersion:             service.version,
		ConfigContentDigest:    preview.ConfigContentDigest,
		ImportedContentDigests: append([]state.ContentDigest(nil), preview.ImportedContentDigests...),
	}
	if err := state.ValidateApprovalRecord(record); err != nil {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "cannot persist invalid setup approval", err)
	}
	configPath := filepath.Join(preview.Facts.CanonicalRoot, filepath.FromSlash(projectConfigPath))
	created, err := writeNewConfig(ctx, configPath, preview.RenderedConfig)
	if err != nil {
		return InitializeResult{}, err
	}
	if err := service.approvals.SaveApproval(ctx, record); err != nil {
		rollbackContext := context.WithoutCancel(ctx)
		var approvalRollbackErr error
		if priorApprovalFound {
			approvalRollbackErr = service.approvals.SaveApproval(rollbackContext, priorApproval)
		} else {
			approvalRollbackErr = service.approvals.DeleteApproval(rollbackContext, projectID)
		}
		configRollbackErr := os.Remove(configPath)
		if errors.Is(configRollbackErr, os.ErrNotExist) {
			configRollbackErr = nil
		}
		if approvalRollbackErr != nil || configRollbackErr != nil {
			return InitializeResult{}, model.Wrap(
				model.CodeInternal,
				"persist setup approval failed; rollback left recoverable residue",
				errors.Join(
					fmt.Errorf("save approval: %w", err),
					wrapRollbackError("restore prior approval", approvalRollbackErr),
					wrapRollbackError("remove new configuration", configRollbackErr),
				),
			)
		}
		return InitializeResult{}, err
	}
	return InitializeResult{ConfigPath: configPath, Hash: preview.Hash, Created: created}, nil
}

// ApproveExisting persists a reviewed, already-committed project configuration
// without rewriting the project file. It is the launcher trust transaction.
func (service *SetupService) ApproveExisting(ctx context.Context, request InitializeRequest) (InitializeResult, error) {
	if ctx == nil {
		return InitializeResult{}, model.NewError(model.CodeInvalidInput, "approve existing configuration: context is nil", nil)
	}
	if !request.Confirmed {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "existing configuration approval requires explicit final confirmation", nil)
	}
	if service == nil || service.approvals == nil {
		return InitializeResult{}, model.NewError(model.CodeInternal, "setup approval repository is not configured", nil)
	}
	preview, err := service.PreviewExisting(ctx, BareStateRequest{Root: request.Root})
	if err != nil {
		return InitializeResult{}, err
	}
	if request.ExpectedHash == "" || request.ExpectedHash != preview.Hash {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "executable hash changed since launcher review", nil)
	}
	if request.ExpectedConfigDigest == "" || request.ExpectedConfigDigest != preview.ConfigContentDigest {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "configuration changed since launcher review", nil)
	}
	if request.ExpectedProjectState == "" || request.ExpectedProjectState != preview.ProjectState {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "project state changed since launcher review", nil)
	}
	if !contentDigestsEqual(request.ExpectedImportedContentDigests, preview.ImportedContentDigests) {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "imported content changed since launcher review", nil)
	}
	projectID, err := model.NewProjectID(preview.Facts.CanonicalRoot)
	if err != nil {
		return InitializeResult{}, model.Wrap(model.CodeInvalidInput, "derive project identity", err)
	}
	record := state.ApprovalRecord{
		Version:                state.ApprovalRecordVersion,
		ProjectID:              projectID,
		Hash:                   preview.Hash,
		ApprovedAt:             service.now().UTC(),
		DSXVersion:             service.version,
		ConfigContentDigest:    preview.ConfigContentDigest,
		ImportedContentDigests: append([]state.ContentDigest(nil), preview.ImportedContentDigests...),
	}
	if err := state.AuthorizeApproval(ctx, service.approvals, state.ApprovalRequest{
		Mode: state.ApprovalModeInteractive, Record: record, FinalConfirmed: true,
	}); err != nil {
		return InitializeResult{}, err
	}
	configPath := filepath.Join(preview.Facts.CanonicalRoot, filepath.FromSlash(projectConfigPath))
	return InitializeResult{ConfigPath: configPath, Hash: preview.Hash, Created: false}, nil
}

func contentDigestsEqual(left, right []state.ContentDigest) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func wrapRollbackError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func suggestConfig(facts projectinspect.Facts) config.ConfigDocument {
	document := config.ConfigDocument{
		SchemaVersion: 1,
		Workspace:     config.WorkspaceConfig{Root: "."},
		Agents:        config.AgentConfig{Default: "codex", Allowed: []string{"codex"}},
	}
	if len(facts.Containerfiles) > 0 {
		document.Image = config.ImageConfig{Build: &config.ImageBuild{Context: ".", File: facts.Containerfiles[0]}}
		return document
	}
	for _, devcontainer := range facts.DevContainers {
		switch {
		case devcontainer.Image != "":
			document.Image = config.ImageConfig{Ref: devcontainer.Image}
			return document
		case devcontainer.Build.Dockerfile != "":
			contextPath := devcontainer.Build.Context
			if contextPath == "" {
				contextPath = "."
			}
			document.Image = config.ImageConfig{Build: &config.ImageBuild{Context: contextPath, File: devcontainer.Build.Dockerfile}}
			return document
		}
	}
	return document
}

func renderedImageUnset(image config.ImageConfig) bool {
	return image.Ref == "" && image.Build == nil
}

func selectedCapabilities(resolved plan.ExecutionPlan) []string {
	capabilities := []string{"workspace"}
	if len(resolved.Setup) > 0 || len(resolved.Processes) > 0 {
		capabilities = append(capabilities, "commands")
	}
	if len(resolved.Mounts) > 0 || len(resolved.Volumes) > 0 {
		capabilities = append(capabilities, "storage")
	}
	if len(resolved.Auth) > 0 {
		capabilities = append(capabilities, "credentials")
	}
	if resolved.Browser != nil {
		capabilities = append(capabilities, "browser")
	}
	if len(resolved.Bridges) > 0 {
		capabilities = append(capabilities, "network")
	}
	if len(resolved.Ports) > 0 {
		capabilities = append(capabilities, "ports")
	}
	return capabilities
}

func projectStateDigest(facts projectinspect.Facts) (string, error) {
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	if err := encoder.Encode(mapProjectFacts(facts)); err != nil {
		return "", err
	}
	paths := []string{projectConfigPath}
	paths = append(paths, facts.GitRoots...)
	paths = append(paths, facts.Containerfiles...)
	for _, devenv := range facts.Devenv {
		paths = append(paths, devenv.Path)
	}
	for _, lockfile := range facts.Lockfiles {
		paths = append(paths, lockfile.Path)
	}
	for _, devcontainer := range facts.DevContainers {
		paths = append(paths, devcontainer.Path)
	}
	sort.Strings(paths)
	for _, relative := range paths {
		_, _ = io.WriteString(hash, relative+"\x00")
		absolute := filepath.Join(facts.WorkspaceRoot, filepath.FromSlash(relative))
		data, err := os.ReadFile(absolute)
		if errors.Is(err, os.ErrNotExist) {
			_, _ = io.WriteString(hash, "missing\x00")
			continue
		}
		if err != nil {
			info, statErr := os.Lstat(absolute)
			if statErr == nil && info.IsDir() {
				_, _ = io.WriteString(hash, "directory\x00")
				continue
			}
			return "", err
		}
		_, _ = hash.Write(data)
		_, _ = io.WriteString(hash, "\x00")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeNewConfig(ctx context.Context, destination string, data []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, model.Wrap(model.CodeUnavailable, "write setup configuration", err)
	}
	directory := filepath.Dir(destination)
	if err := ensureRealPrivateDirectory(directory); err != nil {
		return false, model.Wrap(model.CodeInternal, "create project configuration directory", err)
	}
	if _, err := os.Lstat(destination); err == nil {
		return false, model.NewError(model.CodeInvalidInput, "project configuration already exists", nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, model.Wrap(model.CodeInvalidInput, "inspect project configuration destination", err)
	}
	temporary, err := os.CreateTemp(directory, ".config.jsonc-*")
	if err != nil {
		return false, model.Wrap(model.CodeInternal, "create temporary project configuration", err)
	}
	temporaryPath := temporary.Name()
	cleanup := true
	defer func() {
		_ = temporary.Close()
		if cleanup {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return false, model.Wrap(model.CodeInternal, "set project configuration permissions", err)
	}
	if _, err := io.Copy(temporary, bytes.NewReader(data)); err != nil {
		return false, model.Wrap(model.CodeInternal, "write project configuration", err)
	}
	if err := temporary.Sync(); err != nil {
		return false, model.Wrap(model.CodeInternal, "sync project configuration", err)
	}
	if err := temporary.Close(); err != nil {
		return false, model.Wrap(model.CodeInternal, "close project configuration", err)
	}
	if err := ctx.Err(); err != nil {
		return false, model.Wrap(model.CodeUnavailable, "write setup configuration", err)
	}
	if err := os.Link(temporaryPath, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, model.NewError(model.CodeInvalidInput, "project configuration appeared after setup preview", err)
		}
		return false, model.Wrap(model.CodeInternal, "install project configuration", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		_ = os.Remove(destination)
		return false, model.Wrap(model.CodeInternal, "remove temporary project configuration", err)
	}
	cleanup = false
	parent, err := os.Open(directory)
	if err != nil {
		_ = os.Remove(destination)
		return false, model.Wrap(model.CodeInternal, "open project configuration directory", err)
	}
	syncErr := parent.Sync()
	closeErr := parent.Close()
	if syncErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		return false, model.Wrap(model.CodeInternal, "sync project configuration directory", errors.Join(syncErr, closeErr))
	}
	return true, nil
}

func ensureRealPrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a real directory", path)
	}
	return os.Chmod(path, 0o700)
}
