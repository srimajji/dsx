package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/ownership"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/ports"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
	"github.com/srimajji/dsx/internal/terminal"
)

const (
	cloneGitIdentityName    = "DSX"
	cloneGitIdentityEmail   = "dsx@localhost"
	cloneGitTimestamp       = "2000-01-01T00:00:00Z"
	cloneWorkspaceVolume    = "clone-workspace"
	cloneWorkspaceVolumeDir = "/workspace"
)

type CloneDependencies struct {
	Lifecycle *LifecycleService
	Harness   *HarnessService
	Git       gitx.HostService
	TempRoot  string
}

type CloneService struct {
	lifecycle *LifecycleService
	harness   *HarnessService
	git       gitx.HostService
	tempRoot  string
}

type cloneVolumeIndexes struct {
	workspace  int
	owner      int
	browser    int
	configured map[string]int
}

func NewCloneService(dependencies CloneDependencies) (*CloneService, error) {
	if dependencies.Lifecycle == nil || dependencies.Harness == nil || dependencies.Git == nil {
		return nil, errors.New("clone lifecycle, harness, and host Git services are required")
	}
	if dependencies.TempRoot == "" {
		dependencies.TempRoot = os.TempDir()
	}
	root, err := filepath.Abs(dependencies.TempRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve clone temporary root: %w", err)
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect clone temporary root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("clone temporary root must be a non-symlink directory")
	}
	service := &CloneService{lifecycle: dependencies.Lifecycle, harness: dependencies.Harness, git: dependencies.Git, tempRoot: root}
	dependencies.Lifecycle.cloneCleanupRecovery = service.recoverCleanupCapture
	dependencies.Lifecycle.cloneCleanupFetchedVerifier = service.verifyCleanupFetchedResults
	dependencies.Lifecycle.cloneCleanupIdentityValidator = service.validateCleanupRepositoryIdentities

	return service, nil
}

func (service *CloneService) ready(ctx context.Context) error {
	if ctx == nil {
		return model.NewError(model.CodeInvalidInput, "clone context is nil", nil)
	}
	if service == nil || service.lifecycle == nil || service.harness == nil || service.git == nil {
		return model.NewError(model.CodeUnavailable, "clone service is unavailable", nil)
	}
	return service.lifecycle.ready(ctx)
}

func (service *CloneService) RunClone(ctx context.Context, request CloneRunRequest) (result CloneRunResult, returnErr error) {
	if err := service.ready(ctx); err != nil {
		return result, err
	}
	sandbox, err := model.ParseSandboxName(request.Sandbox)
	if err != nil || sandbox == liveSandboxName {
		return result, model.NewError(model.CodeInvalidInput, "clone sandbox must be a valid non-main name", err)
	}
	start := StartRequest{
		Root: request.Root, ApproveConfig: request.ApproveConfig, Agent: request.Agent,
		Browser: browserEnableOverride(request.Browser), Sandbox: string(sandbox), Mode: model.ModeClone,
	}
	inspected, err := service.lifecycle.inspectApproved(ctx, start)
	if err != nil {
		return result, err
	}
	if browserEnabled(inspected.Plan) {
		if err := rejectPlaywrightMCP(request.MCPServers); err != nil {
			return result, err
		}
	}
	sandboxLease, err := service.lifecycle.locks.LockSandbox(ctx, inspected.Plan.Project.ID, sandbox)
	if err != nil {
		return result, model.Wrap(model.CodeConflict, "lock clone sandbox", err)
	}
	defer func() { returnErr = errors.Join(returnErr, sandboxLease.Unlock()) }()
	projectLock, err := service.lifecycle.locks.LockProject(ctx, inspected.Plan.Project.ID)
	if err != nil {
		return result, model.Wrap(model.CodeConflict, "lock clone lifecycle", err)
	}
	projectLocked := true
	unlockProject := func() error {
		if !projectLocked {
			return nil
		}
		projectLocked = false
		return projectLock.Unlock()
	}
	defer func() { returnErr = errors.Join(returnErr, unlockProject()) }()

	manifests, err := service.lifecycle.manifests.ListProjectManifests(ctx, inspected.Plan.Project.ID)
	if err != nil {
		return result, err
	}
	existing, err := validateCloneAdmission(manifests, sandbox, inspected.Plan.Limits.MaxConcurrentClones)
	if err != nil {
		return result, err
	}
	if existing != nil {
		current, err := service.lifecycle.revalidateApprovedPlan(ctx, start, inspected.Plan.ExecutableHash)
		if err != nil {
			return result, err
		}
		manifest := *existing
		current.Sandbox.RunID = manifest.RunID
		var hostBridges *hostBridgeSession
		snapshot, volumeIndexes, warnings, err := service.prepareCloneResume(ctx, current, &manifest, &hostBridges)
		if err != nil {
			return result, err
		}
		return service.executeCloneRun(ctx, request, current, snapshot, volumeIndexes, &manifest, warnings, hostBridges, func() {}, unlockProject)
	}

	artifacts, warnings, err := service.prepareSources(ctx, inspected.Plan, sandbox)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, service.removeSources(artifacts)) }()
	current, err := service.lifecycle.revalidateApprovedPlan(ctx, start, inspected.Plan.ExecutableHash)
	if err != nil {
		return result, err
	}
	capabilities, err := service.lifecycle.runtime.Probe(ctx)
	if err != nil {
		return result, err
	}
	if err := requireLifecycleCapabilities(current, capabilities); err != nil {
		return result, err
	}
	publication, err := ports.Plan(current.Ports, capabilities)
	if err != nil {
		return result, portPlanError(err)
	}
	defer func() { _ = publication.Abort() }()
	now := service.lifecycle.now().UTC()
	runID, err := service.lifecycle.newRunID(now)
	if err != nil {
		return result, model.Wrap(model.CodeInternal, "generate clone run ID", err)
	}
	current.Sandbox.RunID = runID
	manifest, volumeIndexes, err := plannedCloneManifest(current, runID, now, artifacts)
	if err != nil {
		return result, model.Wrap(model.CodeInvalidInput, "plan clone resource graph", err)
	}
	if err := service.lifecycle.manifests.CreateIntent(ctx, manifest); err != nil {
		return result, err
	}
	rollback := true
	defer func() {
		if !rollback {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), browserCleanupTimeout)
		defer cancel()
		if projectLocked {
			returnErr = errors.Join(returnErr, service.lifecycle.rollbackCreate(cleanupCtx, &manifest))
			return
		}
		cleanupLock, lockErr := service.lifecycle.locks.LockProject(cleanupCtx, manifest.ProjectID)
		if lockErr != nil {
			returnErr = errors.Join(returnErr, lockErr)
			return
		}
		returnErr = errors.Join(returnErr, service.lifecycle.rollbackCreate(cleanupCtx, &manifest), cleanupLock.Unlock())
	}()
	if err := service.lifecycle.transition(ctx, &manifest, model.StateCreating, "create", ""); err != nil {
		return result, err
	}
	var hostBridges *hostBridgeSession
	snapshot, err := service.createClone(ctx, start, current, artifacts, volumeIndexes, publication, &manifest, &hostBridges)
	if err != nil {
		return result, err
	}
	return service.executeCloneRun(ctx, request, current, snapshot, volumeIndexes, &manifest, warnings, hostBridges, func() { rollback = false }, unlockProject)
}

func validateCloneAdmission(manifests []state.Manifest, sandbox model.SandboxName, maximum int) (*state.Manifest, error) {
	activeClones := 0
	var existing *state.Manifest
	for _, manifest := range activeManifests(manifests) {
		if manifest.Mode == model.ModeLive {
			return nil, model.NewError(model.CodeConflict, "live and clone sandboxes cannot run concurrently for one project", nil)
		}
		if manifest.Mode != model.ModeClone {
			return nil, model.NewError(model.CodeAmbiguous, "active sandbox has an unsupported workspace mode", nil)
		}
		if manifest.Sandbox == sandbox {
			if existing != nil {
				return nil, model.NewError(model.CodeAmbiguous, "multiple active manifests claim the requested clone sandbox", nil)
			}
			copy := manifest
			existing = &copy
			continue
		}
		activeClones++
	}
	if existing != nil {
		switch existing.State {
		case model.StateRunning, model.StateStopped, model.StateFailed:
		case model.StateCreating:
			if !existing.UncapturedWork {
				return nil, model.NewError(model.CodeConflict, "clone creation is incomplete and cannot be resumed", nil)
			}
		default:
			return nil, model.NewError(model.CodeConflict, fmt.Sprintf("clone sandbox in state %s cannot be resumed", existing.State), nil)
		}
		return existing, nil
	}
	if maximum <= 0 {
		return nil, model.NewError(model.CodeInvalidInput, "maximum concurrent clone sandboxes must be positive", nil)
	}
	if activeClones >= maximum {
		return nil, model.NewError(model.CodeConflict, "maximum concurrent clone sandboxes reached", nil)
	}
	return nil, nil
}

func (service *CloneService) executeCloneRun(ctx context.Context, request CloneRunRequest, current plan.ExecutionPlan, snapshot runtime.ResourceSnapshot, indexes cloneVolumeIndexes, manifest *state.Manifest, warnings []string, hostBridges *hostBridgeSession, captureStarted func(), releaseProjectLock func() error) (result CloneRunResult, returnErr error) {
	var browserServer *harness.MCPServer
	browserCleanupArmed := false
	defer func() {
		if !browserCleanupArmed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), browserCleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, service.deleteBrowser(cleanupCtx, indexes.browser, manifest))
	}()
	var err error
	bridgesOpen := true
	defer func() {
		if bridgesOpen {
			returnErr = errors.Join(returnErr, hostBridges.Close())
		}
	}()
	if browserEnabled(current) {
		if err := service.resetCloneBrowserRecord(ctx, indexes.browser, manifest); err != nil {
			return result, err
		}
		browserServer, err = service.createBrowser(ctx, snapshot, current, indexes.browser, manifest)
		if err != nil {
			return result, err
		}
		browserCleanupArmed = true
	}
	harnessRequest := request
	if browserServer != nil {
		harnessRequest, err = injectPlaywrightMCP(request, *browserServer)
		if err != nil {
			return result, err
		}
	}
	if err := service.markCapturePending(ctx, manifest); err != nil {
		return result, err
	}
	captureStarted()
	if err := releaseProjectLock(); err != nil {
		return result, err
	}
	harnessResult, harnessErr := service.runHarness(ctx, snapshot, current, harnessRequest, hostBridges)
	bridgeErr := hostBridges.Close()
	bridgesOpen = false
	harnessErr = errors.Join(harnessErr, bridgeErr)
	if browserServer != nil {
		browserCleanupArmed = false
		cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), browserCleanupTimeout)
		defer cancelCleanup()
		cleanupLock, lockErr := service.lifecycle.locks.LockProject(cleanupCtx, manifest.ProjectID)
		if lockErr != nil {
			return result, errors.Join(harnessErr, model.Wrap(model.CodeConflict, "lock browser cleanup", lockErr))
		}
		cleanupErr := service.deleteBrowser(cleanupCtx, indexes.browser, manifest)
		if cleanupErr != nil {
			transitionErr := service.lifecycle.transition(cleanupCtx, manifest, model.StateFailed, "browser-cleanup", cleanupErr.Error())
			return result, errors.Join(harnessErr, cleanupErr, transitionErr, cleanupLock.Unlock())
		}
		if err := cleanupLock.Unlock(); err != nil {
			return result, errors.Join(harnessErr, err)
		}
	}
	captureCtx := ctx
	cancelCapture := func() {}
	if harnessErr != nil || ctx.Err() != nil {
		captureCtx, cancelCapture = context.WithTimeout(context.WithoutCancel(ctx), browserCleanupTimeout)
	}
	defer cancelCapture()
	statuses, captureErr := service.captureResults(captureCtx, snapshot, manifest)
	if captureErr != nil {
		finalizeCtx, cancelFinalize := context.WithTimeout(context.WithoutCancel(ctx), browserCleanupTimeout)
		defer cancelFinalize()
		finalizeLock, lockErr := service.lifecycle.locks.LockProject(finalizeCtx, manifest.ProjectID)
		var transitionErr, unlockErr error
		if lockErr == nil {
			transitionErr = service.lifecycle.transition(finalizeCtx, manifest, model.StateFailed, "capture", captureErr.Error())
			unlockErr = finalizeLock.Unlock()
		}
		return result, errors.Join(harnessErr, captureErr, lockErr, transitionErr, unlockErr)
	}
	finalizeLock, err := service.lifecycle.locks.LockProject(captureCtx, manifest.ProjectID)
	if err != nil {
		return result, errors.Join(harnessErr, err)
	}
	if err := service.finalizeCaptured(captureCtx, manifest, harnessErr); err != nil {
		return result, errors.Join(harnessErr, err, finalizeLock.Unlock())
	}
	unlockErr := finalizeLock.Unlock()
	urls, urlErr := ports.RenderURLs(publishedBindingsFromManifest(*manifest))
	if urlErr != nil {
		return result, errors.Join(harnessErr, urlErr, unlockErr)
	}
	result = CloneRunResult{
		ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID, State: manifest.State,
		Agent: harnessResult.Agent, Exit: harnessResult.Exit, Repositories: statuses, URLs: urls, Warnings: warnings,
	}
	return result, errors.Join(harnessErr, unlockErr)
}

func (service *CloneService) prepareCloneResume(ctx context.Context, current plan.ExecutionPlan, manifest *state.Manifest, activeHostBridges **hostBridgeSession) (snapshot runtime.ResourceSnapshot, indexes cloneVolumeIndexes, warnings []string, returnErr error) {
	var err error
	indexes, err = validateCloneResumeContract(current, *manifest)
	if err != nil {
		return runtime.ResourceSnapshot{}, indexes, nil, err
	}
	for _, record := range manifest.Git {
		repository := gitx.Repository{Name: record.Repository, HostPath: record.HostPath, GuestPath: record.GuestPath, Identity: record.Identity}
		if err := service.git.ValidateRepository(ctx, repository); err != nil {
			return runtime.ResourceSnapshot{}, indexes, nil, model.Wrap(model.CodeUnapproved, "existing clone repository identity changed; clean it before resuming", err)
		}
	}
	snapshot, err = service.inspectCloneResumeResources(ctx, current, *manifest, indexes)
	if err != nil {
		return runtime.ResourceSnapshot{}, indexes, nil, err
	}
	if !manifest.UncapturedWork {
		if err := service.verifyCloneResumeResultSafety(ctx, *manifest); err != nil {
			return runtime.ResourceSnapshot{}, indexes, nil, err
		}
	}
	mirrorIdentity := bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}
	leappMirrorSource, err := service.lifecycle.ensureLeappMirrorForPlan(ctx, current, mirrorIdentity)
	if err != nil {
		return runtime.ResourceSnapshot{}, indexes, nil, err
	}
	mirrorReady := false
	defer func() {
		if !mirrorReady && leappMirrorSource != "" {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			returnErr = errors.Join(returnErr, service.lifecycle.stopLeappMirror(cleanupCtx, mirrorIdentity))
		}
	}()
	resumeState := manifest.State
	restartedWorkspace := snapshot.State == "stopped"
	snapshot, err = service.cloneWorkspace(ctx, manifest, cloneWorkspaceAccess{RestartStopped: true})
	if err != nil {
		return runtime.ResourceSnapshot{}, indexes, nil, err
	}
	restartedSnapshot := snapshot
	rollbackRestart := restartedWorkspace
	defer func() {
		if returnErr == nil || !rollbackRestart {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		leaseErr := service.lifecycle.stopPersistentHostBridges(rollbackCtx, mirrorIdentity)
		stopErr := service.lifecycle.runtime.Stop(rollbackCtx, restartedSnapshot, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"})
		var transitionErr error
		if resumeState == model.StateStopped && manifest.State == model.StateRunning {
			transitionErr = service.lifecycle.transition(rollbackCtx, manifest, model.StateStopped, "stop", returnErr.Error())
		}
		if activeHostBridges != nil {
			*activeHostBridges = nil
		}
		returnErr = errors.Join(returnErr, leaseErr, stopErr, transitionErr)
	}()
	capabilities, err := service.lifecycle.runtime.Probe(ctx)
	if err != nil {
		return runtime.ResourceSnapshot{}, indexes, nil, err
	}
	publications := fallbackManifestBindings(*manifest, current, capabilities)
	if activeHostBridges == nil {
		return runtime.ResourceSnapshot{}, indexes, nil, model.NewError(model.CodeInternal, "host bridge session output is unavailable", nil)
	}
	current, hostBridges, _, err := service.lifecycle.ensurePersistentHostBridges(ctx, snapshot, current, mirrorIdentity, publications, true)
	if err != nil {
		return runtime.ResourceSnapshot{}, indexes, nil, err
	}
	*activeHostBridges = hostBridges
	if manifest.UncapturedWork {
		if err := service.recoverCleanupCapture(ctx, snapshot, manifest); err != nil {
			return runtime.ResourceSnapshot{}, indexes, nil, model.Wrap(model.CodeDataLoss, "recover uncertain clone result before resume", err)
		}
		if err := service.verifyCloneResumeResultSafety(ctx, *manifest); err != nil {
			return runtime.ResourceSnapshot{}, indexes, nil, err
		}
	}
	if guestGraphEnabled(current) {
		if service.lifecycle.guest == nil {
			return runtime.ResourceSnapshot{}, indexes, nil, model.NewError(model.CodeUnavailable, "guest process service is unavailable", nil)
		}
		if err := service.lifecycle.guest.Reconcile(ctx, snapshot); err != nil {
			return runtime.ResourceSnapshot{}, indexes, nil, model.Wrap(model.CodeUnavailable, "reconcile clone guest before resume", err)
		}
		started, err := service.lifecycle.guest.Start(ctx, snapshot, current, 0)
		if err != nil {
			return runtime.ResourceSnapshot{}, indexes, nil, model.Wrap(model.CodeUnavailable, "restart clone guest process graph", err)
		}
		if err := service.lifecycle.waitGuestReady(ctx, snapshot, started.Generation); err != nil {
			return runtime.ResourceSnapshot{}, indexes, nil, err
		}
	}
	warnings = make([]string, 0)
	for _, record := range manifest.Git {
		if record.WarnUntracked {
			warnings = append(warnings, record.Repository+": untracked files were not transferred")
		}
		if record.WarnIgnored {
			warnings = append(warnings, record.Repository+": ignored files were not transferred")
		}
	}

	mirrorReady = true
	rollbackRestart = false
	return snapshot, indexes, warnings, nil
}
func cloneResumeResultGuard(manifest state.Manifest) error {
	for _, record := range manifest.Git {
		if record.HasResultWork() && !record.ResultFetched() {
			return model.NewError(model.CodeDataLoss, "clone has an unfetched prior result; fetch it before resuming", nil)
		}
	}
	return nil
}

func (service *CloneService) verifyCloneResumeResultSafety(ctx context.Context, manifest state.Manifest) error {
	if err := cloneResumeResultGuard(manifest); err != nil {
		return err
	}
	return service.verifyCleanupFetchedResults(ctx, manifest)
}

func validateCloneResumeContract(current plan.ExecutionPlan, manifest state.Manifest) (cloneVolumeIndexes, error) {
	var empty cloneVolumeIndexes
	if manifest.ProjectID != current.Project.ID || manifest.CanonicalRoot != current.Project.CanonicalRoot ||
		manifest.Sandbox != current.Sandbox.Name || manifest.Mode != model.ModeClone {
		return empty, model.NewError(model.CodeAmbiguous, "clone manifest identity differs from the approved plan", nil)
	}
	if manifest.PlanHash != current.ExecutableHash {
		return empty, model.NewError(model.CodeUnapproved, "existing clone plan differs from the currently approved executable plan; clean it before resuming", nil)
	}
	if len(manifest.Git) != len(current.Repositories) {
		return empty, model.NewError(model.CodeUnapproved, "existing clone repository contract differs from the approved plan", nil)
	}
	for index, repository := range current.Repositories {
		record := manifest.Git[index]
		if record.Repository != repository.Name || record.HostPath != repository.HostPath || record.GuestPath != repository.GuestPath {
			return empty, model.NewError(model.CodeUnapproved, "existing clone source contract differs from the approved plan", nil)
		}
	}
	return cloneVolumeIndexesForManifest(current, manifest)
}

func cloneVolumeIndexesForManifest(current plan.ExecutionPlan, manifest state.Manifest) (cloneVolumeIndexes, error) {
	indexes := cloneVolumeIndexes{workspace: -1, owner: -1, browser: -1, configured: make(map[string]int, len(current.Volumes))}
	find := func(kind runtime.ResourceKind, role string) (int, error) {
		found := -1
		for index, record := range manifest.Resources {
			if record.Kind != string(kind) || record.Role != role {
				continue
			}
			if found >= 0 {
				return -1, model.NewError(model.CodeAmbiguous, fmt.Sprintf("clone manifest has duplicate %s resource role %q", kind, role), nil)
			}
			found = index
		}
		if found < 0 {
			return -1, model.NewError(model.CodeUnavailable, fmt.Sprintf("clone manifest is missing %s resource role %q", kind, role), nil)
		}
		return found, nil
	}
	var err error
	if indexes.workspace, err = find(runtime.ResourceVolume, volumeRole(cloneWorkspaceVolume)); err != nil {
		return indexes, err
	}
	if indexes.owner, err = find(runtime.ResourceWorkspace, workspaceRole); err != nil {
		return indexes, err
	}
	for _, volume := range current.Volumes {
		index, findErr := find(runtime.ResourceVolume, volumeRole(volume.Name))
		if findErr != nil {
			return indexes, findErr
		}
		indexes.configured[volume.Name] = index
	}
	if browserEnabled(current) {
		if indexes.browser, err = find(runtime.ResourceBrowser, browserRole); err != nil {
			return indexes, err
		}
	}
	return indexes, nil
}

func (service *CloneService) inspectCloneResumeResources(ctx context.Context, current plan.ExecutionPlan, manifest state.Manifest, indexes cloneVolumeIndexes) (runtime.ResourceSnapshot, error) {
	for index, record := range manifest.Resources {
		if index == indexes.browser && (record.Deleted || record.Absent || !record.Created) {
			continue
		}
		if !record.Created || record.Deleted || record.Absent || record.RuntimeID != record.ExpectedID {
			return runtime.ResourceSnapshot{}, model.NewError(model.CodeUnavailable, fmt.Sprintf("clone resource %q is not complete", record.Role), nil)
		}
		snapshot, found, err := service.lifecycle.findRuntimeResource(ctx, record)
		if err != nil {
			return runtime.ResourceSnapshot{}, err
		}
		if !found {
			return runtime.ResourceSnapshot{}, model.NewError(model.CodeUnavailable, fmt.Sprintf("clone resource %q is missing", record.Role), nil)
		}
		classification := ownership.Classify(&record, &snapshot)
		if !classification.DeleteAllowed {
			return runtime.ResourceSnapshot{}, model.NewError(model.CodeAmbiguous, classification.Reason, nil)
		}
	}
	workspaceRecord := manifest.Resources[indexes.owner]
	workspace, found, err := service.lifecycle.findRuntimeResource(ctx, workspaceRecord)
	if err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	if !found {
		return runtime.ResourceSnapshot{}, model.NewError(model.CodeUnavailable, "clone workspace disappeared during resume validation", nil)
	}
	network, err := manifestResource(manifest, runtime.ResourceNetwork, networkRole)
	if err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	volumes := map[string]string{cloneWorkspaceVolume: manifest.Resources[indexes.workspace].Name}
	for _, volume := range current.Volumes {
		volumes[volume.Name] = manifest.Resources[indexes.configured[volume.Name]].Name
	}
	helper, err := service.lifecycle.guestHelperForPlan(current)
	if err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	leappMirrorSource, err := service.lifecycle.leappMirrorPathForPlan(current, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID})
	if err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	capabilities, err := service.lifecycle.runtime.Probe(ctx)
	if err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	expectedPorts := portRequestsFromBindings(workspace.Ports)
	if ports.UsesFallback(current.Ports, capabilities) {
		expectedPorts = nil
	}
	expected, err := workspaceSpecForClone(current, runtime.Image{}, workspaceRecord, network.Name, volumes, expectedPorts, service.lifecycle.user(), helper, leappMirrorSource)
	if err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	if err := runtime.VerifyWorkspacePostcondition(workspace, expected); err != nil {
		return runtime.ResourceSnapshot{}, model.NewError(model.CodeAmbiguous, "runtime clone workspace grants differ from the approved manifest: "+err.Error(), nil)
	}
	if _, err := ports.ReconcileExisting(current.Ports, publishedBindingsFromManifest(manifest), workspace.Ports, capabilities); err != nil {
		return runtime.ResourceSnapshot{}, model.NewError(model.CodeAmbiguous, "runtime clone port bindings differ from the approved publication plan", err)
	}
	return workspace, nil
}

func (service *CloneService) resetCloneBrowserRecord(ctx context.Context, index int, manifest *state.Manifest) error {
	if index < 0 || index >= len(manifest.Resources) {
		return model.NewError(model.CodeInternal, "planned browser resource is missing", nil)
	}
	record := manifest.Resources[index]
	if !record.Deleted && !record.Absent {
		if record.Created {
			return model.NewError(model.CodeConflict, "previous clone browser resource is still active", nil)
		}
		return nil
	}
	next := *manifest
	next.Resources = append([]state.ResourceRecord(nil), manifest.Resources...)
	record = next.Resources[index]
	record.RuntimeID = ""
	record.Created = false
	record.Deleted = false
	record.Absent = false
	next.Resources[index] = record
	if err := service.lifecycle.replace(ctx, &next); err != nil {
		return err
	}
	*manifest = next
	return nil
}

func (service *CloneService) prepareSources(ctx context.Context, execution plan.ExecutionPlan, sandbox model.SandboxName) ([]gitx.SourceArtifact, []string, error) {
	artifacts := make([]gitx.SourceArtifact, 0, len(execution.Repositories))
	warnings := make([]string, 0)
	for _, repository := range execution.Repositories {
		artifact, err := service.git.PrepareSource(ctx, gitx.SourceRequest{Repository: gitRepository(repository), Sandbox: string(sandbox), TempRoot: service.tempRoot, ApprovedRoot: execution.Project.CanonicalRoot})
		if err != nil {
			return nil, nil, errors.Join(err, service.removeSources(artifacts))
		}
		if err := service.git.VerifyBundle(ctx, artifact.BundlePath, artifact.BundleDigest); err != nil {
			return nil, nil, errors.Join(err, service.git.RemoveArtifact(artifact.BundlePath), service.removeSources(artifacts))
		}
		artifacts = append(artifacts, artifact)
		if artifact.WarnUntracked {
			warnings = append(warnings, repository.Name+": untracked files were not transferred")
		}
		if artifact.WarnIgnored {
			warnings = append(warnings, repository.Name+": ignored files were not transferred")
		}
	}
	return artifacts, warnings, nil
}

func (service *CloneService) removeSources(artifacts []gitx.SourceArtifact) error {
	var result error
	for _, artifact := range artifacts {
		result = errors.Join(result, service.git.RemoveArtifact(artifact.BundlePath))
	}
	return result
}

func gitRepository(repository plan.RepositoryPlan) gitx.Repository {
	return gitx.Repository{Name: repository.Name, HostPath: repository.HostPath, GuestPath: repository.GuestPath}
}

func plannedCloneManifest(execution plan.ExecutionPlan, runID model.RunID, now time.Time, artifacts []gitx.SourceArtifact) (state.Manifest, cloneVolumeIndexes, error) {
	indexes := cloneVolumeIndexes{workspace: -1, owner: -1, browser: -1, configured: make(map[string]int, len(execution.Volumes))}
	if execution.Mode != model.ModeClone || execution.Sandbox.Name == liveSandboxName || len(artifacts) == 0 || len(artifacts) != len(execution.Repositories) {
		return state.Manifest{}, indexes, errors.New("expected a complete named clone plan")
	}
	manifest := state.Manifest{Version: state.ManifestVersion, Generation: 1, ProjectID: execution.Project.ID, CanonicalRoot: execution.Project.CanonicalRoot, Sandbox: execution.Sandbox.Name, RunID: runID, Mode: model.ModeClone, PlanHash: execution.ExecutableHash, State: model.StatePlanned, Operation: "create", CreatedAt: now.UTC(), UpdatedAt: now.UTC(), Git: make([]state.GitRecord, len(artifacts))}
	network, err := ownership.NewIdentity(manifest.ProjectID, manifest.Sandbox, manifest.RunID, runtime.ResourceNetwork, networkRole)
	if err != nil {
		return manifest, indexes, err
	}
	manifest.Resources = append(manifest.Resources, network.ManifestRecord())
	workspaceVolume, err := ownership.NewIdentity(manifest.ProjectID, manifest.Sandbox, manifest.RunID, runtime.ResourceVolume, volumeRole(cloneWorkspaceVolume))
	if err != nil {
		return manifest, indexes, err
	}
	indexes.workspace = len(manifest.Resources)
	manifest.Resources = append(manifest.Resources, workspaceVolume.ManifestRecord())
	for _, volume := range execution.Volumes {
		if volume.Target == cloneWorkspaceVolumeDir {
			return manifest, indexes, errors.New("configured volume cannot replace the clone workspace volume")
		}
		if _, duplicate := indexes.configured[volume.Name]; duplicate {
			return manifest, indexes, fmt.Errorf("duplicate volume %q", volume.Name)
		}
		identity, identityErr := ownership.NewIdentity(manifest.ProjectID, manifest.Sandbox, manifest.RunID, runtime.ResourceVolume, volumeRole(volume.Name))
		if identityErr != nil {
			return manifest, indexes, identityErr
		}
		record := identity.ManifestRecord()
		record.Persistent = volume.Persistent
		indexes.configured[volume.Name] = len(manifest.Resources)
		manifest.Resources = append(manifest.Resources, record)
	}
	workspace, err := ownership.NewIdentity(manifest.ProjectID, manifest.Sandbox, manifest.RunID, runtime.ResourceWorkspace, workspaceRole)
	if err != nil {
		return manifest, indexes, err
	}
	indexes.owner = len(manifest.Resources)
	manifest.Resources = append(manifest.Resources, workspace.ManifestRecord())
	if browserEnabled(execution) {
		browser, browserErr := ownership.NewIdentity(manifest.ProjectID, manifest.Sandbox, manifest.RunID, runtime.ResourceBrowser, browserRole)
		if browserErr != nil {
			return manifest, indexes, browserErr
		}
		indexes.browser = len(manifest.Resources)
		manifest.Resources = append(manifest.Resources, browser.ManifestRecord())
	}
	for index, artifact := range artifacts {
		manifest.Git[index] = state.GitRecord{Repository: artifact.Repository.Name, HostPath: artifact.Repository.HostPath, GuestPath: artifact.Repository.GuestPath, Identity: artifact.Repository.Identity, SourceRef: artifact.SourceRef, SourceCommit: artifact.SourceCommit, TrackedFingerprint: artifact.TrackedFingerprint, WarnUntracked: artifact.WarnUntracked, WarnIgnored: artifact.WarnIgnored, ResultBranch: "dsx/" + string(manifest.Sandbox), SourceBundleDigest: artifact.BundleDigest}
	}
	if err := state.ValidateManifest(manifest); err != nil {
		return manifest, indexes, err
	}
	return manifest, indexes, nil
}

func (service *CloneService) createClone(ctx context.Context, request StartRequest, approved plan.ExecutionPlan, artifacts []gitx.SourceArtifact, indexes cloneVolumeIndexes, publication *ports.PublicationPlan, manifest *state.Manifest, activeHostBridges **hostBridgeSession) (snapshot runtime.ResourceSnapshot, returnErr error) {
	helper, err := service.lifecycle.guestHelperForPlan(approved)
	if err != nil {
		return snapshot, err
	}
	buildRoot := ""
	var staged *config.ImageBuild
	if approved.Image.Reference == "" {
		build := config.ImageBuild{Context: approved.Image.Context, File: approved.Image.File}
		root, digest, stageErr := stageBuildInput(ctx, approved.Project.CanonicalRoot, build)
		if stageErr != nil || digest != approved.Image.InputDigest {
			return snapshot, model.NewError(model.CodeUnapproved, "build input changed while staging", stageErr)
		}
		buildRoot, staged = root, &build
		defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(root)) }()
	}
	imageSpec, err := imageSpecForPlan(approved, buildRoot)
	if err != nil {
		return snapshot, err
	}
	image, err := service.lifecycle.runtime.EnsureImage(ctx, imageSpec)
	if err != nil {
		return snapshot, err
	}
	if staged != nil {
		digest, digestErr := digestBuildInputInto(ctx, buildRoot, *staged, "")
		if digestErr != nil || digest != approved.Image.InputDigest {
			return snapshot, model.NewError(model.CodeUnapproved, "staged build input changed while consumed", digestErr)
		}
	}
	if err := service.lifecycle.createResource(ctx, manifest, 0, func(record state.ResourceRecord) (runtime.Resource, error) {
		return service.lifecycle.runtime.CreateNetwork(ctx, runtime.NetworkSpec{Name: record.Name, Labels: runtimeLabels(record.Labels)})
	}); err != nil {
		return snapshot, err
	}
	if err := service.lifecycle.createResource(ctx, manifest, indexes.workspace, func(record state.ResourceRecord) (runtime.Resource, error) {
		return service.lifecycle.runtime.CreateVolume(ctx, runtime.VolumeSpec{Name: record.Name, Labels: runtimeLabels(record.Labels)})
	}); err != nil {
		return snapshot, err
	}
	volumeNames := map[string]string{cloneWorkspaceVolume: manifest.Resources[indexes.workspace].Name}
	for _, volume := range approved.Volumes {
		index := indexes.configured[volume.Name]
		if err := service.lifecycle.createResource(ctx, manifest, index, func(record state.ResourceRecord) (runtime.Resource, error) {
			return service.lifecycle.runtime.CreateVolume(ctx, runtime.VolumeSpec{Name: record.Name, Labels: runtimeLabels(record.Labels)})
		}); err != nil {
			return snapshot, err
		}
		volumeNames[volume.Name] = manifest.Resources[index].Name
	}
	current, err := service.lifecycle.revalidateApprovedPlan(ctx, request, approved.ExecutableHash)
	if err != nil {
		return snapshot, err
	}
	current.Sandbox.RunID = manifest.RunID
	mirrorIdentity := bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}
	leappMirrorSource, err := service.lifecycle.ensureLeappMirrorForPlan(ctx, current, mirrorIdentity)
	if err != nil {
		return snapshot, err
	}
	if leappMirrorSource != "" {
		defer func() {
			if returnErr != nil {
				rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				returnErr = errors.Join(returnErr, service.lifecycle.stopLeappMirror(rollbackCtx, mirrorIdentity))
			}
		}()
	}
	runtimePorts, err := publication.ReleaseForCreate()
	if err != nil {
		return snapshot, err
	}
	workspaceIndex := indexes.owner
	spec, err := workspaceSpecForClone(current, image, manifest.Resources[workspaceIndex], manifest.Resources[0].Name, volumeNames, runtimePorts, service.lifecycle.user(), helper, leappMirrorSource)
	if err != nil {
		return snapshot, err
	}
	if err := service.lifecycle.createResource(ctx, manifest, workspaceIndex, func(state.ResourceRecord) (runtime.Resource, error) {
		return service.lifecycle.runtime.CreateWorkspace(ctx, spec)
	}); err != nil {
		return snapshot, err
	}
	revalidatedMirror, err := service.lifecycle.ensureLeappMirrorForPlan(ctx, current, mirrorIdentity)
	if err != nil {
		return snapshot, err
	}
	if revalidatedMirror != leappMirrorSource {
		return snapshot, model.NewError(model.CodeAmbiguous, "Leapp mirror path changed during clone workspace creation", nil)
	}
	snapshot, err = service.lifecycle.runtime.Inspect(ctx, runtime.ResourceID(manifest.Resources[workspaceIndex].ExpectedID))
	if err != nil {
		return snapshot, err
	}
	classification := ownership.Classify(&manifest.Resources[workspaceIndex], &snapshot)
	if !classification.DeleteAllowed {
		return snapshot, model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	if err := service.lifecycle.runtime.StartWorkspace(ctx, snapshot); err != nil {
		return snapshot, err
	}
	snapshot, err = service.lifecycle.runtime.Inspect(ctx, runtime.ResourceID(manifest.Resources[workspaceIndex].ExpectedID))
	if err != nil {
		return snapshot, err
	}
	classification = ownership.Classify(&manifest.Resources[workspaceIndex], &snapshot)
	if !classification.DeleteAllowed || snapshot.State != "running" {
		return snapshot, model.NewError(model.CodeAmbiguous, "started clone workspace does not match the owned running snapshot", nil)
	}
	relayBindings, err := publication.ReleaseForRelay()
	if err != nil {
		return snapshot, model.Wrap(model.CodeConflict, "release clone port reservations for host relay", err)
	}
	bindings, err := publication.Reconcile(snapshot.Ports)
	if err != nil {
		return snapshot, err
	}
	identity := bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}
	if activeHostBridges == nil {
		return snapshot, model.NewError(model.CodeInternal, "host bridge session output is unavailable", nil)
	}
	current, hostBridges, _, err := service.lifecycle.ensurePersistentHostBridges(ctx, snapshot, current, identity, relayBindings, true)
	if err != nil {
		return snapshot, err
	}
	*activeHostBridges = hostBridges
	if err := service.lifecycle.recordHostBindings(ctx, manifest, bindings); err != nil {
		return snapshot, err
	}
	if err := service.bootstrap(ctx, snapshot, manifest, artifacts); err != nil {
		return snapshot, err
	}
	if guestGraphEnabled(current) {
		if service.lifecycle.guest == nil {
			return snapshot, model.NewError(model.CodeUnavailable, "guest process service is unavailable", nil)
		}
		started, startErr := service.lifecycle.guest.Start(ctx, snapshot, current, 0)
		if startErr != nil {
			return snapshot, startErr
		}
		if err := service.lifecycle.waitGuestReady(ctx, snapshot, started.Generation); err != nil {
			return snapshot, err
		}
	}
	return snapshot, nil
}

func workspaceSpecForClone(execution plan.ExecutionPlan, image runtime.Image, record state.ResourceRecord, network string, volumes map[string]string, published []runtime.PortRequest, user string, helper runtime.HostPath, leappMirrorSource string) (runtime.WorkspaceSpec, error) {
	workspaceVolume, found := volumes[cloneWorkspaceVolume]
	if !found || workspaceVolume == "" {
		return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "clone workspace volume is missing", nil)
	}
	clonePlan := execution
	clonePlan.Repositories = nil
	spec, err := workspaceSpecForPlan(clonePlan, image, record, network, volumes, published, user, helper, leappMirrorSource)
	if err != nil {
		return spec, err
	}
	for _, mount := range spec.Mounts {
		for _, repository := range execution.Repositories {
			if mount.Source == repository.HostPath || pathWithin(repository.HostPath, mount.Source) || pathWithin(mount.Source, repository.HostPath) {
				return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "clone workspace cannot mount host repository source", nil)
			}
		}
		if mount.Target == cloneWorkspaceVolumeDir {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "clone workspace mount target is reserved", nil)
		}
	}
	uid, gid, err := guestUserIdentity(user)
	if err != nil {
		return runtime.WorkspaceSpec{}, err
	}
	spec.Mounts = append(spec.Mounts, runtime.Mount{Source: workspaceVolume, Target: cloneWorkspaceVolumeDir, Type: "volume", Authority: runtime.MountAuthorityInternal})
	spec.User = "0:0"
	spec.Entrypoint = []string{
		DefaultGuestHelperPath, "serve",
		"--socket", DefaultGuestSocketPath,
		"--child-uid", uid,
		"--child-gid", gid,
		"--initialize-workspace", cloneWorkspaceVolumeDir,
	}
	return spec, nil
}

func (service *CloneService) bootstrap(ctx context.Context, snapshot runtime.ResourceSnapshot, manifest *state.Manifest, artifacts []gitx.SourceArtifact) (returnErr error) {
	root := "/tmp/dsx-source-" + string(manifest.RunID)
	if _, err := service.shell(ctx, snapshot, []string{"/bin/mkdir", "-p", root}, nil, false, nil, nil, nil, nil, "/workspace"); err != nil {
		return err
	}
	defer func() {
		_, err := service.shell(context.WithoutCancel(ctx), snapshot, []string{"/bin/rm", "-rf", "--", root}, nil, false, nil, nil, nil, nil, "/workspace")
		returnErr = errors.Join(returnErr, err)
	}()
	for index, artifact := range artifacts {
		bundle := path.Join(root, fmt.Sprintf("source-%d.bundle", index))
		if err := service.lifecycle.runtime.CopyTo(ctx, snapshot, runtime.HostPath(artifact.BundlePath), runtime.GuestPath(bundle)); err != nil {
			return err
		}
		verificationRepository := path.Join(root, fmt.Sprintf("verify-%d.git", index))
		if _, err := service.gitExec(ctx, snapshot, "/workspace", []string{"init", "--bare", "--quiet", verificationRepository}, nil, nil); err != nil {
			return err
		}
		if _, err := service.gitExec(ctx, snapshot, verificationRepository, []string{"bundle", "verify", bundle}, nil, nil); err != nil {
			return err
		}
		if _, err := service.shell(ctx, snapshot, []string{"/bin/mkdir", "-p", path.Dir(artifact.Repository.GuestPath)}, nil, false, nil, nil, nil, nil, "/workspace"); err != nil {
			return err
		}
		if _, err := service.gitExec(ctx, snapshot, "/workspace", []string{"init", "--quiet", artifact.Repository.GuestPath}, nil, nil); err != nil {
			return err
		}
		if _, err := service.gitExec(ctx, snapshot, artifact.Repository.GuestPath, []string{"fetch", "--no-tags", "--no-write-fetch-head", "--", bundle, artifact.BundleRef}, nil, nil); err != nil {
			return err
		}
		if _, err := service.gitExec(ctx, snapshot, artifact.Repository.GuestPath, []string{"checkout", "-B", manifest.Git[index].ResultBranch, artifact.SourceCommit}, nil, nil); err != nil {
			return err
		}
	}
	return nil
}
func (service *CloneService) markCapturePending(ctx context.Context, manifest *state.Manifest) error {
	if manifest.UncapturedWork {
		return model.NewError(model.CodeInternal, "clone capture is already pending", nil)
	}
	previous := *manifest
	manifest.State = model.StateCreating
	manifest.Operation = "capture"
	manifest.Failure = ""
	manifest.UncapturedWork = true
	if err := service.lifecycle.replace(ctx, manifest); err != nil {
		*manifest = previous
		return err
	}
	return nil
}

func (service *CloneService) finalizeCaptured(ctx context.Context, manifest *state.Manifest, harnessErr error) error {
	next := *manifest
	nextState, failure := model.StateRunning, ""
	if harnessErr != nil {
		nextState, failure = model.StateFailed, harnessErr.Error()
	} else {
		next.UncapturedWork = false
	}
	if err := service.lifecycle.transition(ctx, &next, nextState, "create", failure); err != nil {
		return err
	}
	*manifest = next
	return nil
}
func (service *CloneService) recoverCleanupCapture(ctx context.Context, snapshot runtime.ResourceSnapshot, manifest *state.Manifest) error {
	if !manifest.UncapturedWork {
		next := *manifest
		if next.State == model.StateRunning || next.State == model.StateStopped {
			next.State = model.StateCreating
			next.Operation = "capture"
			next.Failure = ""
		}
		next.UncapturedWork = true
		if err := service.lifecycle.replace(ctx, &next); err != nil {
			return err
		}
		*manifest = next
	}
	if _, err := service.captureResults(ctx, snapshot, manifest); err != nil {
		return err
	}
	if manifest.State == model.StateCreating {
		return service.finalizeCaptured(ctx, manifest, nil)
	}
	next := *manifest
	next.UncapturedWork = false
	if err := service.lifecycle.replace(ctx, &next); err != nil {
		return err
	}
	*manifest = next
	return nil
}
func (service *CloneService) validateCleanupRepositoryIdentities(ctx context.Context, manifest state.Manifest) error {
	for _, record := range manifest.Git {
		repository := gitx.Repository{
			Name: record.Repository, HostPath: record.HostPath, GuestPath: record.GuestPath, Identity: record.Identity,
		}
		if err := service.git.ValidateRepository(ctx, repository); err != nil {
			return model.Wrap(model.CodeAmbiguous, "validate clone repository identity before cleanup", err)
		}
	}
	return nil
}

func (service *CloneService) verifyCleanupFetchedResults(ctx context.Context, manifest state.Manifest) error {
	statuses, err := service.statusLocked(ctx, manifest)
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "verify fetched clone results", err)
	}
	if len(statuses) != len(manifest.Git) {
		return model.NewError(model.CodeInternal, "fetched clone result verification returned an incomplete status set", nil)
	}
	for index, record := range manifest.Git {
		if record.ResultFetched() && !statuses[index].Fetched {
			return model.NewError(model.CodeDataLoss, fmt.Sprintf("clone sandbox %s repository %s fetched host ref moved or is missing", manifest.Sandbox, record.Repository), nil)
		}
	}
	return nil
}

func (service *CloneService) captureResults(ctx context.Context, snapshot runtime.ResourceSnapshot, manifest *state.Manifest) ([]gitx.Status, error) {
	for index := range manifest.Git {
		record := manifest.Git[index]
		if _, err := service.gitExec(ctx, snapshot, record.GuestPath, []string{"add", "-A"}, nil, nil); err != nil {
			return nil, err
		}
		exit, err := service.gitExit(ctx, snapshot, record.GuestPath, []string{"diff", "--cached", "--quiet", "--exit-code"}, nil, nil)
		if err != nil {
			return nil, err
		}
		if exit.Code == nil || (*exit.Code != 0 && *exit.Code != 1) {
			return nil, model.NewError(model.CodeUnavailable, "inspect staged clone result failed", nil)
		}
		if *exit.Code == 1 {
			if _, err := service.gitExec(ctx, snapshot, record.GuestPath, []string{"commit", "--no-gpg-sign", "-m", "DSX result for " + string(manifest.Sandbox)}, nil, nil); err != nil {
				return nil, err
			}
		}
		var output bytes.Buffer
		if _, err := service.gitExec(ctx, snapshot, record.GuestPath, []string{"rev-parse", "--verify", "HEAD^{commit}"}, &output, nil); err != nil {
			return nil, err
		}
		head := strings.TrimSpace(output.String())
		if head == record.SourceCommit {
			record.ResultCommit = ""
			record.ResultBundleDigest = ""
			record.FetchedCommit = ""
			record.FetchedHostRef = ""
			manifest.Git[index] = record
			if err := service.lifecycle.replace(ctx, manifest); err != nil {
				return nil, err
			}
			continue
		}
		ancestor, err := service.gitExit(ctx, snapshot, record.GuestPath, []string{"merge-base", "--is-ancestor", record.SourceCommit, head}, nil, nil)
		if err != nil {
			return nil, err
		}
		if ancestor.Code == nil || *ancestor.Code != 0 || ancestor.Signal != "" {
			return nil, model.NewError(model.CodeConflict, "clone result rewrote or detached the approved source history", nil)
		}
		if _, err := service.gitExec(ctx, snapshot, record.GuestPath, []string{"update-ref", "refs/heads/" + record.ResultBranch, head}, nil, nil); err != nil {
			return nil, err
		}
		if head != record.ResultCommit {
			record.FetchedCommit = ""
			record.FetchedHostRef = ""
		}
		record.ResultCommit = head
		artifact, cleanup, err := service.copyResult(ctx, snapshot, record, manifest.RunID, index)
		if err != nil {
			return nil, err
		}
		record.ResultBundleDigest = artifact.BundleDigest
		if err := cleanup(); err != nil {
			return nil, err
		}
		manifest.Git[index] = record
		if err := service.lifecycle.replace(ctx, manifest); err != nil {
			return nil, err
		}
	}
	return service.statusLocked(ctx, *manifest)
}

func (service *CloneService) copyResult(ctx context.Context, snapshot runtime.ResourceSnapshot, record state.GitRecord, runID model.RunID, index int) (artifact gitx.ResultArtifact, cleanup func() error, returnErr error) {
	cleanup = func() error { return nil }
	if !record.HasResultWork() {
		return artifact, cleanup, model.NewError(model.CodeConflict, "clone repository has no result work", nil)
	}
	guestRoot := path.Join("/tmp/dsx-run", string(runID), "tmp")
	if err := service.harness.mkdirGuest(ctx, snapshot, guestRoot); err != nil {
		return artifact, cleanup, err
	}
	guest := path.Join(guestRoot, fmt.Sprintf("result-%d.bundle", index))
	producerArgv := []string{"/usr/bin/git", "--no-pager", "-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false",
		"bundle", "create", "--version=2", "-", "refs/heads/" + record.ResultBranch}
	if err := service.harness.produceGuestFile(ctx, snapshot, guest, record.GuestPath, gitx.MaxResultBundleBytes, guestproto.CommandSpec{
		Argv: producerArgv, Cwd: record.GuestPath, Env: harnessEnvironment(cloneGitEnvironment()),
	}); err != nil {
		return artifact, cleanup, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		cleanupErr := service.harness.removeGuestExportFile(cleanupCtx, snapshot, guest)
		if cleanupErr != nil {
			cleanupErr = errors.Join(cleanupErr, cleanup())
		}
		returnErr = errors.Join(returnErr, cleanupErr)
	}()
	if _, err := service.gitExec(ctx, snapshot, record.GuestPath, []string{"bundle", "verify", guest}, nil, nil); err != nil {
		return artifact, cleanup, err
	}
	directory, err := os.MkdirTemp(service.tempRoot, ".dsx-result-")
	if err != nil {
		return artifact, cleanup, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return artifact, cleanup, err
	}
	cleanup = func() error { return os.RemoveAll(directory) }
	host := filepath.Join(directory, "result.bundle")
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, cleanup())
		}
	}()
	present, err := service.harness.exportGuestFile(ctx, snapshot, guest, host, "result", gitx.MaxResultBundleBytes+1)
	if err != nil {
		return artifact, cleanup, model.Wrap(model.CodeUnavailable, "export clone result bundle", err)
	}
	if !present {
		return artifact, cleanup, errors.New("verified clone result bundle is absent")
	}
	info, err := os.Lstat(host)
	if err != nil {
		return artifact, cleanup, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != gitx.ResultBundleMode || info.Size() < 0 || info.Size() > gitx.MaxResultBundleBytes {
		return artifact, cleanup, fmt.Errorf("exported result bundle exceeds %d bytes or has unsafe metadata", gitx.MaxResultBundleBytes)
	}
	digest, err := fileSHA256(host)
	if err != nil {
		return artifact, cleanup, err
	}
	if err := service.git.VerifyBundle(ctx, host, digest); err != nil {
		return artifact, cleanup, err
	}
	return gitx.ResultArtifact{
		Repository:   gitx.Repository{Name: record.Repository, HostPath: record.HostPath, GuestPath: record.GuestPath, Identity: record.Identity},
		ResultBranch: record.ResultBranch, ResultCommit: record.ResultCommit, BundlePath: host, BundleDigest: digest,
	}, cleanup, nil
}

func fileSHA256(name string) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	copied, err := io.Copy(digest, io.LimitReader(file, gitx.MaxResultBundleBytes+1))
	if err != nil {
		return "", err
	}
	if copied > gitx.MaxResultBundleBytes {
		return "", fmt.Errorf("result bundle exceeds %d bytes", gitx.MaxResultBundleBytes)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (service *CloneService) GitStatus(ctx context.Context, request GitStatusRequest) (GitStatusResult, error) {
	manifest, unlock, err := service.lockedManifest(ctx, request.Root, request.Sandbox)
	if err != nil {
		return GitStatusResult{}, err
	}
	defer unlock()
	indexes, err := selectedGitIndexes(manifest, request.Repository)
	if err != nil {
		return GitStatusResult{}, err
	}
	statuses, err := service.statusSelected(ctx, manifest, indexes)
	return GitStatusResult{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, Repositories: statuses}, err
}

func (service *CloneService) statusLocked(ctx context.Context, manifest state.Manifest) ([]gitx.Status, error) {
	indexes := make([]int, len(manifest.Git))
	for index := range indexes {
		indexes[index] = index
	}
	return service.statusSelected(ctx, manifest, indexes)
}

func (service *CloneService) statusSelected(ctx context.Context, manifest state.Manifest, indexes []int) ([]gitx.Status, error) {
	result := make([]gitx.Status, 0, len(indexes))
	for _, index := range indexes {
		record := manifest.Git[index]
		status, err := service.git.Status(ctx, gitx.StatusRequest{
			Repository: gitx.Repository{Name: record.Repository, HostPath: record.HostPath, GuestPath: record.GuestPath, Identity: record.Identity},
			Sandbox:    string(manifest.Sandbox), SourceRef: record.SourceRef, SourceCommit: record.SourceCommit,
			ResultBranch: record.ResultBranch, ResultCommit: record.ResultCommit,
			TrackedFingerprint: record.TrackedFingerprint, FetchedCommit: record.FetchedCommit,
		})
		if err != nil {
			return nil, err
		}
		status.WarnUntracked = record.WarnUntracked
		status.WarnIgnored = record.WarnIgnored
		status.Fetched = record.ResultFetched() && status.Fetched && status.FetchedCommit == record.ResultCommit
		result = append(result, status)
	}
	return result, nil
}

func selectedGitIndexes(manifest state.Manifest, repository string) ([]int, error) {
	if repository == "" {
		indexes := make([]int, len(manifest.Git))
		for index := range indexes {
			indexes[index] = index
		}
		return indexes, nil
	}
	for index, record := range manifest.Git {
		if record.Repository == repository {
			return []int{index}, nil
		}
	}
	return nil, model.NewError(model.CodeInvalidInput, fmt.Sprintf("repository %q is not a member of sandbox %q", repository, manifest.Sandbox), nil)
}

func (service *CloneService) GitDiff(ctx context.Context, request GitDiffRequest) (result GitDiffResult, returnErr error) {
	manifest, unlock, err := service.lockedManifest(ctx, request.Root, request.Sandbox)
	if err != nil {
		return GitDiffResult{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	indexes, err := selectedGitIndexes(manifest, request.Repository)
	if err != nil {
		return GitDiffResult{}, err
	}
	result = GitDiffResult{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox}
	var snapshot runtime.ResourceSnapshot
	snapshotReady := false
	for _, index := range indexes {
		record := manifest.Git[index]
		if record.ResultCommit == "" {
			continue
		}
		diffRequest := gitx.DiffRequest{
			Repository: gitx.Repository{
				Name: record.Repository, HostPath: record.HostPath, GuestPath: record.GuestPath, Identity: record.Identity,
			},
			BaseCommit: record.SourceCommit, HeadCommit: record.ResultCommit, MaxBytes: request.MaxBytes,
		}
		var cleanup func() error
		if !record.ResultFetched() {
			if !snapshotReady {
				snapshot, err = service.cloneWorkspace(ctx, &manifest, cloneWorkspaceAccess{RestartStopped: true})
				if err != nil {
					return GitDiffResult{}, err
				}
				snapshotReady = true
			}
			artifact, artifactCleanup, err := service.copyResult(ctx, snapshot, record, manifest.RunID, index)
			if err != nil {
				return GitDiffResult{}, err
			}
			cleanup = artifactCleanup
			diffRequest.Bundle = &gitx.DiffBundle{
				Path: artifact.BundlePath, Digest: artifact.BundleDigest, Ref: "refs/heads/" + artifact.ResultBranch,
			}
		}
		diff, diffErr := service.git.Diff(ctx, diffRequest)
		var cleanupErr error
		if cleanup != nil {
			cleanupErr = cleanup()
		}
		if diffErr != nil || cleanupErr != nil {
			return GitDiffResult{}, errors.Join(diffErr, cleanupErr)
		}
		result.Diffs = append(result.Diffs, RepositoryDiff{Repository: record.Repository, Patch: diff.Patch, Truncated: diff.Truncated})
	}
	return result, nil
}

func (service *CloneService) GitFetch(ctx context.Context, request GitFetchRequest) (result GitFetchResult, returnErr error) {
	manifest, unlock, err := service.lockedManifest(ctx, request.Root, request.Sandbox)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	indexes, err := selectedGitIndexes(manifest, request.Repository)
	if err != nil {
		return result, err
	}
	result.ProjectID, result.Sandbox = manifest.ProjectID, manifest.Sandbox
	snapshot, err := service.cloneWorkspace(ctx, &manifest, cloneWorkspaceAccess{RestartStopped: true})
	if err != nil {
		return result, err
	}
	if manifest.UncapturedWork {
		if err := service.recoverCleanupCapture(ctx, snapshot, &manifest); err != nil {
			return result, model.Wrap(model.CodeDataLoss, "recover uncertain clone result before fetch", err)
		}
	}
	for _, index := range indexes {
		record := &manifest.Git[index]
		if !record.HasResultWork() {
			continue
		}
		artifact, cleanup, err := service.copyResult(ctx, snapshot, *record, manifest.RunID, index)
		if err != nil {
			return result, err
		}
		fetched, fetchErr := service.git.FetchResult(ctx, gitx.FetchRequest{
			Repository: artifact.Repository, Sandbox: string(manifest.Sandbox), BundlePath: artifact.BundlePath,
			Digest: artifact.BundleDigest, ExpectedCommit: record.ResultCommit,
		})
		cleanupErr := cleanup()
		if fetchErr != nil || cleanupErr != nil {
			return result, errors.Join(fetchErr, cleanupErr)
		}
		if fetched.HostRef != gitx.RefNamespace+string(manifest.Sandbox) || fetched.Commit != record.ResultCommit {
			return result, model.NewError(model.CodeInternal, "host Git fetch returned unexpected ref or commit", nil)
		}
		record.ResultBundleDigest = artifact.BundleDigest
		record.FetchedHostRef = fetched.HostRef
		record.FetchedCommit = fetched.Commit
		if err := service.lifecycle.replace(ctx, &manifest); err != nil {
			return result, err
		}
		result.Repositories = append(result.Repositories, fetched)
	}
	return result, nil
}

type preparedCloneApply struct {
	repository  string
	transaction gitx.ApplyTransaction
}

func (service *CloneService) GitApply(ctx context.Context, request GitApplyRequest) (result GitApplyResult, returnErr error) {
	manifest, unlock, err := service.lockedManifest(ctx, request.Root, request.Sandbox)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	indexes, err := selectedGitIndexes(manifest, request.Repository)
	if err != nil {
		return result, err
	}
	result.ProjectID, result.Sandbox = manifest.ProjectID, manifest.Sandbox
	for _, index := range indexes {
		record := manifest.Git[index]
		if !record.HasResultWork() {
			continue
		}
		if !record.ResultFetched() || record.FetchedHostRef != gitx.RefNamespace+string(manifest.Sandbox) {
			return GitApplyResult{}, model.NewError(model.CodeConflict, "clone result must be fetched before apply", nil)
		}
	}
	prepared := make([]preparedCloneApply, 0, len(indexes))
	for _, index := range indexes {
		record := manifest.Git[index]
		if !record.HasResultWork() {
			continue
		}
		transaction, err := service.git.PrepareApply(ctx, gitx.ApplyRequest{
			Repository: gitx.Repository{
				Name: record.Repository, HostPath: record.HostPath, GuestPath: record.GuestPath, Identity: record.Identity,
			},
			SourceCommit: record.SourceCommit, TrackedFingerprint: record.TrackedFingerprint,
			FetchedRef: record.FetchedHostRef, ExpectedCommit: record.ResultCommit,
		})
		if err != nil {
			return GitApplyResult{}, fmt.Errorf("prepare repository %q apply: %w", record.Repository, err)
		}
		prepared = append(prepared, preparedCloneApply{repository: record.Repository, transaction: transaction})
	}
	for _, item := range prepared {
		applied, err := item.transaction.Commit(ctx)
		if err != nil {
			rollbackErr := rollbackPreparedApplies(context.WithoutCancel(ctx), prepared)
			return GitApplyResult{}, errors.Join(fmt.Errorf("apply repository %q: %w", item.repository, err), rollbackErr)
		}
		result.Repositories = append(result.Repositories, applied)
	}
	return result, nil
}

func rollbackPreparedApplies(ctx context.Context, prepared []preparedCloneApply) error {
	var rollbackErrors []error
	for index := len(prepared) - 1; index >= 0; index-- {
		if err := prepared[index].transaction.Rollback(ctx); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("rollback repository %q: %w", prepared[index].repository, err))
		}
	}
	return errors.Join(rollbackErrors...)
}

func (service *CloneService) lockedManifest(ctx context.Context, root, requested string) (state.Manifest, func() error, error) {
	if err := service.ready(ctx); err != nil {
		return state.Manifest{}, nil, err
	}
	sandbox, err := model.ParseSandboxName(requested)
	if err != nil {
		return state.Manifest{}, nil, model.NewError(model.CodeInvalidInput, "invalid clone sandbox", err)
	}
	projectID, err := projectIDForRoot(root)
	if err != nil {
		return state.Manifest{}, nil, err
	}
	sandboxLease, err := service.lifecycle.locks.LockSandbox(ctx, projectID, sandbox)
	if err != nil {
		return state.Manifest{}, nil, err
	}
	unlockSandbox := func() error { return sandboxLease.Unlock() }
	projectLock, err := service.lifecycle.locks.LockProject(ctx, projectID)
	if err != nil {
		return state.Manifest{}, nil, errors.Join(err, unlockSandbox())
	}
	unlockProject := func() error { return projectLock.Unlock() }
	manifests, err := service.lifecycle.manifests.ListProjectManifests(ctx, projectID)
	if err != nil {
		return state.Manifest{}, nil, errors.Join(err, unlockProject(), unlockSandbox())
	}
	var found *state.Manifest
	for index := range manifests {
		manifest := manifests[index]
		if manifest.Sandbox != sandbox || manifest.Mode != model.ModeClone || manifest.State == model.StateDeleted {
			continue
		}
		if found != nil {
			return state.Manifest{}, nil, errors.Join(
				model.NewError(model.CodeAmbiguous, "multiple active clone manifests", nil),
				unlockProject(),
				unlockSandbox(),
			)
		}
		copy := manifest
		found = &copy
	}
	if found == nil {
		return state.Manifest{}, nil, errors.Join(
			model.NewError(model.CodeConflict, "clone sandbox manifest not found", nil),
			unlockProject(),
			unlockSandbox(),
		)
	}
	ready := found.State == model.StateRunning || found.State == model.StateStopped || found.State == model.StateFailed
	if found.State == model.StateCreating {
		// A durable capture marker means the harness may have run even when a
		// crash prevented the final state transition. Expose every member that
		// was finalized before the crash while retaining cleanup uncertainty.
		ready = found.UncapturedWork
	}
	if !ready {
		return state.Manifest{}, nil, errors.Join(
			model.NewError(model.CodeConflict, "clone result is not ready", nil),
			unlockProject(),
			unlockSandbox(),
		)
	}
	if err := unlockProject(); err != nil {
		return state.Manifest{}, nil, errors.Join(err, unlockSandbox())
	}
	return *found, unlockSandbox, nil
}

func (service *CloneService) cloneWorkspace(ctx context.Context, manifest *state.Manifest, access cloneWorkspaceAccess) (runtime.ResourceSnapshot, error) {
	record, err := manifestResource(*manifest, runtime.ResourceWorkspace, workspaceRole)
	if err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	snapshot, found, err := service.lifecycle.findRuntimeResource(ctx, record)
	if err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	if !found {
		return runtime.ResourceSnapshot{}, model.NewError(model.CodeUnavailable, "clone workspace is missing", nil)
	}
	classification := ownership.Classify(&record, &snapshot)
	if !classification.DeleteAllowed {
		return snapshot, model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	recoverableFailed := manifest.State == model.StateFailed && manifest.UncapturedWork
	switch snapshot.State {
	case "running":
		if manifest.State == model.StateStopped {
			return snapshot, model.NewError(model.CodeAmbiguous, "stopped clone manifest has a running runtime workspace", nil)
		}
		return snapshot, nil
	case "stopped":
		if !access.RestartStopped {
			return snapshot, model.NewError(model.CodeConflict, "clone workspace is stopped", nil)
		}
		if manifest.State != model.StateStopped && !recoverableFailed {
			return snapshot, model.NewError(model.CodeAmbiguous, "runtime workspace is stopped but clone manifest is not restartable", nil)
		}
	default:
		return snapshot, model.NewError(model.CodeUnavailable, fmt.Sprintf("clone workspace has unsupported state %q", snapshot.State), nil)
	}
	if err := service.lifecycle.runtime.StartWorkspace(ctx, snapshot); err != nil {
		return snapshot, model.Wrap(model.CodeUnavailable, "restart clone workspace", err)
	}
	restarted, found, inspectErr := service.lifecycle.findRuntimeResource(ctx, record)
	if inspectErr != nil || !found {
		stopErr := service.lifecycle.runtime.Stop(context.WithoutCancel(ctx), snapshot, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"})
		if !found && inspectErr == nil {
			inspectErr = model.NewError(model.CodeUnavailable, "restarted clone workspace is missing", nil)
		}
		return restarted, errors.Join(inspectErr, stopErr)
	}
	classification = ownership.Classify(&record, &restarted)
	if !classification.DeleteAllowed || restarted.State != "running" {
		stopErr := service.lifecycle.runtime.Stop(context.WithoutCancel(ctx), restarted, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"})
		return restarted, errors.Join(model.NewError(model.CodeAmbiguous, "restarted clone workspace does not match the owned running snapshot", nil), stopErr)
	}
	if service.lifecycle.guest != nil {
		if err := service.lifecycle.guest.Reconcile(ctx, restarted); err != nil {
			stopErr := service.lifecycle.runtime.Stop(context.WithoutCancel(ctx), restarted, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"})
			return restarted, errors.Join(model.Wrap(model.CodeUnavailable, "reconcile restarted clone guest", err), stopErr)
		}
	}
	if recoverableFailed {
		return restarted, nil
	}
	if err := service.lifecycle.transition(ctx, manifest, model.StateRunning, "create", ""); err != nil {
		stopErr := service.lifecycle.runtime.Stop(context.WithoutCancel(ctx), restarted, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"})
		return restarted, errors.Join(err, stopErr)
	}
	return restarted, nil
}

func (service *CloneService) shell(ctx context.Context, snapshot runtime.ResourceSnapshot, argv []string, environment map[string]string, terminal bool, stdin io.Reader, stdout, stderr io.Writer, runInteractive InteractiveChildRunner, workingDirectory string) (runtime.Exit, error) {
	if len(argv) == 0 {
		return runtime.Exit{}, model.NewError(model.CodeInvalidInput, "clone command argv is required", nil)
	}
	if workingDirectory == "" {
		workingDirectory = cloneWorkspaceVolumeDir
	}
	arguments := append([]string{DefaultGuestHelperPath, "exec", "--"}, argv...)
	spec := runtime.ExecSpec{Argv: arguments, Env: harnessEnvironment(environment), WorkingDir: runtime.GuestPath(workingDirectory), User: service.lifecycle.user(), Terminal: terminal}
	if terminal {
		if runInteractive == nil {
			return runtime.Exit{}, model.NewError(model.CodeInvalidInput, "interactive clone runner is not configured", nil)
		}
		process, err := service.lifecycle.runtime.PrepareExec(ctx, snapshot, spec)
		if err != nil {
			return runtime.Exit{}, err
		}
		return runInteractive(ctx, InteractiveChild{Argv: append([]string{process.Executable}, process.Args...), Env: append([]string(nil), process.Env...), Dir: process.Dir, Stdin: stdin, Stdout: stdout, Stderr: stderr})
	}
	return service.lifecycle.runtime.Exec(ctx, snapshot, spec, runtime.ExecIO{Stdin: stdin, Stdout: stdout, Stderr: stderr})
}

func cloneGitEnvironment() map[string]string {
	return map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_SYSTEM": os.DevNull, "GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0", "GIT_PAGER": "cat", "LC_ALL": "C",
		"GIT_AUTHOR_NAME": cloneGitIdentityName, "GIT_AUTHOR_EMAIL": cloneGitIdentityEmail, "GIT_AUTHOR_DATE": cloneGitTimestamp,
		"GIT_COMMITTER_NAME": cloneGitIdentityName, "GIT_COMMITTER_EMAIL": cloneGitIdentityEmail, "GIT_COMMITTER_DATE": cloneGitTimestamp,
	}
}

func (service *CloneService) gitExit(ctx context.Context, snapshot runtime.ResourceSnapshot, workingDirectory string, arguments []string, stdout, stderr io.Writer) (runtime.Exit, error) {
	argv := []string{"/usr/bin/git", "--no-pager", "-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false"}
	argv = append(argv, arguments...)
	return service.shell(ctx, snapshot, argv, cloneGitEnvironment(), false, nil, stdout, stderr, nil, workingDirectory)
}

func (service *CloneService) gitExec(ctx context.Context, snapshot runtime.ResourceSnapshot, workingDirectory string, arguments []string, stdout, stderr io.Writer) (runtime.Exit, error) {
	var captured cappedBuffer
	if stderr == nil {
		captured.limit = 4096
		stderr = &captured
	}
	exit, err := service.gitExit(ctx, snapshot, workingDirectory, arguments, stdout, stderr)
	if err != nil {
		return exit, err
	}
	if exit.Code == nil || *exit.Code != 0 || exit.Signal != "" {
		operation := "command"
		if len(arguments) != 0 && arguments[0] != "" {
			operation = arguments[0]
		}
		outcome := "without an exit code"
		if exit.Code != nil {
			outcome = fmt.Sprintf("with exit code %d", *exit.Code)
		} else if exit.Signal != "" {
			outcome = "from signal " + exit.Signal
		}
		message := fmt.Sprintf("guest Git %s failed %s", operation, outcome)
		if detail := strings.TrimSpace(captured.String()); detail != "" {
			message += ": " + terminal.SanitizeLine(detail)
		}
		return exit, model.NewError(model.CodeUnavailable, message, nil)
	}
	return exit, nil
}

func (service *CloneService) runHarness(ctx context.Context, snapshot runtime.ResourceSnapshot, execution plan.ExecutionPlan, request CloneRunRequest, hostBridges *hostBridgeSession) (result HarnessRunResult, returnErr error) {
	name, err := harness.ParseName(request.Agent)
	if err != nil {
		return result, model.NewError(model.CodeInvalidInput, err.Error(), nil)
	}
	adapter := service.harness.adapters[name]
	if adapter == nil {
		return result, model.NewError(model.CodeUnavailable, fmt.Sprintf("harness %q is not installed", name), nil)
	}
	profileName := request.Profile
	if profileName == "" {
		profileName = "default"
	}
	persistence, err := authorizeHarnessGrant(execution, name, profileName)
	if err != nil {
		return result, err
	}
	if err := service.harness.verifyHarnessBuildAttestation(ctx, snapshot, execution, adapter, func(stdout, stderr io.Writer) (runtime.Exit, error) {
		return service.shell(ctx, snapshot, []string{"/bin/cat", "--", harness.BuildAttestationPath}, nil, false, nil, stdout, stderr, nil, "/workspace")
	}); err != nil {
		return result, err
	}
	invocationID, err := service.lifecycle.newRunID(service.lifecycle.now().UTC())
	if err != nil {
		return result, model.Wrap(model.CodeInternal, "generate clone harness invocation ID", err)
	}
	roots := harnessRoots(invocationID)
	if diagnostics, err := adapter.Preflight(ctx, roots); err != nil {
		return result, err
	} else {
		for _, diagnostic := range diagnostics {
			if diagnostic.Severity == "error" {
				return result, model.NewError(model.CodeUnavailable, "harness preflight failed: "+diagnostic.Code, nil)
			}
		}
	}
	profile := auth.Profile{Harness: name, Name: profileName}
	if persistence == "sandbox" {
		profile = auth.SandboxProfile(profile, execution.Project.ID, string(execution.Sandbox.Name))
	}
	var authCopy auth.Copy
	if persistence == "global" {
		if _, err := service.harness.auth.Ensure(ctx, profile, adapter); err != nil {
			return result, model.Wrap(model.CodeUnavailable, "ensure authentication profile", err)
		}
		authCopy, err = service.harness.auth.PrepareGlobalSandbox(ctx, profile, invocationID, execution.Project.ID, string(execution.Sandbox.Name), adapter)
	} else {
		authCopy, err = service.harness.auth.PrepareSandbox(ctx, profile, invocationID, adapter)
	}
	if err != nil {
		return result, model.Wrap(model.CodeUnavailable, "prepare authentication profile copy", err)
	}
	cleanupBase := context.WithoutCancel(ctx)
	defer func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(cleanupBase, 30*time.Second)
		defer cancelCleanup()
		returnErr = errors.Join(returnErr, service.harness.auth.RemoveRun(cleanupCtx, authCopy))
	}()

	if err := service.harness.prepareGuestRoots(ctx, snapshot, roots); err != nil {
		return result, err
	}
	defer func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(cleanupBase, 30*time.Second)
		defer cancelCleanup()
		_, cleanupErr := service.shell(cleanupCtx, snapshot, []string{"/bin/rm", "-rf", "--", path.Dir(roots.Home)}, nil, false, nil, nil, nil, nil, roots.Workspace)
		returnErr = errors.Join(returnErr, cleanupErr)
	}()
	layout := adapter.AuthLayout()
	if err := service.harness.copyAuthToGuest(ctx, snapshot, authCopy.Root, roots.Auth, layout); err != nil {
		return result, err
	}
	readOnlyConfig, err := service.harness.copyReadOnlyConfigToGuest(ctx, snapshot, authCopy.ReadOnlyRoot, roots.ReadOnlyConfig, layout)
	if err != nil {
		return result, err
	}
	if len(readOnlyConfig) != 0 {
		defer func() {
			cleanupCtx, cancelCleanup := context.WithTimeout(cleanupBase, 30*time.Second)
			defer cancelCleanup()
			returnErr = errors.Join(returnErr, service.harness.removeReadOnlyGuestRoot(cleanupCtx, snapshot, roots.ReadOnlyConfig))
		}()
	}
	artifact := adapter.Version()
	var versionStdout, versionStderr cappedBuffer
	versionStdout.limit = maxHarnessVersionOutput
	versionStderr.limit = maxHarnessVersionOutput
	versionExit, err := service.shell(ctx, snapshot, []string{artifact.Executable, "--version"}, rootEnvironment(roots, layout), false, nil, &versionStdout, &versionStderr, nil, roots.Workspace)
	if err != nil {
		return result, err
	}
	if versionExit.Code == nil || *versionExit.Code != 0 {
		return result, model.NewError(model.CodeUnavailable, fmt.Sprintf("%s version command failed", name), nil)
	}
	if err := adapter.ValidateVersion(versionStdout.String(), versionStderr.String()); err != nil {
		return result, model.Wrap(model.CodeUnavailable, "validate pinned harness version", err)
	}
	mcpRequest := harness.MCPRequest{Roots: roots, Servers: append([]harness.MCPServer(nil), request.MCPServers...)}
	injection, err := adapter.EphemeralMCP(mcpRequest)
	if err != nil {
		return result, err
	}
	if err := service.harness.installGeneratedFiles(ctx, snapshot, authCopy.Root, roots, injection.Files); err != nil {
		return result, err
	}
	if err := service.verifyCloneEffectiveMCP(ctx, snapshot, invocationID, adapter, mcpRequest, injection); err != nil {
		return result, err
	}
	interactive := request.RunInteractive != nil
	spec, err := adapter.Invocation(harness.InvocationRequest{Roots: roots, Prompt: request.Prompt, Interactive: interactive, Environment: cloneEnvironment(request.Environment), ReadOnlyConfig: readOnlyConfig})
	if err != nil {
		return result, err
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return result, model.Wrap(model.CodeInvalidInput, "validate harness invocation", err)
	}
	if spec.Cwd != roots.Workspace {
		return result, model.NewError(model.CodeInvalidInput, "harness working directory must be the workspace root", nil)
	}
	spec.Argv = insertHarnessArgs(spec.Argv, injection.Args)
	for key, value := range injection.Env {
		if spec.Env == nil {
			spec.Env = make(map[string]string)
		}
		spec.Env[key] = value
	}
	spec.Env, err = mergeHostBridgeEnvironment(spec.Env, hostBridges.Environment())
	if err != nil {
		return result, err
	}
	exit, invocationErr := service.shellWithSecretEnvironment(ctx, snapshot, invocationID, spec.Argv, spec.Env, adapter.RedactionRules().EnvironmentKeys, interactive, request.Stdin, request.Stdout, request.Stderr, request.RunInteractive, spec.Cwd)
	syncCtx, cancelSync := context.WithTimeout(cleanupBase, 30*time.Second)
	defer cancelSync()
	pullErr := service.pullCloneAuth(syncCtx, snapshot, authCopy, roots.Auth, adapter)
	var promotion auth.Promotion
	var promotionErr error
	if pullErr == nil {
		promotion, promotionErr = service.harness.auth.Promote(syncCtx, authCopy, adapter)
	}
	result = HarnessRunResult{Agent: name, Version: artifact.Version, Exit: exit, AuthPromotion: promotion}
	if promotion.Conflict {
		promotionErr = errors.Join(promotionErr, model.NewError(model.CodeConflict, "authentication refresh conflicted; candidate preserved", nil))
	}

	return result, errors.Join(invocationErr, pullErr, promotionErr)
}
func (service *CloneService) verifyCloneEffectiveMCP(ctx context.Context, snapshot runtime.ResourceSnapshot, runID model.RunID, adapter harness.Adapter, request harness.MCPRequest, injection harness.ConfigInjection) error {
	verifier, required := adapter.(harness.MCPVerifier)
	if !required {
		return nil
	}
	spec, err := verifier.MCPVerification(request, injection)
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "prepare effective MCP verification", err)
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return model.Wrap(model.CodeUnavailable, "validate effective MCP verification command", err)
	}
	if spec.Terminal || spec.Cwd != request.Roots.Workspace {
		return model.NewError(model.CodeUnavailable, "effective MCP verification must be non-interactive in the workspace root", nil)
	}
	var stdout, stderr cappedBuffer
	stdout.limit = maxHarnessVersionOutput
	stderr.limit = maxHarnessVersionOutput
	exit, err := service.shellWithSecretEnvironment(ctx, snapshot, runID, spec.Argv, spec.Env, adapter.RedactionRules().EnvironmentKeys, false, nil, &stdout, &stderr, nil, spec.Cwd)
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "inspect effective MCP registry", err)
	}
	if exit.Signal != "" || exit.Code == nil || *exit.Code != 0 {
		return model.NewError(model.CodeUnavailable, "effective MCP registry inspection failed", nil)
	}
	if err := verifier.ValidateEffectiveMCP(request, stdout.String(), stderr.String()); err != nil {
		return model.Wrap(model.CodeUnavailable, "refuse inexact effective MCP registry", err)
	}
	return nil
}

func (service *CloneService) pullCloneAuth(ctx context.Context, snapshot runtime.ResourceSnapshot, authCopy auth.Copy, guestRoot string, adapter harness.Adapter) error {
	return service.harness.copyAuthFromGuest(ctx, snapshot, authCopy, guestRoot, adapter)
}

func (service *CloneService) shellWithSecretEnvironment(ctx context.Context, snapshot runtime.ResourceSnapshot, runID model.RunID, argv []string, environment map[string]string, secretKeys []string, terminal bool, stdin io.Reader, stdout, stderr io.Writer, runInteractive InteractiveChildRunner, workingDirectory string) (exit runtime.Exit, returnErr error) {
	staged, err := service.lifecycle.stageExecEnvironment(ctx, snapshot, runID, environment, secretKeys)
	if err != nil {
		return exit, err
	}
	if staged.guest != "" {
		cleanupBase := context.WithoutCancel(ctx)
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(cleanupBase, secretEnvironmentCleanupTimeout)
			defer cancel()
			returnErr = errors.Join(returnErr, service.lifecycle.cleanupGuestSecretEnvironment(cleanupCtx, snapshot, staged.guest))
		}()
	}
	arguments := append([]string(nil), argv...)
	if staged.guest != "" {
		arguments = append([]string{DefaultGuestHelperPath, "exec", "--env-file", string(staged.guest), "--"}, arguments...)
	} else {
		arguments = append([]string{DefaultGuestHelperPath, "exec", "--"}, arguments...)
	}
	spec := runtime.ExecSpec{Argv: arguments, Env: staged.ordinary, WorkingDir: runtime.GuestPath(workingDirectory), User: service.lifecycle.user(), Terminal: terminal}
	if terminal {
		if runInteractive == nil {
			return exit, model.NewError(model.CodeInvalidInput, "interactive clone runner is not configured", nil)
		}
		process, err := service.lifecycle.runtime.PrepareExec(ctx, snapshot, spec)
		if err != nil {
			return exit, err
		}
		return runInteractive(ctx, InteractiveChild{Argv: append([]string{process.Executable}, process.Args...), Env: append([]string(nil), process.Env...), Dir: process.Dir, Stdin: stdin, Stdout: stdout, Stderr: stderr})
	}
	return service.lifecycle.runtime.Exec(ctx, snapshot, spec, runtime.ExecIO{Stdin: stdin, Stdout: stdout, Stderr: stderr})
}
