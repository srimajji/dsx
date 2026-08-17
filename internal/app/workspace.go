package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	agentimage "github.com/srimajji/dsx/images/agent"
	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/ownership"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
	"github.com/srimajji/dsx/internal/terminal"
)

const (
	workspaceNetworkRole = "network"
	workspaceOwnerRole   = "workspace"
	workspaceSourceRole  = "source"
	workspaceAuthRole    = "auth"
	workspaceSessionRole = "session"
	workspaceDepsRole    = "deps"
	workspaceServiceRole = "service"
	workspaceStopSeconds = 10
	workspaceGuestRoot   = runtime.GuestPath("/workspace")
)

var privateWorkspaceVolumes = []struct {
	role   string
	target runtime.GuestPath
}{
	{workspaceSourceRole, "/workspace"},
	{workspaceAuthRole, "/home/dsx/.dsx/auth"},
	{workspaceSessionRole, "/home/dsx/.local/state/dsx"},
	{workspaceDepsRole, "/home/dsx/.cache"},
	{workspaceServiceRole, "/var/lib/dsx"},
}

const standardWorkspaceUser = "1000:1000"

var workspaceInitializationArgv = []string{
	DefaultGuestHelperPath, "initialize-workspace",
	"--child-uid", "1000", "--child-gid", "1000",
	"--path", "/workspace",
	"--path", "/home/dsx/.dsx/auth",
	"--path", "/home/dsx/.local/state/dsx",
	"--path", "/home/dsx/.cache",
	"--path", "/var/lib/dsx",
}

type WorkspacePlanResolver func(context.Context, string) (plan.ExecutionPlan, error)
type WorkspaceRemovalGuard func(context.Context, state.Manifest, runtime.ResourceSnapshot) ([]string, error)

type WorkspaceDependencies struct {
	Inspection        *InspectionService
	ResolvePlan       WorkspacePlanResolver
	Approvals         state.ApprovalRepository
	Manifests         state.ManifestRepository
	Locks             state.LockRepository
	Runtime           runtime.Adapter
	HostAWS           bridge.HostAWSWorkspaceManager
	Git               gitx.HostService
	TempRoot          string
	Now               func() time.Time
	NewRunID          func(time.Time) (model.RunID, error)
	GuestHelperSource func() (runtime.HostPath, error)
	RemovalGuard      WorkspaceRemovalGuard
}

type WorkspaceService struct {
	inspection        *InspectionService
	resolvePlan       WorkspacePlanResolver
	approvals         state.ApprovalRepository
	manifests         state.ManifestRepository
	locks             state.LockRepository
	runtime           runtime.Adapter
	hostAWS           bridge.HostAWSWorkspaceManager
	git               gitx.HostService
	tempRoot          string
	now               func() time.Time
	newRunID          func(time.Time) (model.RunID, error)
	guestHelperSource func() (runtime.HostPath, error)
	removalGuard      WorkspaceRemovalGuard
}

func NewWorkspaceService(dependencies WorkspaceDependencies) *WorkspaceService {
	now := dependencies.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newRunID := dependencies.NewRunID
	if newRunID == nil {
		newRunID = model.NewRunID
	}
	tempRoot := dependencies.TempRoot
	if tempRoot == "" {
		tempRoot = filepath.Clean(os.TempDir())
	}
	service := &WorkspaceService{
		inspection: dependencies.Inspection, resolvePlan: dependencies.ResolvePlan,
		approvals: dependencies.Approvals, manifests: dependencies.Manifests, locks: dependencies.Locks,
		runtime: dependencies.Runtime, git: dependencies.Git, hostAWS: dependencies.HostAWS, tempRoot: tempRoot, now: now,
		newRunID: newRunID, guestHelperSource: dependencies.GuestHelperSource,
		removalGuard: dependencies.RemovalGuard,
	}
	if service.removalGuard == nil && service.git != nil {
		service.removalGuard = service.protectWorkspaceRemoval
	}
	return service
}

type WorkspaceCreateRequest struct {
	Root           string
	Workspace      model.WorkspaceName
	SourceBranch   string
	SourceRevision string
	Snapshot       bool
	DefaultAgent   string
	ApproveConfig  string
	Open           bool
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
	RunInteractive InteractiveChildRunner
}

type WorkspaceStartRequest struct {
	Root      string
	Workspace model.WorkspaceName
}
type WorkspaceStopRequest struct {
	Root      string
	Workspace model.WorkspaceName
}
type WorkspaceRestartRequest struct {
	Root      string
	Workspace model.WorkspaceName
}
type WorkspaceListRequest struct{ Root string }

type WorkspaceOpenRequest struct {
	Root           string
	Workspace      model.WorkspaceName
	Terminal       bool
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
	RunInteractive InteractiveChildRunner
}

type InteractiveChildRunner func(context.Context, InteractiveChild) (runtime.Exit, error)

type WorkspaceRemoveRequest struct {
	Root             string
	Workspace        model.WorkspaceName
	Confirmed        bool
	DiscardUnfetched bool
	LegacyResources  bool
}

type WorkspaceResult struct {
	ProjectID model.ProjectID      `json:"project_id"`
	Workspace model.WorkspaceName  `json:"workspace"`
	RunID     model.RunID          `json:"run_id"`
	State     model.WorkspaceState `json:"state"`
	Existing  bool                 `json:"existing"`
	Warnings  []string             `json:"warnings,omitempty"`
	URLs      []string             `json:"urls,omitempty"`
}

type WorkspaceOpenResult struct {
	WorkspaceResult
	Exit runtime.Exit `json:"exit"`
}

type WorkspaceRemoveResult struct {
	ProjectID        model.ProjectID      `json:"project_id"`
	Workspace        model.WorkspaceName  `json:"workspace"`
	RunID            model.RunID          `json:"run_id"`
	State            model.WorkspaceState `json:"state"`
	DeletedResources int                  `json:"deleted_resources"`
	DeletedManifest  bool                 `json:"deleted_manifest"`
	Preserved        []string             `json:"preserved,omitempty"`
}

type WorkspaceSummary struct {
	ProjectID      model.ProjectID      `json:"project_id"`
	Workspace      model.WorkspaceName  `json:"workspace"`
	RunID          model.RunID          `json:"run_id"`
	State          model.WorkspaceState `json:"state"`
	DefaultAgent   string               `json:"default_agent,omitempty"`
	SourceBranch   string               `json:"source_branch,omitempty"`
	SourceRevision string               `json:"source_revision,omitempty"`
	SourceSnapshot bool                 `json:"source_snapshot"`
	Resources      int                  `json:"resources"`
	MutationActive bool                 `json:"mutation_active"`
	Legacy         bool                 `json:"legacy"`
	Warnings       []string             `json:"warnings,omitempty"`
	URLs           []string             `json:"urls,omitempty"`
}

type WorkspaceListResult struct {
	Workspaces []WorkspaceSummary `json:"workspaces"`
}

type workspaceAccess struct {
	Manifest  *state.Manifest
	Plan      plan.ExecutionPlan
	Workspace runtime.ResourceSnapshot
	Network   state.ResourceRecord
}

func (service *WorkspaceService) ready(ctx context.Context) error {
	if ctx == nil {
		return model.NewError(model.CodeInvalidInput, "workspace context is nil", nil)
	}
	if service == nil || service.manifests == nil || service.locks == nil || service.runtime == nil {
		return model.NewError(model.CodeUnavailable, "workspace service is unavailable", nil)
	}
	return ctx.Err()
}

func (service *WorkspaceService) Create(ctx context.Context, request WorkspaceCreateRequest) (result WorkspaceResult, returnErr error) {
	if err := service.ready(ctx); err != nil {
		return result, err
	}
	if service.git == nil || service.guestHelperSource == nil {
		return result, model.NewError(model.CodeUnavailable, "workspace source transfer is unavailable", nil)
	}
	root, projectID, err := canonicalWorkspaceRoot(request.Root)
	if err != nil {
		return result, err
	}
	name, err := checkedWorkspaceName(request.Workspace)
	if err != nil {
		return result, err
	}
	if request.Open && request.RunInteractive == nil {
		return result, model.NewError(model.CodeInvalidInput, "create --open requires an interactive child runner", nil)
	}
	_, _, unlock, err := service.lockWorkspaceProject(ctx, projectID, name)
	if err != nil {
		return result, err
	}
	locked := true
	releaseLocks := func() error {
		if !locked {
			return nil
		}
		locked = false
		return unlock()
	}
	defer func() { returnErr = errors.Join(returnErr, releaseLocks()) }()

	manifests, err := service.manifests.ListProjectManifests(ctx, projectID)
	if err != nil {
		return result, err
	}
	for _, existing := range manifests {
		if existing.Workspace == name && existing.State != model.StateDeleted {
			return result, model.NewError(model.CodeConflict, "workspace already exists", nil)
		}
	}
	execution, err := service.currentPlan(ctx, root, request.ApproveConfig)
	if err != nil {
		return result, err
	}
	if err := enforceWorkspaceLimit(manifests, execution.Limits.MaxConcurrentWorkspaces, "", "create"); err != nil {
		return result, err
	}
	if _, err := reviewedRuntimeMounts(execution); err != nil {
		return result, err
	}
	if execution.AWS.Mode == plan.AWSModeHostDefault && service.hostAWS == nil {
		return result, model.NewError(model.CodeUnavailable, "host AWS workspace publication is unavailable", nil)
	}
	defaultAgent := request.DefaultAgent
	if defaultAgent == "" {
		defaultAgent = execution.Agents.Default
	}
	if !approvedAgent(execution.Agents.Allowed, defaultAgent) {
		return result, model.NewError(model.CodeInvalidInput, "workspace default agent is not approved", nil)
	}
	artifacts, warnings, err := service.prepareWorkspaceSources(ctx, execution, name, request.SourceBranch, request.SourceRevision, request.Snapshot)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, service.removeSourceArtifacts(artifacts)) }()
	runID, err := service.newRunID(service.now())
	if err != nil {
		return result, model.Wrap(model.CodeInternal, "create workspace run ID", err)
	}
	manifest, err := plannedWorkspaceManifest(execution, name, runID, defaultAgent, artifacts, service.now())
	if err != nil {
		return result, err
	}
	if err := service.manifests.CreateIntent(ctx, manifest); err != nil {
		return result, err
	}
	created := true
	defer func() {
		if returnErr != nil && created {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			returnErr = errors.Join(returnErr, service.rollbackCreate(rollbackCtx, &manifest))
		}
	}()
	if err := service.transitionManifest(ctx, &manifest, model.StateCreating, "create", ""); err != nil {
		return result, err
	}
	image, err := service.ensureWorkspaceImage(ctx, execution)
	if err != nil {
		return result, err
	}
	for index := range manifest.Resources {
		record := manifest.Resources[index]
		switch record.Kind {
		case string(runtime.ResourceNetwork):
			err = service.createResource(ctx, &manifest, index, func() (runtime.Resource, error) {
				return service.runtime.CreateNetwork(ctx, runtime.NetworkSpec{Name: record.Name, Labels: workspaceRuntimeLabels(record.Labels)})
			})
		case string(runtime.ResourceVolume):
			err = service.createResource(ctx, &manifest, index, func() (runtime.Resource, error) {
				return service.runtime.CreateVolume(ctx, runtime.VolumeSpec{Name: record.Name, Labels: workspaceRuntimeLabels(record.Labels)})
			})
		}
		if err != nil {
			return result, err
		}
	}
	network, err := workspaceManifestResource(manifest, runtime.ResourceNetwork, workspaceNetworkRole)
	if err != nil {
		return result, err
	}
	owner, err := workspaceManifestResource(manifest, runtime.ResourceWorkspace, workspaceOwnerRole)
	if err != nil {
		return result, err
	}
	helper, err := service.guestHelperSource()
	if err != nil {
		return result, err
	}
	var hostAWSMirror runtime.HostPath
	if execution.AWS.Mode == plan.AWSModeHostDefault {
		stablePath, prepareErr := service.hostAWS.Prepare(ctx, workspaceLeaseIdentity(manifest))
		if prepareErr != nil {
			return result, prepareErr
		}
		hostAWSMirror = runtime.HostPath(stablePath)
	}
	spec, err := workspaceSpec(execution, manifest, image, network, owner, helper, hostAWSMirror)
	if err != nil {
		return result, err
	}
	ownerIndex := workspaceResourceIndex(manifest, runtime.ResourceWorkspace, workspaceOwnerRole)
	if err := service.createResource(ctx, &manifest, ownerIndex, func() (runtime.Resource, error) { return service.runtime.CreateWorkspace(ctx, spec) }); err != nil {
		return result, err
	}
	snapshot, err := service.ownedSnapshot(ctx, manifest, manifest.Resources[ownerIndex], false)
	if err != nil {
		return result, err
	}
	if err := service.runtime.StartWorkspace(ctx, snapshot); err != nil {
		return result, err
	}
	snapshot, err = service.ownedSnapshot(ctx, manifest, manifest.Resources[ownerIndex], true)
	if err != nil {
		return result, err
	}
	if err := service.initializeWorkspaceVolumes(ctx, snapshot); err != nil {
		return result, err
	}
	if err := service.bootstrapWorkspace(ctx, snapshot, &manifest, artifacts); err != nil {
		return result, err
	}
	if err := service.transitionManifest(ctx, &manifest, model.StateRunning, "", ""); err != nil {
		return result, err
	}
	created = false
	result = workspaceResult(manifest)
	result.Warnings = warnings
	if request.Open {
		if err := releaseLocks(); err != nil {
			return result, err
		}
		_, err := service.Open(ctx, WorkspaceOpenRequest{Root: root, Workspace: name, Terminal: true, Stdin: request.Stdin, Stdout: request.Stdout, Stderr: request.Stderr, RunInteractive: request.RunInteractive})
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (service *WorkspaceService) initializeWorkspaceVolumes(ctx context.Context, snapshot runtime.ResourceSnapshot) error {
	var stderr cappedBuffer
	stderr.limit = 4096
	exit, err := service.runtime.Exec(ctx, snapshot, runtime.ExecSpec{
		Argv:       append([]string(nil), workspaceInitializationArgv...),
		WorkingDir: "/workspace",
		User:       "0:0",
	}, runtime.ExecIO{Stderr: &stderr})
	if err == nil && exit.Code != nil && *exit.Code == 0 && exit.Signal == "" {
		return nil
	}
	detail := terminal.SanitizeLine(strings.TrimSpace(stderr.String()))
	if detail == "" && err != nil {
		detail = terminal.SanitizeLine(strings.TrimSpace(err.Error()))
	}
	if detail == "" {
		switch {
		case exit.Signal != "":
			detail = "guest initializer terminated by signal " + terminal.SanitizeLine(exit.Signal)
		case exit.Code != nil:
			detail = fmt.Sprintf("guest initializer exited with status %d", *exit.Code)
		default:
			detail = "guest initializer returned no exit status"
		}
	}
	return model.NewError(model.CodeUnavailable, "initialize workspace volume ownership: "+detail, err)
}

func (service *WorkspaceService) Start(ctx context.Context, request WorkspaceStartRequest) (result WorkspaceResult, returnErr error) {
	access, unlock, err := service.workspaceAccess(ctx, request.Root, request.Workspace, false)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	manifest := access.Manifest
	if manifest.State != model.StateStopped && manifest.State != model.StateNeedsResolution {
		return result, model.NewError(model.CodeConflict, "workspace is not stopped", nil)
	}
	manifests, err := service.manifests.ListProjectManifests(ctx, manifest.ProjectID)
	if err != nil {
		return result, err
	}
	if err := enforceWorkspaceLimit(manifests, access.Plan.Limits.MaxConcurrentWorkspaces, manifest.RunID, "start"); err != nil {
		return result, err
	}
	if access.Workspace.State != "running" {
		if err := service.transitionManifest(ctx, manifest, manifest.State, "start", ""); err != nil {
			return result, err
		}
		defer func() {
			if returnErr != nil {
				returnErr = errors.Join(returnErr, service.finalizeLifecycleFailure(ctx, manifest, "start", returnErr))
			}
		}()
		if err := service.activateHostAWSForRuntime(ctx, manifest, access.Plan); err != nil {
			return result, err
		}
		if err := service.runtime.StartWorkspace(ctx, access.Workspace); err != nil {
			return result, err
		}
	}
	if err := service.initializeWorkspaceVolumes(ctx, access.Workspace); err != nil {
		return result, err
	}
	next := model.StateRunning
	if workspaceHasConflict(*manifest) {
		next = model.StateNeedsResolution
	}
	if err := service.transitionManifest(ctx, manifest, next, "", ""); err != nil {
		return result, err
	}
	return workspaceResult(*manifest), nil
}

func (service *WorkspaceService) Stop(ctx context.Context, request WorkspaceStopRequest) (result WorkspaceResult, returnErr error) {
	access, unlock, err := service.workspaceAccess(ctx, request.Root, request.Workspace, false)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	if access.Manifest.State != model.StateRunning && access.Manifest.State != model.StateNeedsResolution && access.Manifest.State != model.StateStopped {
		return result, model.NewError(model.CodeConflict, "workspace cannot be stopped from its current state", nil)
	}
	if err := service.transitionManifest(ctx, access.Manifest, access.Manifest.State, "stop", ""); err != nil {
		return result, err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, service.finalizeLifecycleFailure(ctx, access.Manifest, "stop", returnErr))
		}
	}()
	if err := service.cleanupSessionBrowser(ctx, access.Manifest); err != nil {
		return result, err
	}
	if err := service.clearActiveSession(ctx, access.Manifest); err != nil {
		return result, err
	}
	if err := service.disableHostAWSForStoppedRuntime(ctx, *access.Manifest); err != nil {
		return result, err
	}
	if access.Workspace.State == "running" {
		if err := service.runtime.Stop(ctx, access.Workspace, runtime.StopPolicy{TimeoutSeconds: workspaceStopSeconds, Signal: "TERM"}); err != nil {
			return result, err
		}
	}
	next := model.StateStopped
	if workspaceHasConflict(*access.Manifest) {
		next = model.StateNeedsResolution
	}
	if err := service.transitionManifest(ctx, access.Manifest, next, "", ""); err != nil {
		return result, err
	}
	return workspaceResult(*access.Manifest), nil
}

func (service *WorkspaceService) Restart(ctx context.Context, request WorkspaceRestartRequest) (result WorkspaceResult, returnErr error) {
	access, unlock, err := service.workspaceAccess(ctx, request.Root, request.Workspace, false)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	manifest := access.Manifest
	if manifest.State != model.StateRunning && manifest.State != model.StateStopped && manifest.State != model.StateNeedsResolution {
		return result, model.NewError(model.CodeConflict, "workspace cannot be restarted from its current state", nil)
	}
	if err := service.transitionManifest(ctx, manifest, manifest.State, "restart", ""); err != nil {
		return result, err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, service.finalizeLifecycleFailure(ctx, manifest, "restart", returnErr))
		}
	}()
	if err := service.cleanupSessionBrowser(ctx, manifest); err != nil {
		return result, err
	}
	if err := service.clearActiveSession(ctx, manifest); err != nil {
		return result, err
	}
	if access.Workspace.State == "running" {
		if err := service.runtime.Stop(ctx, access.Workspace, runtime.StopPolicy{TimeoutSeconds: workspaceStopSeconds, Signal: "TERM"}); err != nil {
			return result, err
		}
		access.Workspace.State = "stopped"
	}
	if manifest.State == model.StateRunning {
		if err := service.transitionManifest(ctx, manifest, model.StateStopped, "restart", ""); err != nil {
			return result, err
		}
	}
	if err := service.activateHostAWSForRuntime(ctx, manifest, access.Plan); err != nil {
		return result, err
	}
	if err := service.runtime.StartWorkspace(ctx, access.Workspace); err != nil {
		return result, err
	}
	next := model.StateRunning
	if workspaceHasConflict(*manifest) {
		next = model.StateNeedsResolution
	}
	if err := service.transitionManifest(ctx, manifest, next, "", ""); err != nil {
		return result, err
	}
	return workspaceResult(*manifest), nil
}

func (service *WorkspaceService) Open(ctx context.Context, request WorkspaceOpenRequest) (result WorkspaceOpenResult, returnErr error) {
	access, unlock, err := service.workspaceAccess(ctx, request.Root, request.Workspace, false)
	if err != nil {
		return result, err
	}
	if access.Workspace.State != "running" {
		if err := unlock(); err != nil {
			return result, err
		}
		if _, err := service.Start(ctx, WorkspaceStartRequest{Root: request.Root, Workspace: request.Workspace}); err != nil {
			return result, err
		}
		access, unlock, err = service.workspaceAccess(ctx, request.Root, request.Workspace, true)
		if err != nil {
			return result, err
		}
	}
	locked := true
	defer func() {
		if locked {
			returnErr = errors.Join(returnErr, unlock())
		}
	}()
	if access.Manifest.State != model.StateRunning && access.Manifest.State != model.StateNeedsResolution {
		return result, model.NewError(model.CodeConflict, "workspace is not openable", nil)
	}
	if access.Manifest.ActiveSession != nil {
		return result, model.NewError(model.CodeConflict, "workspace has an active session", nil)
	}
	spec := runtime.ExecSpec{Argv: []string{"/bin/zsh", "-il"}, WorkingDir: "/workspace", User: standardWorkspaceUser, Terminal: request.Terminal}
	spec, err = service.PrepareWorkspaceExecution(ctx, *access.Manifest, spec)
	if err != nil {
		return result, err
	}
	var process runtime.ProcessSpec
	if request.Terminal {
		if request.RunInteractive == nil {
			return result, model.NewError(model.CodeInvalidInput, "interactive workspace runner is required", nil)
		}
		process, err = service.prepareWorkspaceExec(ctx, access.Workspace, spec)
		if err != nil {
			return result, err
		}
	}
	sessionID, err := service.newRunID(service.now().UTC())
	if err != nil {
		return result, model.Wrap(model.CodeInternal, "generate workspace session ID", err)
	}
	workspaceRunID := access.Manifest.RunID
	access.Manifest.ActiveSession = &state.SessionRecord{SessionID: sessionID, Kind: "open", StartedAt: service.now().UTC()}
	if err := service.replaceManifest(ctx, access.Manifest); err != nil {
		return result, err
	}
	result.WorkspaceResult = workspaceResult(*access.Manifest)
	if err := unlock(); err != nil {
		locked = false
		return result, err
	}
	locked = false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), browserCleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, service.clearMatchingSession(cleanupCtx, request.Root, request.Workspace, workspaceRunID, sessionID))
	}()
	var exit runtime.Exit
	if request.Terminal {
		exit, err = request.RunInteractive(ctx, InteractiveChild{Argv: append([]string{process.Executable}, process.Args...), Env: process.Env, Dir: process.Dir, Stdin: request.Stdin, Stdout: request.Stdout, Stderr: request.Stderr})
	} else {
		exit, err = service.execWorkspace(ctx, access.Workspace, spec, runtime.ExecIO{Stdin: request.Stdin, Stdout: request.Stdout, Stderr: request.Stderr})
	}
	if err != nil {
		return result, err
	}
	result.Exit = exit
	return result, nil
}

func (service *WorkspaceService) List(ctx context.Context, request WorkspaceListRequest) (WorkspaceListResult, error) {
	if err := service.ready(ctx); err != nil {
		return WorkspaceListResult{}, err
	}
	_, projectID, err := canonicalWorkspaceRoot(request.Root)
	if err != nil {
		return WorkspaceListResult{}, err
	}
	manifests, err := service.manifests.ListProjectManifests(ctx, projectID)
	if err != nil {
		return WorkspaceListResult{}, err
	}
	result := WorkspaceListResult{Workspaces: make([]WorkspaceSummary, 0, len(manifests))}
	for _, manifest := range manifests {
		if manifest.State == model.StateDeleted {
			continue
		}
		summary := WorkspaceSummary{ProjectID: manifest.ProjectID, Workspace: manifest.Workspace, RunID: manifest.RunID, State: manifest.State, DefaultAgent: manifest.DefaultAgent, Resources: len(manifest.Resources), MutationActive: manifest.Operation != "", Legacy: manifest.Legacy}
		if len(manifest.Git) != 0 {
			summary.SourceBranch, summary.SourceRevision, summary.SourceSnapshot = manifest.Git[0].SourceBranch, manifest.Git[0].SourceRevision, manifest.Git[0].SourceSnapshot
		}
		if manifest.Legacy {
			summary.Warnings = []string{"Legacy — cleanup only"}
		}
		result.Workspaces = append(result.Workspaces, summary)
	}
	sort.Slice(result.Workspaces, func(i, j int) bool { return result.Workspaces[i].Workspace < result.Workspaces[j].Workspace })
	return result, nil
}

func (service *WorkspaceService) Remove(ctx context.Context, request WorkspaceRemoveRequest) (result WorkspaceRemoveResult, returnErr error) {
	if !request.Confirmed {
		return result, model.NewError(model.CodeInvalidInput, "workspace removal requires confirmation", nil)
	}
	if err := service.ready(ctx); err != nil {
		return result, err
	}
	_, projectID, err := canonicalWorkspaceRoot(request.Root)
	if err != nil {
		return result, err
	}
	name, err := checkedWorkspaceName(request.Workspace)
	if err != nil {
		return result, err
	}
	_, _, unlock, err := service.lockWorkspaceProject(ctx, projectID, name)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	manifest, err := service.oneWorkspaceManifest(ctx, projectID, name, true)
	if err != nil {
		return result, err
	}
	result = WorkspaceRemoveResult{ProjectID: manifest.ProjectID, Workspace: manifest.Workspace, RunID: manifest.RunID, State: manifest.State}
	if manifest.Legacy && !request.LegacyResources {
		return result, model.NewError(model.CodeConflict, "legacy resources require explicit legacy cleanup", nil)
	}
	if manifest.ActiveSession != nil {
		return result, model.NewError(model.CodeConflict, "workspace has an active session", nil)
	}
	protected := protectedWorkspaceResults(manifest)
	if !manifest.Legacy {
		owner, ownerErr := workspaceManifestResource(manifest, runtime.ResourceWorkspace, workspaceOwnerRole)
		if ownerErr != nil {
			return result, ownerErr
		}
		snapshot, snapshotErr := service.ownedSnapshot(ctx, manifest, owner, false)
		if snapshotErr != nil {
			return result, model.Wrap(model.CodeUnavailable, "verify workspace before removal", snapshotErr)
		}
		if service.removalGuard == nil {
			return result, model.NewError(model.CodeUnavailable, "workspace removal safety guard is unavailable", nil)
		}
		more, guardErr := service.removalGuard(ctx, manifest, snapshot)
		if guardErr != nil {
			return result, guardErr
		}
		protected = append(protected, more...)
	}
	protected = uniqueSorted(protected)
	if len(protected) != 0 && !request.DiscardUnfetched {
		result.Preserved = protected
		return result, model.NewError(model.CodeConflict, "workspace contains unfetched or unintegrated work", nil)
	}
	if err := service.preflightManifestResources(ctx, manifest); err != nil {
		return result, err
	}
	if !manifest.Legacy {
		if err := service.transitionManifest(ctx, &manifest, model.StateCleaning, "remove", ""); err != nil {
			return result, err
		}
		if err := service.revokeAndRemoveHostAWS(ctx, &manifest); err != nil {
			return result, err
		}
	}
	deleted, preserved, err := service.deleteManifestResources(ctx, &manifest)
	result.DeletedResources, result.Preserved = deleted, preserved
	if err != nil {
		return result, err
	}
	if !manifest.Legacy {
		manifest.UncapturedWork = false
		for index := range manifest.Git {
			manifest.Git[index].FetchedCommit = manifest.Git[index].ResultCommit
			if manifest.Git[index].ResultCommit != "" {
				manifest.Git[index].FetchedHostRef = gitx.RefNamespace + string(manifest.Workspace)
			}
		}
		if err := service.transitionManifest(ctx, &manifest, model.StateDeleted, "remove", ""); err != nil {
			return result, err
		}
	}
	if err := service.manifests.DeleteManifest(ctx, manifest.ProjectID, manifest.Workspace, manifest.RunID); err != nil {
		return result, err
	}
	result.DeletedManifest = true
	result.State = model.StateDeleted
	return result, nil
}

type WorkspaceAttachInfoRequest struct {
	Root      string
	Workspace model.WorkspaceName
}

type WorkspaceAttachInfo struct {
	Workspace model.WorkspaceName
	Container string
}

func (service *WorkspaceService) AttachInfo(ctx context.Context, request WorkspaceAttachInfoRequest) (result WorkspaceAttachInfo, returnErr error) {
	access, unlock, err := service.workspaceAccess(ctx, request.Root, request.Workspace, true)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	return WorkspaceAttachInfo{Workspace: access.Manifest.Workspace, Container: access.Workspace.Name}, nil
}

func (service *WorkspaceService) workspaceAccess(ctx context.Context, root string, name model.WorkspaceName, requireRunning bool) (access workspaceAccess, unlock func() error, returnErr error) {
	if err := service.ready(ctx); err != nil {
		return access, nil, err
	}
	canonical, projectID, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return access, nil, err
	}
	name, err = checkedWorkspaceName(name)
	if err != nil {
		return access, nil, err
	}
	_, _, unlock, err = service.lockWorkspaceProject(ctx, projectID, name)
	if err != nil {
		return access, nil, err
	}
	ok := false
	defer func() {
		if !ok && unlock != nil {
			returnErr = errors.Join(returnErr, unlock())
		}
	}()
	manifest, err := service.oneWorkspaceManifest(ctx, projectID, name, false)
	if err != nil {
		return access, nil, err
	}
	if manifest.Legacy {
		return access, nil, model.NewError(model.CodeConflict, "legacy workspace is cleanup-only", nil)
	}
	if manifest.Operation != "" {
		return access, nil, model.NewError(model.CodeConflict, "workspace has an unfinished "+manifest.Operation+" operation", nil)
	}
	execution, err := service.currentPlan(ctx, canonical, "")
	if err != nil {
		return access, nil, err
	}
	if execution.ExecutableHash != manifest.PlanHash {
		return access, nil, model.NewError(model.CodeUnapproved, "workspace plan differs from current approved configuration", nil)
	}
	owner, err := workspaceManifestResource(manifest, runtime.ResourceWorkspace, workspaceOwnerRole)
	if err != nil {
		return access, nil, err
	}
	snapshot, err := service.ownedSnapshot(ctx, manifest, owner, requireRunning)
	if err != nil {
		return access, nil, err
	}
	network, err := workspaceManifestResource(manifest, runtime.ResourceNetwork, workspaceNetworkRole)
	if err != nil {
		return access, nil, err
	}
	access = workspaceAccess{Manifest: &manifest, Plan: execution, Workspace: snapshot, Network: network}
	ok = true
	return access, unlock, nil
}

// ApprovedProjectPlan resolves the current reusable project defaults and
// requires their executable hash to match the persisted project approval.
// It intentionally does not create, select, or infer a workspace.
func (service *WorkspaceService) ApprovedProjectPlan(ctx context.Context, root string) (plan.ExecutionPlan, error) {
	if err := service.ready(ctx); err != nil {
		return plan.ExecutionPlan{}, err
	}
	canonical, _, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return plan.ExecutionPlan{}, err
	}
	return service.currentPlan(ctx, canonical, "")
}

func (service *WorkspaceService) currentPlan(ctx context.Context, root, approvedHash string) (plan.ExecutionPlan, error) {
	var execution plan.ExecutionPlan
	var err error
	if service.resolvePlan != nil {
		execution, err = service.resolvePlan(ctx, root)
	} else if service.inspection != nil {
		var inspected InspectResult
		inspected, err = service.inspection.Inspect(ctx, InspectRequest{Root: root})
		execution = inspected.Plan
	} else {
		err = errors.New("workspace plan resolver is unavailable")
	}
	if err != nil {
		return execution, err
	}
	if execution.ContractVersion != plan.ContractVersion || execution.Project.CanonicalRoot != root {
		return execution, model.NewError(model.CodeAmbiguous, "resolved workspace plan identity changed", nil)
	}
	if approvedHash != "" && approvedHash != execution.ExecutableHash {
		return execution, model.NewError(model.CodeUnapproved, "--approve-config does not match the current plan", nil)
	}
	if service.approvals != nil && approvedHash == "" {
		record, found, loadErr := service.approvals.LoadApproval(ctx, execution.Project.ID)
		if loadErr != nil {
			return execution, loadErr
		}
		if !found || record.Hash != execution.ExecutableHash {
			return execution, model.NewError(model.CodeUnapproved, "workspace configuration is not approved", nil)
		}
	}
	return execution, nil
}

func (service *WorkspaceService) lockWorkspaceProject(ctx context.Context, projectID model.ProjectID, name model.WorkspaceName) (state.ProjectLock, state.ProjectLock, func() error, error) {
	workspaceLock, err := service.locks.LockWorkspace(ctx, projectID, name)
	if err != nil {
		return nil, nil, nil, model.Wrap(model.CodeConflict, "lock workspace lifecycle", err)
	}
	projectLock, err := service.locks.LockProject(ctx, projectID)
	if err != nil {
		return nil, nil, nil, errors.Join(model.Wrap(model.CodeConflict, "lock project lifecycle", err), workspaceLock.Unlock())
	}
	return workspaceLock, projectLock, func() error { return errors.Join(projectLock.Unlock(), workspaceLock.Unlock()) }, nil
}

func (service *WorkspaceService) oneWorkspaceManifest(ctx context.Context, projectID model.ProjectID, name model.WorkspaceName, includeLegacy bool) (state.Manifest, error) {
	manifests, err := service.manifests.ListProjectManifests(ctx, projectID)
	if err != nil {
		return state.Manifest{}, err
	}
	var found *state.Manifest
	for index := range manifests {
		candidate := manifests[index]
		if candidate.Workspace != name || candidate.State == model.StateDeleted || (!includeLegacy && candidate.Legacy) {
			continue
		}
		if found != nil {
			return state.Manifest{}, model.NewError(model.CodeAmbiguous, "multiple active manifests exist for workspace", nil)
		}
		copy := candidate
		found = &copy
	}
	if found == nil {
		return state.Manifest{}, model.NewError(model.CodeUnavailable, "workspace does not exist", nil)
	}
	return *found, nil
}

func (service *WorkspaceService) replaceManifest(ctx context.Context, manifest *state.Manifest) error {
	if manifest.Legacy {
		return model.NewError(model.CodeConflict, "legacy manifests are cleanup-only", nil)
	}
	expected := manifest.Generation
	manifest.UpdatedAt = service.now().UTC()
	if manifest.UpdatedAt.Before(manifest.CreatedAt) {
		manifest.UpdatedAt = manifest.CreatedAt
	}
	if err := service.manifests.ReplaceManifest(ctx, *manifest, expected); err != nil {
		return err
	}
	manifest.Generation++
	return nil
}

func (service *WorkspaceService) transitionManifest(ctx context.Context, manifest *state.Manifest, next model.WorkspaceState, operation, failure string) error {
	if manifest.State != next {
		if err := manifest.State.Transition(next); err != nil {
			return model.Wrap(model.CodeInternal, "transition workspace manifest", err)
		}
	}
	manifest.State, manifest.Operation, manifest.Failure = next, operation, boundedWorkspaceFailure(failure)
	return service.replaceManifest(ctx, manifest)
}
func (service *WorkspaceService) finalizeLifecycleFailure(ctx context.Context, manifest *state.Manifest, operation string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	durable, found, err := service.manifests.LoadManifest(cleanupCtx, manifest.ProjectID, manifest.Workspace, manifest.RunID)
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "load workspace mutation for failure finalization", err)
	}
	if !found {
		return model.NewError(model.CodeUnavailable, "workspace mutation manifest disappeared during failure finalization", nil)
	}
	*manifest = durable
	failure := boundedWorkspaceFailure(cause.Error())
	next := model.StateFailed
	owner, ownerErr := workspaceManifestResource(durable, runtime.ResourceWorkspace, workspaceOwnerRole)
	if ownerErr == nil {
		snapshot, snapshotErr := service.ownedSnapshot(cleanupCtx, durable, owner, false)
		if snapshotErr == nil {
			switch snapshot.State {
			case "running":
				next = model.StateRunning
				if workspaceHasConflict(durable) {
					next = model.StateNeedsResolution
				}
			case "created", "stopped":
				next = model.StateStopped
				if workspaceHasConflict(durable) {
					next = model.StateNeedsResolution
				}
			}
		} else {
			failure = boundedWorkspaceFailure(failure + "; reconcile runtime: " + snapshotErr.Error())
		}
	} else {
		failure = boundedWorkspaceFailure(failure + "; reconcile ownership: " + ownerErr.Error())
	}
	if durable.State != next && !durable.State.CanTransitionTo(next) {
		next = durable.State
	}
	return service.transitionManifest(cleanupCtx, manifest, next, operation, failure)
}

func (service *WorkspaceService) cleanupSessionBrowser(ctx context.Context, manifest *state.Manifest) error {
	index, found := manifestResourceIndex(manifest.Resources, runtime.ResourceBrowser)
	if !found {
		return nil
	}
	record := manifest.Resources[index]
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), browserCleanupTimeout)
	defer cancel()
	browser, err := service.runtime.Inspect(cleanupCtx, runtime.ResourceID(record.ExpectedID))
	if errors.Is(err, runtime.ErrResourceNotFound) {
		return service.markSessionBrowserDeleted(cleanupCtx, manifest, index)
	}
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "inspect session browser during workspace lifecycle", err)
	}
	classification := ownership.Classify(&record, &browser)
	if !classification.DeleteAllowed {
		return model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	if browser.State == "running" {
		if err := service.runtime.Stop(cleanupCtx, browser, runtime.StopPolicy{TimeoutSeconds: workspaceStopSeconds, Signal: "TERM"}); err != nil {
			return model.Wrap(model.CodeUnavailable, "stop session browser during workspace lifecycle", err)
		}
		browser, err = service.runtime.Inspect(cleanupCtx, browser.ID)
		if errors.Is(err, runtime.ErrResourceNotFound) {
			return service.markSessionBrowserDeleted(cleanupCtx, manifest, index)
		}
		if err != nil {
			return model.Wrap(model.CodeUnavailable, "inspect stopped session browser", err)
		}
		classification = ownership.Classify(&record, &browser)
		if !classification.DeleteAllowed {
			return model.NewError(model.CodeAmbiguous, classification.Reason, nil)
		}
	}
	if err := service.runtime.Delete(cleanupCtx, browser); err != nil {
		return model.Wrap(model.CodeUnavailable, "delete session browser during workspace lifecycle", err)
	}
	return service.markSessionBrowserDeleted(cleanupCtx, manifest, index)
}

func (service *WorkspaceService) markSessionBrowserDeleted(ctx context.Context, manifest *state.Manifest, index int) error {
	record := manifest.Resources[index]
	record.Created = true
	record.RuntimeID = record.ExpectedID
	record.Deleted = true
	record.Absent = true
	manifest.Resources[index] = record
	return service.replaceManifest(ctx, manifest)
}

func (service *WorkspaceService) clearActiveSession(ctx context.Context, manifest *state.Manifest) error {
	if manifest.ActiveSession == nil {
		return nil
	}
	manifest.ActiveSession = nil
	return service.replaceManifest(ctx, manifest)
}

func (service *WorkspaceService) clearMatchingSession(ctx context.Context, root string, name model.WorkspaceName, workspaceRunID, sessionID model.RunID) (returnErr error) {
	_, projectID, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return err
	}
	_, _, unlock, err := service.lockWorkspaceProject(ctx, projectID, name)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	manifest, err := service.oneWorkspaceManifest(ctx, projectID, name, false)
	if model.ErrorCodeOf(err) == model.CodeUnavailable {
		return nil
	}
	if err != nil {
		return err
	}
	if manifest.RunID != workspaceRunID || manifest.ActiveSession == nil || manifest.ActiveSession.SessionID != sessionID {
		return nil
	}
	manifest.ActiveSession = nil
	return service.replaceManifest(ctx, &manifest)
}

func (service *WorkspaceService) prepareWorkspaceExec(ctx context.Context, snapshot runtime.ResourceSnapshot, spec runtime.ExecSpec) (runtime.ProcessSpec, error) {
	return service.runtime.PrepareExec(ctx, snapshot, spec)
}

func (service *WorkspaceService) execWorkspace(ctx context.Context, snapshot runtime.ResourceSnapshot, spec runtime.ExecSpec, execIO runtime.ExecIO) (runtime.Exit, error) {
	return service.runtime.Exec(ctx, snapshot, spec, execIO)
}

func (service *WorkspaceService) createResource(ctx context.Context, manifest *state.Manifest, index int, create func() (runtime.Resource, error)) error {
	resource, err := create()
	if err != nil {
		return err
	}
	record := &manifest.Resources[index]
	if string(resource.ID) != record.ExpectedID || resource.Name != record.Name || string(resource.Kind) != record.Kind {
		return model.NewError(model.CodeUnavailable, "runtime resource differs from write-ahead identity", nil)
	}
	record.RuntimeID, record.Created = string(resource.ID), true
	return service.replaceManifest(ctx, manifest)
}

func (service *WorkspaceService) ownedSnapshot(ctx context.Context, manifest state.Manifest, record state.ResourceRecord, requireRunning bool) (runtime.ResourceSnapshot, error) {
	snapshot, err := service.runtime.Inspect(ctx, runtime.ResourceID(record.ExpectedID))
	if err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	classification := ownership.Classify(&record, &snapshot)
	if !classification.AdoptAllowed {
		return runtime.ResourceSnapshot{}, model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	if requireRunning && snapshot.State != "running" {
		return runtime.ResourceSnapshot{}, model.NewError(model.CodeConflict, "workspace is not running", nil)
	}
	return snapshot, nil
}

func (service *WorkspaceService) preflightManifestResources(ctx context.Context, manifest state.Manifest) error {
	for index := len(manifest.Resources) - 1; index >= 0; index-- {
		record := manifest.Resources[index]
		if record.Deleted || record.Absent {
			continue
		}
		snapshot, err := service.runtime.Inspect(ctx, runtime.ResourceID(record.ExpectedID))
		if errors.Is(err, runtime.ErrResourceNotFound) {
			continue
		}
		if err != nil {
			return model.Wrap(model.CodeUnavailable, "verify workspace resources before removal", err)
		}
		classification := ownership.Classify(&record, &snapshot)
		if !classification.DeleteAllowed {
			return model.NewError(model.CodeAmbiguous, classification.Reason, nil)
		}
	}
	return nil
}

func (service *WorkspaceService) deleteManifestResources(ctx context.Context, manifest *state.Manifest) (int, []string, error) {
	deleted := 0
	preserved := make([]string, 0)
	var result error
	for index := len(manifest.Resources) - 1; index >= 0; index-- {
		record := &manifest.Resources[index]
		if record.Deleted || record.Absent {
			continue
		}
		snapshot, err := service.runtime.Inspect(ctx, runtime.ResourceID(record.ExpectedID))
		if errors.Is(err, runtime.ErrResourceNotFound) {
			if !manifest.Legacy {
				record.Absent = !record.Created
				record.Deleted = record.Created
				result = errors.Join(result, service.replaceManifest(ctx, manifest))
			}
			continue
		}
		if err != nil {
			preserved = append(preserved, record.Name)
			result = errors.Join(result, err)
			continue
		}
		classification := ownership.Classify(record, &snapshot)
		if !classification.DeleteAllowed {
			preserved = append(preserved, record.Name)
			result = errors.Join(result, model.NewError(model.CodeAmbiguous, classification.Reason, nil))
			continue
		}
		if (snapshot.Kind == runtime.ResourceWorkspace || snapshot.Kind == runtime.ResourceBrowser) && snapshot.State == "running" {
			if err := service.runtime.Stop(ctx, snapshot, runtime.StopPolicy{TimeoutSeconds: workspaceStopSeconds, Signal: "TERM"}); err != nil {
				preserved = append(preserved, record.Name)
				result = errors.Join(result, err)
				continue
			}
		}
		if err := service.runtime.Delete(ctx, snapshot); err != nil {
			preserved = append(preserved, record.Name)
			result = errors.Join(result, err)
			continue
		}
		deleted++
		if !manifest.Legacy {
			record.RuntimeID, record.Created, record.Deleted = string(snapshot.ID), true, true
			result = errors.Join(result, service.replaceManifest(ctx, manifest))
		}
	}
	return deleted, uniqueSorted(preserved), result
}

func (service *WorkspaceService) rollbackCreate(ctx context.Context, manifest *state.Manifest) error {
	if manifest.State != model.StateCleaning {
		_ = service.transitionManifest(ctx, manifest, model.StateCleaning, "remove", "")
	}
	if err := service.revokeAndRemoveHostAWS(ctx, manifest); err != nil {
		return err
	}
	_, preserved, err := service.deleteManifestResources(ctx, manifest)
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "workspace rollback preserved resources: "+strings.Join(preserved, ", "), err)
	}
	manifest.UncapturedWork = false
	if transitionErr := service.transitionManifest(ctx, manifest, model.StateDeleted, "remove", ""); transitionErr != nil {
		return transitionErr
	}
	return service.manifests.DeleteManifest(ctx, manifest.ProjectID, manifest.Workspace, manifest.RunID)
}

func (service *WorkspaceService) prepareWorkspaceSources(ctx context.Context, execution plan.ExecutionPlan, name model.WorkspaceName, expectedBranch, expectedHeadRevision string, snapshot bool) ([]gitx.SourceArtifact, []string, error) {
	artifacts := make([]gitx.SourceArtifact, 0, len(execution.Repositories))
	warnings := make([]string, 0)
	for _, repository := range execution.Repositories {
		artifact, err := service.git.PrepareSource(ctx, gitx.SourceRequest{
			Repository:   gitx.Repository{Name: repository.Name, HostPath: repository.HostPath, GuestPath: repository.GuestPath},
			ApprovedRoot: execution.Project.CanonicalRoot, Workspace: string(name), TempRoot: service.tempRoot, Snapshot: snapshot,
		})
		if err != nil {
			return nil, nil, errors.Join(err, service.removeSourceArtifacts(artifacts))
		}
		if err := service.git.VerifyBundle(ctx, artifact.BundlePath, artifact.BundleDigest); err != nil {
			return nil, nil, errors.Join(err, service.git.RemoveArtifact(artifact.BundlePath), service.removeSourceArtifacts(artifacts))
		}
		if expectedBranch != "" && artifact.SourceBranch != expectedBranch {
			return nil, nil, errors.Join(model.NewError(model.CodeConflict, "selected source branch changed", nil), service.git.RemoveArtifact(artifact.BundlePath), service.removeSourceArtifacts(artifacts))
		}
		if expectedHeadRevision != "" && artifact.SourceHeadRevision != expectedHeadRevision {
			return nil, nil, errors.Join(model.NewError(model.CodeConflict, "selected source revision changed", nil), service.git.RemoveArtifact(artifact.BundlePath), service.removeSourceArtifacts(artifacts))
		}
		artifacts = append(artifacts, artifact)
		if artifact.SourceSnapshot {
			warnings = append(warnings, repository.Name+": source snapshot included final tracked content and nonignored untracked files")
		}
		if artifact.WarnUntracked {
			warnings = append(warnings, repository.Name+": untracked files were not transferred")
		}
		if artifact.WarnIgnored {
			warnings = append(warnings, repository.Name+": ignored files were not transferred")
		}
	}
	if len(artifacts) == 0 {
		return nil, nil, model.NewError(model.CodeInvalidInput, "workspace plan has no Git repositories", nil)
	}
	return artifacts, warnings, nil
}

func (service *WorkspaceService) removeSourceArtifacts(artifacts []gitx.SourceArtifact) error {
	var result error
	for _, artifact := range artifacts {
		result = errors.Join(result, service.git.RemoveArtifact(artifact.BundlePath))
	}
	return result
}

func plannedWorkspaceManifest(execution plan.ExecutionPlan, name model.WorkspaceName, runID model.RunID, defaultAgent string, artifacts []gitx.SourceArtifact, now time.Time) (state.Manifest, error) {
	manifest := state.Manifest{Version: state.ManifestVersion, Generation: 1, ProjectID: execution.Project.ID, CanonicalRoot: execution.Project.CanonicalRoot, Workspace: name, RunID: runID, PlanHash: execution.ExecutableHash, DefaultAgent: defaultAgent, State: model.StatePlanned, Operation: "create", CreatedAt: now.UTC(), UpdatedAt: now.UTC(), Git: make([]state.GitRecord, len(artifacts))}
	if execution.AWS.Mode == plan.AWSModeHostDefault {
		manifest.AWSGrant = &state.AWSGrantRecord{}
	}
	resources := []struct {
		kind runtime.ResourceKind
		role string
	}{{runtime.ResourceNetwork, workspaceNetworkRole}}
	for _, volume := range privateWorkspaceVolumes {
		resources = append(resources, struct {
			kind runtime.ResourceKind
			role string
		}{runtime.ResourceVolume, volume.role})
	}
	for _, volume := range execution.Volumes {
		resources = append(resources, struct {
			kind runtime.ResourceKind
			role string
		}{runtime.ResourceVolume, workspaceConfiguredVolumeRole(volume.Name)})
	}
	resources = append(resources, struct {
		kind runtime.ResourceKind
		role string
	}{runtime.ResourceWorkspace, workspaceOwnerRole})
	for _, item := range resources {
		identity, err := ownership.NewIdentity(manifest.ProjectID, manifest.CanonicalRoot, manifest.Workspace, manifest.RunID, item.kind, item.role)
		if err != nil {
			return manifest, err
		}
		record := identity.ManifestRecord()
		record.Persistent = item.kind == runtime.ResourceVolume
		manifest.Resources = append(manifest.Resources, record)
	}
	for index, artifact := range artifacts {
		manifest.Git[index] = state.GitRecord{Repository: artifact.Repository.Name, HostPath: artifact.Repository.HostPath, GuestPath: artifact.Repository.GuestPath, Identity: artifact.Repository.Identity, SourceBranch: artifact.SourceBranch, SourceRevision: artifact.SourceRevision, SourceSnapshot: artifact.SourceSnapshot, SourceHeadRevision: artifact.SourceHeadRevision, SourceTree: artifact.SourceTree, TrackedFingerprint: artifact.TrackedFingerprint, WarnUntracked: artifact.WarnUntracked, WarnIgnored: artifact.WarnIgnored, WorkspaceBranch: "dsx/" + string(name), SourceBundleDigest: artifact.BundleDigest}
	}
	return manifest, state.ValidateManifest(manifest)
}

func workspaceSpec(execution plan.ExecutionPlan, manifest state.Manifest, image runtime.Image, network, owner state.ResourceRecord, helper, hostAWSMirror runtime.HostPath) (runtime.WorkspaceSpec, error) {
	mounts := make([]runtime.Mount, 0, len(manifest.Resources)+len(execution.Mounts)+2)
	for _, volume := range privateWorkspaceVolumes {
		record, err := workspaceManifestResource(manifest, runtime.ResourceVolume, volume.role)
		if err != nil {
			return runtime.WorkspaceSpec{}, err
		}
		mounts = append(mounts, runtime.Mount{Source: record.Name, Target: string(volume.target), Type: "volume", Authority: runtime.MountAuthorityVolume})
	}
	for _, volume := range execution.Volumes {
		record, err := workspaceManifestResource(manifest, runtime.ResourceVolume, workspaceConfiguredVolumeRole(volume.Name))
		if err != nil {
			return runtime.WorkspaceSpec{}, err
		}
		for _, private := range privateWorkspaceVolumes {
			if workspaceGuestPathsOverlap(volume.Target, string(private.target)) {
				return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "configured volume overlaps private workspace storage", nil)
			}
		}
		mounts = append(mounts, runtime.Mount{Source: record.Name, Target: volume.Target, Type: "volume", Authority: runtime.MountAuthorityVolume})
	}
	reviewed, err := reviewedRuntimeMounts(execution)
	if err != nil {
		return runtime.WorkspaceSpec{}, err
	}
	for _, mount := range reviewed {
		for _, existing := range mounts {
			if workspaceGuestPathsOverlap(mount.Target, existing.Target) {
				return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "reviewed host mount overlaps workspace volume target", nil)
			}
		}
		mounts = append(mounts, mount)
	}
	mounts = append(mounts, runtime.Mount{Source: filepath.Dir(string(helper)), Target: DefaultGuestHelperDirectory, Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityGuestHelper})
	if execution.AWS.Mode == plan.AWSModeHostDefault {
		if hostAWSMirror == "" {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeUnavailable, "host AWS workspace publication path is unavailable", nil)
		}
		mounts = append(mounts, runtime.Mount{Source: string(hostAWSMirror), Target: plan.AWSGuestDestination, Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityHostAWSMirror})
	}
	uid, gid, err := guestUserIdentity(standardWorkspaceUser)
	if err != nil {
		return runtime.WorkspaceSpec{}, err
	}
	ports := make([]runtime.PortRequest, len(execution.Ports))
	for index, port := range execution.Ports {
		ports[index] = runtime.PortRequest{HostIP: port.HostIP, HostPort: port.HostPort, GuestPort: port.GuestPort, Protocol: port.Protocol}
	}
	return runtime.WorkspaceSpec{Name: owner.Name, CanonicalRoot: runtime.HostPath(execution.Project.CanonicalRoot), HostAWSMirrorSource: hostAWSMirror, Image: image, Entrypoint: []string{DefaultGuestHelperPath, "serve", "--socket", DefaultGuestSocketPath, "--child-uid", uid, "--child-gid", gid}, WorkingDir: "/workspace", User: standardWorkspaceUser, Mounts: mounts, Networks: []string{network.Name}, Ports: ports, Labels: workspaceRuntimeLabels(owner.Labels), CPUs: execution.Limits.CPUs, MemoryBytes: execution.Limits.MemoryBytes}, nil
}

func enforceWorkspaceLimit(manifests []state.Manifest, maximum int, exclude model.RunID, operation string) error {
	if maximum < 1 {
		return model.NewError(model.CodeInvalidInput, "maximum concurrent workspaces must be positive", nil)
	}
	active := 0
	for _, manifest := range manifests {
		if manifest.Legacy || manifest.RunID == exclude || manifest.State == model.StateStopped || manifest.State == model.StateDeleted {
			continue
		}
		active++
	}
	if active >= maximum {
		return model.NewError(model.CodeConflict, operation+" would exceed maximum concurrent workspaces", nil)
	}
	return nil
}

func reviewedRuntimeMounts(execution plan.ExecutionPlan) ([]runtime.Mount, error) {
	result := make([]runtime.Mount, 0, len(execution.Mounts))
	projectRoot := filepath.Clean(execution.Project.CanonicalRoot)
	protectedTargets := []string{"/workspace", "/auth", DefaultGuestHelperPath, "/run/dsx/aws"}
	for _, mount := range execution.Mounts {
		if mount.SourceType != "host" || !mount.ReadOnly {
			return nil, model.NewError(model.CodeInvalidInput, "workspace mount must be a reviewed read-only host mount", nil)
		}
		current, err := resolveHostMount(mount.Source)
		if err != nil {
			return nil, model.Wrap(model.CodeInvalidInput, "revalidate reviewed host mount", err)
		}
		if current.CanonicalPath != mount.Source || current.Identity != mount.SourceIdentity {
			return nil, model.NewError(model.CodeConflict, "reviewed host mount identity changed", nil)
		}
		if pathWithin(projectRoot, current.CanonicalPath) || pathWithin(current.CanonicalPath, projectRoot) {
			return nil, model.NewError(model.CodeInvalidInput, "reviewed host mount overlaps project source", nil)
		}
		target := path.Clean(mount.Target)
		if !path.IsAbs(target) || target != mount.Target || target == "/" {
			return nil, model.NewError(model.CodeInvalidInput, "reviewed host mount target is invalid", nil)
		}
		for _, protected := range protectedTargets {
			if workspaceGuestPathsOverlap(target, protected) {
				return nil, model.NewError(model.CodeInvalidInput, "reviewed host mount overlaps protected guest storage", nil)
			}
		}
		for _, existing := range result {
			if workspaceGuestPathsOverlap(target, existing.Target) {
				return nil, model.NewError(model.CodeInvalidInput, "reviewed host mount targets overlap", nil)
			}
		}
		result = append(result, runtime.Mount{Source: current.CanonicalPath, Target: target, Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityReviewedHost})
	}
	return result, nil
}

// PrepareStandardImage materializes and verifies the embedded development image.
// Release plans already carry an immutable published reference and are only pulled.
func (service *WorkspaceService) PrepareStandardImage(ctx context.Context, execution plan.ExecutionPlan) error {
	if !execution.Image.Standard {
		return model.NewError(model.CodeInvalidInput, "prepare standard image: plan does not select DSX Standard", nil)
	}
	_, err := service.ensureWorkspaceImage(ctx, execution)
	return err
}

func (service *WorkspaceService) ensureWorkspaceImage(ctx context.Context, execution plan.ExecutionPlan) (image runtime.Image, returnErr error) {
	spec := runtime.ImageSpec{Reference: execution.Image.Reference, Target: execution.Image.Target, Reuse: true}
	for _, argument := range execution.Image.BuildArgs {
		spec.BuildArgs = append(spec.BuildArgs, runtime.Label{Key: argument.Key, Value: argument.Value})
	}
	if execution.Image.Standard && execution.Image.Reference == "" {
		if execution.Image.InputDigest != agentimage.InputDigest() {
			return runtime.Image{}, model.NewError(model.CodeUnapproved, "approved standard image build authority is inconsistent", nil)
		}
		stageRoot, digest, err := stageStandardImage(ctx, execution.Project.CanonicalRoot)
		if err != nil {
			return runtime.Image{}, model.Wrap(model.CodeUnavailable, "stage DSX Standard image", err)
		}
		defer func() {
			returnErr = errors.Join(returnErr, os.RemoveAll(stageRoot))
		}()
		spec.Reference = "dsx.local/standard:" + digest[:12]
		spec.Context = runtime.HostPath(stageRoot)
		spec.File = runtime.HostPath(filepath.Join(stageRoot, agentimage.BuildFile))
		spec.Labels = []runtime.Label{{Key: "dev.dsx.standard-input", Value: digest}}
	} else if execution.Image.Context != "" {
		spec.Context = runtime.HostPath(filepath.Join(execution.Project.CanonicalRoot, filepath.FromSlash(execution.Image.Context)))
		spec.File = runtime.HostPath(filepath.Join(string(spec.Context), filepath.FromSlash(execution.Image.File)))
	}
	return service.runtime.EnsureImage(ctx, spec)
}

func (service *WorkspaceService) bootstrapWorkspace(ctx context.Context, snapshot runtime.ResourceSnapshot, manifest *state.Manifest, artifacts []gitx.SourceArtifact) (returnErr error) {
	staging := "/tmp/dsx-source-" + string(manifest.RunID)
	if _, err := service.execWorkspace(ctx, snapshot, runtime.ExecSpec{Argv: []string{"/bin/mkdir", "-p", staging}, WorkingDir: "/workspace", User: standardWorkspaceUser}, runtime.ExecIO{}); err != nil {
		return err
	}
	defer func() {
		_, err := service.execWorkspace(context.WithoutCancel(ctx), snapshot, runtime.ExecSpec{Argv: []string{"/bin/rm", "-rf", "--", staging}, WorkingDir: "/workspace", User: standardWorkspaceUser}, runtime.ExecIO{})
		returnErr = errors.Join(returnErr, err)
	}()
	for index, artifact := range artifacts {
		bundle := path.Join(staging, fmt.Sprintf("source-%d.bundle", index))
		if err := service.runtime.CopyTo(ctx, snapshot, runtime.HostPath(artifact.BundlePath), runtime.GuestPath(bundle)); err != nil {
			return err
		}
		commands := [][]string{{"/usr/bin/git", "init", "--quiet", artifact.Repository.GuestPath}, {"/usr/bin/git", "-C", artifact.Repository.GuestPath, "fetch", "--no-tags", "--no-write-fetch-head", "--", bundle, artifact.BundleRef}, {"/usr/bin/git", "-C", artifact.Repository.GuestPath, "checkout", "-B", manifest.Git[index].WorkspaceBranch, artifact.SourceRevision}}
		if _, err := service.execWorkspace(ctx, snapshot, runtime.ExecSpec{Argv: []string{"/bin/mkdir", "-p", path.Dir(artifact.Repository.GuestPath)}, WorkingDir: "/workspace", User: standardWorkspaceUser}, runtime.ExecIO{}); err != nil {
			return err
		}
		for _, argv := range commands {
			exit, err := service.execWorkspace(ctx, snapshot, runtime.ExecSpec{Argv: argv, WorkingDir: "/workspace", User: standardWorkspaceUser}, runtime.ExecIO{})
			if err != nil {
				return err
			}
			if exit.Code == nil || *exit.Code != 0 || exit.Signal != "" {
				return model.NewError(model.CodeUnavailable, "guest Git bootstrap failed", nil)
			}
		}
	}
	return nil
}

func workspaceResult(manifest state.Manifest) WorkspaceResult {
	return WorkspaceResult{ProjectID: manifest.ProjectID, Workspace: manifest.Workspace, RunID: manifest.RunID, State: manifest.State}
}
func approvedAgent(allowed []string, candidate string) bool {
	for _, value := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
func workspaceHasConflict(manifest state.Manifest) bool {
	for _, record := range manifest.Git {
		if record.Conflict {
			return true
		}
	}
	return false
}
func checkedWorkspaceName(name model.WorkspaceName) (model.WorkspaceName, error) {
	parsed, err := model.ParseWorkspaceName(string(name))
	if err != nil || parsed != name {
		return "", model.NewError(model.CodeInvalidInput, "invalid workspace name", err)
	}
	return parsed, nil
}
func canonicalWorkspaceRoot(root string) (string, model.ProjectID, error) {
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", "", model.Wrap(model.CodeInvalidInput, "resolve project root", err)
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return "", "", model.Wrap(model.CodeInvalidInput, "canonicalize project root", err)
	}
	id, err := model.NewProjectID(canonical)
	return canonical, id, err
}
func projectIDForRoot(root string) (model.ProjectID, error) {
	_, projectID, err := canonicalWorkspaceRoot(root)
	return projectID, err
}
func workspaceRuntimeLabels(labels []state.OwnershipLabel) []runtime.Label {
	result := make([]runtime.Label, len(labels))
	for i, label := range labels {
		result[i] = runtime.Label{Key: label.Key, Value: label.Value}
	}
	return result
}
func workspaceManifestResource(manifest state.Manifest, kind runtime.ResourceKind, role string) (state.ResourceRecord, error) {
	for _, resource := range manifest.Resources {
		if resource.Kind == string(kind) && resource.Role == role {
			return resource, nil
		}
	}
	return state.ResourceRecord{}, model.NewError(model.CodeInternal, "workspace manifest is missing "+role, nil)
}
func workspaceResourceIndex(manifest state.Manifest, kind runtime.ResourceKind, role string) int {
	for index, resource := range manifest.Resources {
		if resource.Kind == string(kind) && resource.Role == role {
			return index
		}
	}
	return -1
}
func workspaceConfiguredVolumeRole(name string) string {
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("v-%x", digest[:3])
}
func workspaceGuestPathsOverlap(left, right string) bool {
	left = path.Clean(left)
	right = path.Clean(right)
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}
func protectedWorkspaceResults(manifest state.Manifest) []string {
	protected := make([]string, 0)
	if manifest.UncapturedWork {
		protected = append(protected, "uncaptured workspace work")
	}
	for _, record := range manifest.Git {
		if record.Conflict {
			protected = append(protected, record.Repository+": rebase conflict")
		}
		if record.HasResultWork() && !record.ResultFetched() {
			protected = append(protected, record.Repository+": unfetched result")
		}
	}
	return protected
}
func uniqueSorted(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if value != "" && (len(result) == 0 || result[len(result)-1] != value) {
			result = append(result, value)
		}
	}
	return result
}
func boundedWorkspaceFailure(value string) string {
	const limit = 4096
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
