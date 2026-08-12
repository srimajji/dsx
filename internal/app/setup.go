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
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/srimajji/dsx/internal/buildinfo"
	"github.com/srimajji/dsx/internal/config"
	projectinspect "github.com/srimajji/dsx/internal/inspect"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

const (
	DefaultWorkspaceCPUs        = 6
	DefaultWorkspaceMemoryBytes = int64(6 << 30)
	DefaultWorkspaceMemory      = "6GiB"
)

// OwnedResourceInventory is the read-only project state port used to choose a
// bare-command screen. Implementations read DSX manifests; they must not query
// or mutate the runtime.
type OwnedResourceInventory interface {
	CountOwnedResources(context.Context, model.ProjectID) (int, error)
}

type StandardImagePreparer interface {
	PrepareStandardImage(context.Context, plan.ExecutionPlan) error
}
type ContainerSystemController interface {
	CheckSystemStatus(context.Context) error
	Status(context.Context) (runtime.SystemStatus, error)
	StartSystem(context.Context) error
}

type SetupDependencies struct {
	Inspection      *InspectionService
	Approvals       state.ApprovalRepository
	Inventory       OwnedResourceInventory
	ContainerSystem ContainerSystemController
	ImagePreparer   StandardImagePreparer
	Now             func() time.Time
	DSXVersion      string
	StandardImage   string
}

// SetupService owns the final-confirmation transaction. PreviewSetup is
// read-only; Initialize is the only setup path allowed to write configuration
// and approval state.
type SetupService struct {
	inspection      *InspectionService
	approvals       state.ApprovalRepository
	inventory       OwnedResourceInventory
	containerSystem ContainerSystemController
	imagePreparer   StandardImagePreparer
	now             func() time.Time
	version         string
	standardImage   string
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
	if dependencies.StandardImage == "" {
		dependencies.StandardImage = buildinfo.Current().AgentImage
	}
	return &SetupService{
		inspection:      dependencies.Inspection,
		approvals:       dependencies.Approvals,
		inventory:       dependencies.Inventory,
		containerSystem: dependencies.ContainerSystem,
		imagePreparer:   dependencies.ImagePreparer,
		now:             dependencies.Now,
		version:         dependencies.DSXVersion,
		standardImage:   dependencies.StandardImage,
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
	location, configExists, err := service.inspection.activeConfig(facts.WorkspaceRoot)
	if err != nil {
		return BareState{}, err
	}
	var configuredPorts []config.PortConfig
	if configExists {
		validated, diagnostics := service.inspection.parseConfig(location.absolute, location.display)
		if diagnosticsHaveErrors(diagnostics) {
			return BareState{}, invalidDiagnosticsError(diagnostics)
		}
		mapped.ConfigExists = true
		mapped.ConfigPath = location.display
		configuredPorts = append([]config.PortConfig(nil), validated.Document.Ports...)
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
	systemStatus := runtime.SystemStatus{
		State: runtime.SystemStateUnavailable, Remediation: "Run `dsx doctor` to inspect Apple Container.",
	}
	if service.containerSystem != nil {
		systemStatus, err = service.containerSystem.Status(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return BareState{}, ctxErr
			}
			systemStatus = runtime.SystemStatus{
				State: runtime.SystemStateUnavailable, Remediation: "Run `dsx doctor` to inspect Apple Container.",
			}
		}
	}
	screen := BareSetup
	if configExists || owned > 0 {
		screen = BareDashboard
	}
	return BareState{
		Screen: screen, ConfigExists: configExists, OwnedResources: owned, Facts: mapped,
		ContainerSystem: systemStatus, ConfiguredPorts: configuredPorts,
	}, nil
}
func (service *SetupService) StartContainerSystem(ctx context.Context) error {
	if ctx == nil {
		return model.NewError(model.CodeInvalidInput, "start container system: context is nil", nil)
	}
	if service == nil || service.containerSystem == nil {
		return model.NewError(model.CodeUnavailable, "Apple container system control is unavailable", nil)
	}
	status, err := service.containerSystem.Status(ctx)
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "read Apple container system status", err)
	}
	if status.State == runtime.SystemStateRunning {
		return nil
	}
	if err := service.containerSystem.StartSystem(ctx); err != nil {
		return model.Wrap(model.CodeUnavailable, "start Apple container system", err)
	}
	if err := service.containerSystem.CheckSystemStatus(ctx); err != nil {
		return model.Wrap(model.CodeUnavailable, "verify Apple container system after start", err)
	}
	return nil
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
	preview.ImageOptions = setupImageOptions(facts, service.standardImage)
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
			document, preview.SelectedImageOption = suggestConfig(facts, preview.ImageOptions)
		}
		rendered, err = json.MarshalIndent(document, "", "  ")
		if err != nil {
			return preview, model.Wrap(model.CodeInvalidInput, "render setup configuration", err)
		}
		rendered = append(rendered, '\n')
	}
	if preview.SelectedImageOption == "" {
		preview.SelectedImageOption = selectedSetupImageOption(document.Image, preview.ImageOptions)
	}
	if generatedSuggestion && renderedImageUnset(document.Image) {
		preview.Config = document
		preview.RenderedConfig = rendered
		preview.Diagnostics = append(preview.Diagnostics, config.Diagnostic{
			Severity: "warning",
			Code:     "image_required",
			Path:     "/image",
			Message:  "no usable standard or detected project image is available; select a custom digest-pinned image before review",
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

	authority, err := collectAuthorityInputs(facts.WorkspaceRoot, validated, nil, service.inspection.resolveHostMount)
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
		Defaults: plan.DefaultValues{
			DefaultAgent:            "codex",
			Internet:                true,
			CPUs:                    DefaultWorkspaceCPUs,
			MemoryBytes:             DefaultWorkspaceMemoryBytes,
			MaxConcurrentWorkspaces: 1,
		},
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
	inspected, err := service.inspection.Inspect(ctx, InspectRequest{Root: request.Root})
	if err != nil {
		return SetupPreview{}, err
	}
	if !inspected.Facts.ConfigExists {
		return SetupPreview{}, model.NewError(model.CodeInvalidInput, "project configuration does not exist", nil)
	}
	location, found, err := service.inspection.activeConfig(inspected.Facts.CanonicalRoot)
	if err != nil {
		return SetupPreview{}, err
	}
	if !found || location.display != inspected.Facts.ConfigPath {
		return SetupPreview{}, model.NewError(model.CodeConflict, "active DSX configuration changed during launcher review", nil)
	}
	file, err := os.Open(location.absolute)
	if err != nil {
		return SetupPreview{}, model.Wrap(model.CodeInvalidInput, "open existing DSX configuration", err)
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
	if err := service.checkContainerSystem(ctx); err != nil {
		return InitializeResult{}, err
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
	local, shared, err := service.inspection.configLocations(preview.Facts.CanonicalRoot)
	if err != nil {
		return InitializeResult{}, err
	}
	configLocation := local
	if configLocation.absolute == "" {
		configLocation = shared
	}
	if _, found, locateErr := service.inspection.activeConfig(preview.Facts.CanonicalRoot); locateErr != nil {
		return InitializeResult{}, locateErr
	} else if found {
		return InitializeResult{}, model.NewError(model.CodeInvalidInput, "a DSX configuration appeared after setup preview", nil)
	}
	configPath := configLocation.absolute
	created, err := writeNewConfig(ctx, configPath, preview.RenderedConfig)
	if err != nil {
		return InitializeResult{}, err
	}
	if active, found, locateErr := service.inspection.activeConfig(preview.Facts.CanonicalRoot); locateErr != nil || !found || active.absolute != configPath {
		_ = os.Remove(configPath)
		if locateErr != nil {
			return InitializeResult{}, locateErr
		}
		return InitializeResult{}, model.NewError(model.CodeConflict, "DSX configuration selection changed while setup was saved", nil)
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
	result := InitializeResult{ConfigPath: configPath, Hash: preview.Hash, Created: created}
	if preview.Plan.Image.Standard && service.imagePreparer != nil {
		if err := service.imagePreparer.PrepareStandardImage(ctx, preview.Plan); err != nil {
			return result, model.Wrap(model.CodeUnavailable, "prepare DSX Standard image after saving setup", err)
		}
	}
	return result, nil
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
	if err := service.checkContainerSystem(ctx); err != nil {
		return InitializeResult{}, err
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
	location, found, err := service.inspection.activeConfig(preview.Facts.CanonicalRoot)
	if err != nil {
		return InitializeResult{}, err
	}
	if !found {
		return InitializeResult{}, model.NewError(model.CodeConflict, "approved DSX configuration disappeared", nil)
	}
	return InitializeResult{ConfigPath: location.absolute, Hash: preview.Hash, Created: false}, nil
}

// UpdateExisting atomically replaces only the published-port portion of a
// reviewed configuration and persists approval for the resulting plan.
func (service *SetupService) UpdateExisting(ctx context.Context, request InitializeRequest) (InitializeResult, error) {
	if ctx == nil {
		return InitializeResult{}, model.NewError(model.CodeInvalidInput, "update existing configuration: context is nil", nil)
	}
	if !request.Confirmed {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "port reconfiguration requires explicit final confirmation", nil)
	}
	if service == nil || service.approvals == nil {
		return InitializeResult{}, model.NewError(model.CodeInternal, "setup approval repository is not configured", nil)
	}
	current, err := service.PreviewExisting(ctx, BareStateRequest{Root: request.Root})
	if err != nil {
		return InitializeResult{}, err
	}
	if request.ReplacesConfigDigest == "" || request.ReplacesConfigDigest != current.ConfigContentDigest {
		return InitializeResult{}, model.NewError(model.CodeConflict, "project configuration changed since port editing began", nil)
	}
	candidate, err := service.PreviewSetup(ctx, SetupPreviewRequest{Root: request.Root, RenderedConfig: request.RenderedConfig})
	if err != nil {
		return InitializeResult{}, err
	}
	if request.ExpectedHash == "" || request.ExpectedHash != candidate.Hash ||
		request.ExpectedConfigDigest == "" || request.ExpectedConfigDigest != candidate.ConfigContentDigest ||
		request.ExpectedProjectState == "" || request.ExpectedProjectState != candidate.ProjectState {
		return InitializeResult{}, model.NewError(model.CodeUnapproved, "port configuration changed since final review", nil)
	}
	currentDocument, candidateDocument := current.Config, candidate.Config
	currentDocument.Ports = nil
	candidateDocument.Ports = nil
	if !reflect.DeepEqual(currentDocument, candidateDocument) {
		return InitializeResult{}, model.NewError(model.CodeInvalidInput, "port reconfiguration may change only published ports", nil)
	}
	for _, port := range candidate.Config.Ports {
		if !port.Host.Dynamic || port.Host.Port != 0 || (port.Bind != "" && port.Bind != "127.0.0.1") || (port.Protocol != "" && port.Protocol != "tcp") {
			return InitializeResult{}, model.NewError(model.CodeInvalidInput, "interactive port configuration supports only dynamic loopback TCP publications", nil)
		}
	}
	if err := service.checkContainerSystem(ctx); err != nil {
		return InitializeResult{}, err
	}
	projectID, err := model.NewProjectID(candidate.Facts.CanonicalRoot)
	if err != nil {
		return InitializeResult{}, model.Wrap(model.CodeInvalidInput, "derive project identity", err)
	}
	priorApproval, priorApprovalFound, err := service.approvals.LoadApproval(ctx, projectID)
	if err != nil {
		return InitializeResult{}, model.Wrap(model.CodeInternal, "load prior setup approval", err)
	}
	location, found, err := service.inspection.activeConfig(candidate.Facts.CanonicalRoot)
	if err != nil {
		return InitializeResult{}, err
	}
	if !found {
		return InitializeResult{}, model.NewError(model.CodeConflict, "project configuration disappeared during port review", nil)
	}
	if err := replaceExistingConfig(ctx, location.absolute, current.ConfigContentDigest, candidate.RenderedConfig); err != nil {
		return InitializeResult{}, err
	}
	record := state.ApprovalRecord{
		Version: state.ApprovalRecordVersion, ProjectID: projectID, Hash: candidate.Hash,
		ApprovedAt: service.now().UTC(), DSXVersion: service.version,
		ConfigContentDigest: candidate.ConfigContentDigest,
	}
	if err := service.approvals.SaveApproval(ctx, record); err != nil {
		rollbackContext := context.WithoutCancel(ctx)
		configRollbackErr := replaceExistingConfig(rollbackContext, location.absolute, candidate.ConfigContentDigest, current.RenderedConfig)
		var approvalRollbackErr error
		if priorApprovalFound {
			approvalRollbackErr = service.approvals.SaveApproval(rollbackContext, priorApproval)
		} else {
			approvalRollbackErr = service.approvals.DeleteApproval(rollbackContext, projectID)
		}
		if configRollbackErr != nil || approvalRollbackErr != nil {
			return InitializeResult{}, model.Wrap(model.CodeInternal, "persist port approval failed; rollback left recoverable residue",
				errors.Join(err, wrapRollbackError("restore configuration", configRollbackErr), wrapRollbackError("restore approval", approvalRollbackErr)))
		}
		return InitializeResult{}, err
	}
	return InitializeResult{ConfigPath: location.absolute, Hash: candidate.Hash, Created: false}, nil
}
func (service *SetupService) checkContainerSystem(ctx context.Context) error {
	if service.containerSystem == nil {
		return model.NewError(model.CodeUnavailable, "Apple container system status check is not configured", nil)
	}
	if err := service.containerSystem.CheckSystemStatus(ctx); err != nil {
		return model.Wrap(model.CodeUnavailable, "check Apple container system after final confirmation", err)
	}
	return nil
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

func suggestConfig(facts projectinspect.Facts, options []SetupImageOption) (config.ConfigDocument, string) {
	document := config.ConfigDocument{
		SchemaVersion: 1,
		Workspace:     config.WorkspaceConfig{Root: "."},
		Agents:        config.AgentConfig{Default: "codex", Allowed: []string{"codex"}},
		Resources:     config.ResourceLimits{CPUs: DefaultWorkspaceCPUs, Memory: DefaultWorkspaceMemory},
	}
	for _, option := range options {
		if option.Available && option.ID != "custom" {
			document.Image = option.Image
			return document, option.ID
		}
	}
	return document, "custom"
}

func setupImageOptions(facts projectinspect.Facts, standardImage string) []SetupImageOption {
	options := make([]SetupImageOption, 0, len(facts.Containerfiles)+2)
	_, publishedStandard := pinnedImageDigest(standardImage)
	standardDescription := "Built locally on first use with Codex, Claude, OMP, and OpenCode"
	standardConfig := config.ImageConfig{Standard: true}
	if publishedStandard {
		standardDescription = "Ready to use with Codex, Claude, OMP, and OpenCode"
		standardConfig = config.ImageConfig{Ref: standardImage}
	}
	options = append(options, SetupImageOption{
		ID: "standard", Name: "DSX Standard — Ubuntu (Recommended)", Description: standardDescription, Available: true,
		Image: standardConfig,
	})
	for _, candidate := range facts.Containerfiles {
		location := "Detected in the project root"
		if filepath.ToSlash(filepath.Dir(candidate)) != "." {
			location = "Detected at " + candidate
		}
		options = append(options, SetupImageOption{
			ID: "dockerfile:" + candidate, Name: "Use this project's " + filepath.Base(candidate),
			Description: location, Available: true,
			Image: config.ImageConfig{Build: &config.ImageBuild{Context: ".", File: candidate}},
		})
	}
	options = append(options, SetupImageOption{
		ID: "custom", Name: "Use another image", Description: "Advanced", Available: true,
	})
	return options
}

func selectedSetupImageOption(image config.ImageConfig, options []SetupImageOption) string {
	for _, option := range options {
		if option.ID == "custom" {
			continue
		}
		if image.Standard && option.Image.Standard {
			return option.ID
		}
		if image.Ref != "" && image.Ref == option.Image.Ref {
			return option.ID
		}
		if image.Build != nil && option.Image.Build != nil &&
			image.Build.Context == option.Image.Build.Context &&
			image.Build.File == option.Image.Build.File &&
			image.Build.Target == option.Image.Build.Target &&
			maps.Equal(image.Build.Args, option.Image.Build.Args) {
			return option.ID
		}
	}
	return "custom"
}

func renderedImageUnset(image config.ImageConfig) bool {
	return image.Ref == "" && image.Build == nil && !image.Standard
}

func selectedCapabilities(resolved plan.ExecutionPlan) []string {
	capabilities := []string{"workspace"}
	if len(resolved.Setup) > 0 || len(resolved.Processes) > 0 {
		capabilities = append(capabilities, "commands")
	}
	if len(resolved.Mounts) > 0 || len(resolved.Volumes) > 0 {
		capabilities = append(capabilities, "storage")
	}
	if len(resolved.Auth.Imports) > 0 {
		capabilities = append(capabilities, "credentials")
	}
	if resolved.AWS.Mode == plan.AWSModeHostDefault {
		capabilities = append(capabilities, "aws")
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

func replaceExistingConfig(ctx context.Context, destination, expectedDigest string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "replace project configuration", err)
	}
	info, err := os.Lstat(destination)
	if err != nil {
		return model.Wrap(model.CodeConflict, "inspect project configuration before replacement", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > config.MaxConfigBytes {
		return model.NewError(model.CodeInvalidInput, "project configuration must remain a bounded regular non-symlink file", nil)
	}
	current, err := os.ReadFile(destination)
	if err != nil {
		return model.Wrap(model.CodeInvalidInput, "read project configuration before replacement", err)
	}
	digest := sha256.Sum256(current)
	if expectedDigest == "" || hex.EncodeToString(digest[:]) != expectedDigest {
		return model.NewError(model.CodeConflict, "project configuration changed before replacement", nil)
	}
	directory := filepath.Dir(destination)
	temporary, err := os.CreateTemp(directory, ".config.jsonc-update-*")
	if err != nil {
		return model.Wrap(model.CodeInternal, "create temporary project configuration update", err)
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
		return model.Wrap(model.CodeInternal, "set updated project configuration permissions", err)
	}
	if _, err := io.Copy(temporary, bytes.NewReader(data)); err != nil {
		return model.Wrap(model.CodeInternal, "write updated project configuration", err)
	}
	if err := temporary.Sync(); err != nil {
		return model.Wrap(model.CodeInternal, "sync updated project configuration", err)
	}
	if err := temporary.Close(); err != nil {
		return model.Wrap(model.CodeInternal, "close updated project configuration", err)
	}
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "replace project configuration", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return model.Wrap(model.CodeInternal, "install updated project configuration", err)
	}
	cleanup = false
	parent, err := os.Open(directory)
	if err != nil {
		return model.Wrap(model.CodeInternal, "open project configuration directory after replacement", err)
	}
	syncErr := parent.Sync()
	closeErr := parent.Close()
	if syncErr != nil || closeErr != nil {
		return model.Wrap(model.CodeInternal, "sync project configuration directory after replacement", errors.Join(syncErr, closeErr))
	}
	return nil
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
