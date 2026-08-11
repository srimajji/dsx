package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/hostsource"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/ownership"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/ports"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

const (
	liveSandboxName            = model.SandboxName("main")
	workspaceRole              = "workspace"
	networkRole                = "network"
	workspaceGuestRoot         = runtime.GuestPath("/workspace")
	lifecycleStopSeconds       = 10
	lifecycleGuestReadyTimeout = 2 * time.Minute
	lifecycleGuestPollInterval = 100 * time.Millisecond
)

type SandboxAuthCleaner func(context.Context, model.ProjectID, model.SandboxName) error
type CloneCleanupRecovery func(context.Context, runtime.ResourceSnapshot, *state.Manifest) error
type CloneCleanupFetchedVerifier func(context.Context, state.Manifest) error
type CloneCleanupIdentityValidator func(context.Context, state.Manifest) error

var errTerminalRequiredGuestFailure = errors.New("terminal required guest process failure")

type LifecycleDependencies struct {
	Inspection        *InspectionService
	Approvals         state.ApprovalRepository
	Manifests         state.ManifestRepository
	Locks             state.LockRepository
	Runtime           runtime.Adapter
	Now               func() time.Time
	TimingRecorder    PhaseTimingRecorder
	TimingClock       func() time.Time
	NewRunID          func(time.Time) (model.RunID, error)
	User              func() string
	Guest             GuestController
	GuestHelperSource func() (runtime.HostPath, error)
	CleanSandboxAuth  SandboxAuthCleaner
	BridgeLeases      bridge.LeaseManager
	LeappMirrors      bridge.LeappMirrorManager
}

type LifecycleService struct {
	inspection                    *InspectionService
	approvals                     state.ApprovalRepository
	manifests                     state.ManifestRepository
	locks                         state.LockRepository
	runtime                       runtime.Adapter
	now                           func() time.Time
	timingRecorder                PhaseTimingRecorder
	timingClock                   func() time.Time
	newRunID                      func(time.Time) (model.RunID, error)
	user                          func() string
	guest                         GuestController
	guestHelperSource             func() (runtime.HostPath, error)
	cleanSandboxAuth              SandboxAuthCleaner
	hostBridges                   hostBridgeRuntime
	bridgeLeases                  bridge.LeaseManager
	leappMirrors                  bridge.LeappMirrorManager
	cloneCleanupRecovery          CloneCleanupRecovery
	cloneCleanupFetchedVerifier   CloneCleanupFetchedVerifier
	cloneCleanupIdentityValidator CloneCleanupIdentityValidator
}

func NewLifecycleService(dependencies LifecycleDependencies) *LifecycleService {
	now := dependencies.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newRunID := dependencies.NewRunID
	if newRunID == nil {
		newRunID = model.NewRunID
	}
	user := dependencies.User
	if user == nil {
		user = func() string { return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) }
	}
	guest := dependencies.Guest
	if guest == nil && dependencies.Runtime != nil {
		guest = NewGuestClientWithDependencies(GuestClientDependencies{Adapter: dependencies.Runtime, User: user()})
	}
	return &LifecycleService{
		inspection:        dependencies.Inspection,
		approvals:         dependencies.Approvals,
		manifests:         dependencies.Manifests,
		locks:             dependencies.Locks,
		runtime:           dependencies.Runtime,
		now:               now,
		timingRecorder:    dependencies.TimingRecorder,
		timingClock:       dependencies.TimingClock,
		newRunID:          newRunID,
		user:              user,
		guest:             guest,
		guestHelperSource: dependencies.GuestHelperSource,
		hostBridges:       defaultHostBridgeRuntime(),
		bridgeLeases:      dependencies.BridgeLeases,
		leappMirrors:      dependencies.LeappMirrors,
		cleanSandboxAuth:  dependencies.CleanSandboxAuth,
	}
}

type StartRequest struct {
	Root           string
	ApproveConfig  string
	Interactive    bool
	FinalConfirmed bool
	Agent          string
	Browser        *bool
	Sandbox        string
	Mode           model.WorkspaceMode
}

// StartProgressStep identifies a fixed, secret-free workspace startup milestone.
type StartProgressStep string

const (
	StartProgressValidate  StartProgressStep = "validate"
	StartProgressImage     StartProgressStep = "image"
	StartProgressResources StartProgressStep = "resources"
	StartProgressWorkspace StartProgressStep = "workspace"
	StartProgressServices  StartProgressStep = "services"
	StartProgressReady     StartProgressStep = "ready"
)

// StartProgressReporter receives ordered workspace startup milestones.
type StartProgressReporter func(StartProgressStep)

type StartResult struct {
	ProjectID   model.ProjectID    `json:"project_id"`
	Sandbox     model.SandboxName  `json:"sandbox"`
	RunID       model.RunID        `json:"run_id"`
	State       model.SandboxState `json:"state"`
	Existing    bool               `json:"existing"`
	URLs        []string           `json:"urls,omitempty"`
	hostBridges *hostBridgeSession
}

type StopRequest struct {
	Root    string
	Sandbox string
}

type StopResult struct {
	ProjectID model.ProjectID    `json:"project_id"`
	Sandbox   model.SandboxName  `json:"sandbox"`
	RunID     model.RunID        `json:"run_id"`
	State     model.SandboxState `json:"state"`
}
type CleanRequest struct {
	Root             string
	Sandbox          string
	All              bool
	Confirmed        bool
	DiscardUnfetched bool
}
type CleanResult struct {
	ProjectID        model.ProjectID `json:"project_id,omitempty"`
	Projects         int             `json:"projects"`
	DeletedManifests int             `json:"deleted_manifests"`
	DeletedResources int             `json:"deleted_resources"`
	Preserved        []string        `json:"preserved,omitempty"`
}

type ListRequest struct {
	Root string
}

type SandboxSummary struct {
	ProjectID model.ProjectID     `json:"project_id"`
	Sandbox   model.SandboxName   `json:"sandbox"`
	RunID     model.RunID         `json:"run_id"`
	Mode      model.WorkspaceMode `json:"mode"`
	State     model.SandboxState  `json:"state"`
	Resources int                 `json:"resources"`
	Warnings  []string            `json:"warnings,omitempty"`
	URLs      []string            `json:"urls,omitempty"`
}

type ListResult struct {
	Sandboxes []SandboxSummary `json:"sandboxes"`
}

type InteractiveChildRunner func(context.Context, InteractiveChild) (runtime.Exit, error)
type ShellReady struct {
	URLs      []string
	Processes []guestproto.ProcessStatus
}

type ShellReadyHandler func(ShellReady) error

type ShellRequest struct {
	ApproveConfig         string
	Root                  string
	Agent                 string
	Argv                  []string
	Env                   map[string]string
	SecretEnvironmentKeys []string
	Terminal              bool
	Stdin                 io.Reader
	Stdout                io.Writer
	Stderr                io.Writer
	RunInteractive        InteractiveChildRunner
	BeforeExec            ShellReadyHandler
}

type ShellResult struct {
	Exit runtime.Exit `json:"exit"`
}

func (service *LifecycleService) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	return service.start(ctx, request, nil)
}

// StartWithProgress runs the same lifecycle transaction as Start while
// reporting fixed milestones that are safe for user-facing presentation.
func (service *LifecycleService) StartWithProgress(ctx context.Context, request StartRequest, report StartProgressReporter) (StartResult, error) {
	return service.start(ctx, request, report)
}

func (service *LifecycleService) start(ctx context.Context, request StartRequest, report StartProgressReporter) (result StartResult, err error) {
	if err := service.ready(ctx); err != nil {
		return result, err
	}
	reportStartProgress(report, StartProgressValidate)
	timing := beginPhase(service.timingRecorder, service.timingClock, PhaseStart)
	defer timing.Stop()
	inspected, err := service.inspectApproved(ctx, request)
	if err != nil {
		return result, err
	}
	projectID := inspected.Plan.Project.ID
	lock, err := service.locks.LockProject(ctx, projectID)
	if err != nil {
		return result, model.Wrap(model.CodeConflict, "lock project lifecycle", err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, model.Wrap(model.CodeInternal, "unlock project lifecycle", unlockErr))
		}
	}()
	result, err = service.startLocked(ctx, request, inspected, report)
	if err == nil {
		reportStartProgress(report, StartProgressReady)
	}
	return result, err
}

func reportStartProgress(report StartProgressReporter, step StartProgressStep) {
	if report != nil {
		report(step)
	}
}

// RecreatePorts replaces only the live workspace resource so create-time port
// publication can change without deleting the project network or volumes.
func (service *LifecycleService) RecreatePorts(ctx context.Context, request StartRequest) (result StartResult, returnErr error) {
	if err := service.ready(ctx); err != nil {
		return result, err
	}
	inspected, err := service.inspectApproved(ctx, request)
	if err != nil {
		return result, err
	}
	projectID := inspected.Plan.Project.ID
	lock, err := service.locks.LockProject(ctx, projectID)
	if err != nil {
		return result, model.Wrap(model.CodeConflict, "lock project port reconfiguration", err)
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Unlock()) }()
	manifest, err := service.oneLiveManifest(ctx, projectID)
	if err != nil {
		return result, err
	}
	if manifest.State != model.StateRunning && manifest.State != model.StateStopped {
		return result, model.NewError(model.CodeConflict, fmt.Sprintf("cannot reconfigure ports while the live workspace is %s", manifest.State), nil)
	}
	capabilities, err := service.runtime.Probe(ctx)
	if err != nil {
		return result, err
	}
	if err := requireLifecycleCapabilities(inspected.Plan, capabilities); err != nil {
		return result, err
	}
	publication, err := ports.Plan(inspected.Plan.Ports, capabilities)
	if err != nil {
		return result, portPlanError(err)
	}
	defer func() { _ = publication.Abort() }()
	workspaceIndex := -1
	for index := range manifest.Resources {
		if manifest.Resources[index].Kind == string(runtime.ResourceWorkspace) && manifest.Resources[index].Role == workspaceRole {
			workspaceIndex = index
			break
		}
	}
	if workspaceIndex < 0 {
		return result, model.NewError(model.CodeAmbiguous, "live workspace manifest has no workspace resource", nil)
	}
	workspaceRecord := manifest.Resources[workspaceIndex]
	snapshot, found, err := service.findRuntimeResource(ctx, workspaceRecord)
	if err != nil {
		return result, err
	}
	if !found {
		return result, model.NewError(model.CodeUnavailable, "live workspace is unavailable for port reconfiguration", nil)
	}
	classification := ownership.Classify(&workspaceRecord, &snapshot)
	if !classification.DeleteAllowed {
		return result, model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	identity := bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}
	if err := service.stopPersistentHostBridges(ctx, identity); err != nil {
		return result, err
	}
	if snapshot.State == "running" {
		if err := service.runtime.Stop(ctx, snapshot, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"}); err != nil {
			return result, err
		}
	}
	if manifest.State == model.StateRunning {
		if err := service.transition(ctx, &manifest, model.StateStopped, "stop", ""); err != nil {
			return result, err
		}
	}
	if err := service.runtime.Delete(ctx, snapshot); err != nil {
		return result, err
	}
	manifest.Resources[workspaceIndex].RuntimeID = string(snapshot.ID)
	manifest.Resources[workspaceIndex].Created = true
	manifest.Resources[workspaceIndex].Deleted = true
	if err := service.replace(ctx, &manifest); err != nil {
		return result, err
	}
	manifest.Resources[workspaceIndex].RuntimeID = ""
	manifest.Resources[workspaceIndex].Created = false
	manifest.Resources[workspaceIndex].Deleted = false
	manifest.Resources[workspaceIndex].Absent = false
	manifest.PlanHash = inspected.Plan.ExecutableHash
	manifest.HostBindings = nil
	manifest.Operation = "reconfigure-ports"
	manifest.Failure = ""
	if err := service.replace(ctx, &manifest); err != nil {
		return result, err
	}
	current, err := service.revalidateApprovedPlan(ctx, request, inspected.Plan.ExecutableHash)
	if err != nil {
		return result, err
	}
	current.Sandbox.RunID = manifest.RunID
	network, err := manifestResource(manifest, runtime.ResourceNetwork, networkRole)
	if err != nil {
		return result, err
	}
	volumeNames := make(map[string]string, len(current.Volumes))
	for _, volume := range current.Volumes {
		record, recordErr := manifestResource(manifest, runtime.ResourceVolume, volumeRole(volume.Name))
		if recordErr != nil {
			return result, recordErr
		}
		volumeNames[volume.Name] = record.Name
	}
	imageReference := current.Image.Reference
	if imageReference == "" {
		if current.Image.Standard {
			imageReference = fmt.Sprintf("dsx.local/standard:%s", current.Image.InputDigest[:12])
		} else {
			imageReference = fmt.Sprintf("dsx.local/%s:%s", current.Project.ID, current.Image.InputDigest[:12])
		}
	}
	if _, ok := pinnedImageDigest("local@" + snapshot.ImageDigest); !ok {
		return result, model.NewError(model.CodeUnavailable, "existing workspace image digest is malformed", nil)
	}
	image := runtime.Image{
		Reference: imageReference,
		Digest:    snapshot.ImageDigest,
		Local:     current.Image.Reference == "",
	}
	guestHelper, err := service.guestHelperForPlan(current)
	if err != nil {
		return result, err
	}
	leappMirrorSource, err := service.ensureLeappMirrorForPlan(ctx, current, identity)
	if err != nil {
		return result, err
	}
	runtimePorts, err := publication.ReleaseForCreate()
	if err != nil {
		return result, model.NewError(model.CodeConflict, "release port reservations for workspace recreation", err)
	}
	workspaceSpec, err := workspaceSpecForPlan(current, image, manifest.Resources[workspaceIndex], network.Name, volumeNames, runtimePorts, service.user(), guestHelper, leappMirrorSource)
	if err != nil {
		return result, err
	}
	if err := service.createResource(ctx, &manifest, workspaceIndex, func(state.ResourceRecord) (runtime.Resource, error) {
		return service.runtime.CreateWorkspace(ctx, workspaceSpec)
	}); err != nil {
		return result, err
	}
	recreated, err := service.runtime.Inspect(ctx, runtime.ResourceID(manifest.Resources[workspaceIndex].ExpectedID))
	if err != nil {
		return result, err
	}
	classification = ownership.Classify(&manifest.Resources[workspaceIndex], &recreated)
	if !classification.DeleteAllowed {
		return result, model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	if err := service.runtime.StartWorkspace(ctx, recreated); err != nil {
		return result, err
	}
	recreated, err = service.runtime.Inspect(ctx, runtime.ResourceID(manifest.Resources[workspaceIndex].ExpectedID))
	if err != nil {
		return result, err
	}
	relayBindings, err := publication.ReleaseForRelay()
	if err != nil {
		return result, model.NewError(model.CodeConflict, "release host relay reservations after workspace recreation", err)
	}
	bindings, err := publication.Reconcile(recreated.Ports)
	if err != nil {
		return result, model.NewError(model.CodeAmbiguous, "recreated workspace ports differ from the approved plan", err)
	}
	current, hostBridges, _, err := service.ensurePersistentHostBridges(ctx, recreated, current, identity, relayBindings, true)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, hostBridges.Close()) }()
	if err := service.recordHostBindings(ctx, &manifest, bindings); err != nil {
		return result, err
	}
	if guestGraphEnabled(current) {
		if err := service.guest.Reconcile(ctx, recreated); err != nil {
			return result, model.Wrap(model.CodeUnavailable, "reconcile recreated guest supervisor", err)
		}
		started, startErr := service.guest.Start(ctx, recreated, current, 0)
		if startErr != nil {
			return result, model.Wrap(model.CodeUnavailable, "start recreated guest process graph", startErr)
		}
		if err := service.waitGuestReady(ctx, recreated, started.Generation); err != nil {
			return result, err
		}
	}
	if err := service.transition(ctx, &manifest, model.StateRunning, "create", ""); err != nil {
		return result, err
	}
	urls, err := ports.RenderURLs(bindings)
	if err != nil {
		return result, err
	}
	return StartResult{
		ProjectID: projectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID,
		State: manifest.State, Existing: true, URLs: urls,
	}, nil
}

func (service *LifecycleService) startLocked(ctx context.Context, request StartRequest, inspected InspectResult, report StartProgressReporter) (result StartResult, err error) {
	projectID := inspected.Plan.Project.ID
	capabilities, probeErr := service.runtime.Probe(ctx)
	if probeErr != nil {
		return result, probeErr
	}
	if capabilityErr := requireLifecycleCapabilities(inspected.Plan, capabilities); capabilityErr != nil {
		return result, capabilityErr
	}
	manifests, err := service.manifests.ListProjectManifests(ctx, projectID)
	if err != nil {
		return result, err
	}
	active := activeManifests(manifests)
	if len(active) != 0 {
		if len(active) != 1 || active[0].Sandbox != liveSandboxName || active[0].Mode != model.ModeLive {
			return result, model.NewError(model.CodeConflict, "live and clone sandboxes cannot run concurrently for one project", nil)
		}
		current, revalidateErr := service.revalidateApprovedPlan(ctx, request, inspected.Plan.ExecutableHash)
		if revalidateErr != nil {
			return result, revalidateErr
		}
		return service.startExisting(ctx, active[0], current, capabilities, report)
	}
	publication, publicationErr := ports.Plan(inspected.Plan.Ports, capabilities)
	if publicationErr != nil {
		return result, portPlanError(publicationErr)
	}
	defer func() { _ = publication.Abort() }()
	now := service.now().UTC()
	runID, err := service.newRunID(now)
	if err != nil {
		return result, model.Wrap(model.CodeInternal, "generate lifecycle run ID", err)
	}
	manifest, volumeResources, err := plannedLiveManifest(inspected.Plan, runID, now)
	if err != nil {
		return result, model.Wrap(model.CodeInvalidInput, "plan live resource graph", err)
	}
	if err := service.manifests.CreateIntent(ctx, manifest); err != nil {
		return result, err
	}
	if err := service.transition(ctx, &manifest, model.StateCreating, "create", ""); err != nil {
		return result, err
	}
	var hostBridges *hostBridgeSession
	created, urls, createErr := service.createLive(ctx, request, inspected.Plan, volumeResources, publication, &manifest, &hostBridges, report)
	if createErr != nil {
		rollbackErr := service.rollbackCreate(ctx, &manifest)
		return result, errors.Join(createErr, rollbackErr)
	}
	if err := service.transition(ctx, &manifest, model.StateRunning, "create", ""); err != nil {
		bridgeErr := hostBridges.Close()
		rollbackErr := service.rollbackCreate(ctx, &manifest)
		return result, errors.Join(err, bridgeErr, rollbackErr)
	}
	return StartResult{ProjectID: projectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID, State: manifest.State, Existing: !created, URLs: urls, hostBridges: hostBridges}, nil
}

func (service *LifecycleService) startExisting(ctx context.Context, manifest state.Manifest, current plan.ExecutionPlan, capabilities runtime.Capabilities, report StartProgressReporter) (result StartResult, returnErr error) {
	reportStartProgress(report, StartProgressWorkspace)
	result = StartResult{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID, State: manifest.State, Existing: true}
	if manifest.PlanHash != current.ExecutableHash {
		return result, model.NewError(model.CodeUnapproved, "existing sandbox plan differs from the currently approved executable plan; clean it before starting", nil)
	}
	if manifest.State != model.StateRunning && manifest.State != model.StateStopped {
		return result, model.NewError(model.CodeConflict, fmt.Sprintf("existing sandbox is %s; run dsx clean before starting", manifest.State), nil)
	}
	snapshot, urls, err := service.inspectVerifiedWorkspace(ctx, manifest, current)
	if err != nil {
		return result, err
	}
	result.URLs = urls
	var hostBridges *hostBridgeSession
	persistentLeaseEnsured := false
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, hostBridges.Close())
			if persistentLeaseEnsured {
				rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				returnErr = errors.Join(returnErr, service.stopPersistentHostBridges(rollbackCtx, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}))
			}
		}
	}()
	mirrorIdentity := bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}
	mirrorEnsured := false
	defer func() {
		if returnErr != nil && mirrorEnsured {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			returnErr = errors.Join(returnErr, service.stopLeappMirror(rollbackCtx, mirrorIdentity))
		}
	}()

	reportStartProgress(report, StartProgressServices)
	switch snapshot.State {
	case "running":
		if mirrorSource, mirrorErr := service.ensureLeappMirrorForPlan(ctx, current, mirrorIdentity); mirrorErr != nil {
			return result, mirrorErr
		} else {
			mirrorEnsured = mirrorSource != ""
		}
		current, hostBridges, _, err = service.ensurePersistentHostBridges(ctx, snapshot, current, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}, fallbackManifestBindings(manifest, current, capabilities), true)
		persistentLeaseEnsured = hostBridges != nil
		if err != nil {
			return result, err
		}
		if guestGraphEnabled(current) {
			if service.guest == nil {
				return result, model.NewError(model.CodeUnavailable, "guest process service is unavailable", nil)
			}
			status, statusErr := service.guest.Status(ctx, snapshot)
			if statusErr != nil {
				return result, model.Wrap(model.CodeUnavailable, "read existing guest process graph", statusErr)
			}
			if readyErr := service.waitGuestReady(ctx, snapshot, status.Generation); readyErr != nil {
				if errors.Is(readyErr, errTerminalRequiredGuestFailure) && manifest.State == model.StateRunning {
					transitionErr := service.transition(context.WithoutCancel(ctx), &manifest, model.StateFailed, manifest.Operation, "required guest process failed")
					return result, errors.Join(readyErr, transitionErr)
				}
				return result, readyErr
			}
		}
		if manifest.State == model.StateRunning {
			result.hostBridges = hostBridges
			return result, nil
		}
	case "stopped":
		if mirrorSource, mirrorErr := service.ensureLeappMirrorForPlan(ctx, current, mirrorIdentity); mirrorErr != nil {
			return result, mirrorErr
		} else {
			mirrorEnsured = mirrorSource != ""
		}
		if err := service.runtime.StartWorkspace(ctx, snapshot); err != nil {
			if manifest.State == model.StateRunning {
				reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				reconcileErr := service.transition(reconcileCtx, &manifest, model.StateStopped, "stop", err.Error())
				return result, errors.Join(err, reconcileErr)
			}
			return result, err
		}
		restarted, inspectErr := service.runtime.Inspect(ctx, snapshot.ID)
		if inspectErr != nil {
			return result, inspectErr
		}
		workspaceRecord, recordErr := manifestResource(manifest, runtime.ResourceWorkspace, workspaceRole)
		if recordErr != nil {
			return result, recordErr
		}
		if classification := ownership.Classify(&workspaceRecord, &restarted); !classification.DeleteAllowed {
			return result, model.NewError(model.CodeAmbiguous, classification.Reason, nil)
		}
		snapshot = restarted
		current, hostBridges, _, err = service.ensurePersistentHostBridges(ctx, snapshot, current, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}, fallbackManifestBindings(manifest, current, capabilities), true)
		persistentLeaseEnsured = hostBridges != nil
		if err != nil {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			return result, errors.Join(err, service.runtime.Stop(rollbackCtx, snapshot, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"}))
		}
		if guestGraphEnabled(current) {
			if service.guest == nil {
				return result, model.NewError(model.CodeUnavailable, "guest process service is unavailable", nil)
			}
			if reconcileErr := service.guest.Reconcile(ctx, snapshot); reconcileErr != nil {
				rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				stopErr := service.runtime.Stop(rollbackCtx, snapshot, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"})
				return result, errors.Join(model.Wrap(model.CodeUnavailable, "reconcile restarted guest supervisor", reconcileErr), stopErr)
			}
			started, guestErr := service.guest.Start(ctx, snapshot, current, 0)
			if guestErr == nil {
				guestErr = service.waitGuestReady(ctx, snapshot, started.Generation)
			}
			if guestErr != nil {
				rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				stopErr := service.runtime.Stop(rollbackCtx, snapshot, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"})
				if manifest.State == model.StateRunning {
					guestErr = errors.Join(guestErr, service.transition(rollbackCtx, &manifest, model.StateStopped, "stop", guestErr.Error()))
				}
				return result, errors.Join(guestErr, stopErr)
			}
		}
	default:
		return result, model.NewError(model.CodeUnavailable, fmt.Sprintf("runtime workspace has unsupported state %q", snapshot.State), nil)
	}
	if manifest.State == model.StateStopped {
		if err := service.transition(ctx, &manifest, model.StateRunning, "create", ""); err != nil {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			stopErr := service.runtime.Stop(rollbackCtx, snapshot, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"})
			manifestErr := service.transition(rollbackCtx, &manifest, model.StateStopped, "stop", errors.Join(err, stopErr).Error())
			return result, errors.Join(err, stopErr, manifestErr)
		}
	}
	result.State = model.StateRunning
	result.hostBridges = hostBridges
	return result, nil
}
func (service *LifecycleService) inspectVerifiedWorkspace(ctx context.Context, manifest state.Manifest, current plan.ExecutionPlan) (runtime.ResourceSnapshot, []string, error) {
	capabilities, err := service.runtime.Probe(ctx)
	if err != nil {
		return runtime.ResourceSnapshot{}, nil, err
	}
	workspace, err := manifestResource(manifest, runtime.ResourceWorkspace, workspaceRole)
	if err != nil {
		return runtime.ResourceSnapshot{}, nil, err
	}
	snapshot, err := service.runtime.Inspect(ctx, runtime.ResourceID(workspace.ExpectedID))
	if err != nil {
		return runtime.ResourceSnapshot{}, nil, err
	}
	classification := ownership.Classify(&workspace, &snapshot)
	if !classification.DeleteAllowed {
		return runtime.ResourceSnapshot{}, nil, model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	current.Sandbox.RunID = manifest.RunID
	network, err := manifestResource(manifest, runtime.ResourceNetwork, networkRole)
	if err != nil {
		return runtime.ResourceSnapshot{}, nil, err
	}
	volumeNames := make(map[string]string, len(current.Volumes))
	for _, volume := range current.Volumes {
		record, resourceErr := manifestResource(manifest, runtime.ResourceVolume, volumeRole(volume.Name))
		if resourceErr != nil {
			return runtime.ResourceSnapshot{}, nil, resourceErr
		}
		volumeNames[volume.Name] = record.Name
	}
	guestHelper, err := service.guestHelperForPlan(current)
	if err != nil {
		return runtime.ResourceSnapshot{}, nil, err
	}
	leappMirrorSource, err := service.leappMirrorPathForPlan(current, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID})
	if err != nil {
		return runtime.ResourceSnapshot{}, nil, err
	}
	expectedPorts := portRequestsFromBindings(snapshot.Ports)
	if ports.UsesFallback(current.Ports, capabilities) {
		expectedPorts = nil
	}
	expected, err := workspaceSpecForPlan(current, runtime.Image{}, workspace, network.Name, volumeNames, expectedPorts, service.user(), guestHelper, leappMirrorSource)
	if err != nil {
		return runtime.ResourceSnapshot{}, nil, err
	}
	if err := runtime.VerifyWorkspacePostcondition(snapshot, expected); err != nil {
		return runtime.ResourceSnapshot{}, nil, model.NewError(model.CodeAmbiguous, "runtime workspace grants differ from the approved manifest: "+err.Error(), nil)
	}
	bindings, bindingErr := ports.ReconcileExisting(current.Ports, publishedBindingsFromManifest(manifest), snapshot.Ports, capabilities)
	if bindingErr != nil {
		return runtime.ResourceSnapshot{}, nil, model.NewError(model.CodeAmbiguous, "runtime port bindings differ from the approved publication plan", bindingErr)
	}
	urls, err := ports.RenderURLs(bindings)
	if err != nil {
		return runtime.ResourceSnapshot{}, nil, model.NewError(model.CodeAmbiguous, "render inspected port bindings", err)
	}
	return snapshot, urls, nil
}

func (service *LifecycleService) createLive(ctx context.Context, request StartRequest, approved plan.ExecutionPlan, volumeResources map[string]int, publication *ports.PublicationPlan, manifest *state.Manifest, activeHostBridges **hostBridgeSession, report StartProgressReporter) (created bool, urls []string, returnErr error) {
	if _, err := service.revalidateApprovedPlan(ctx, request, approved.ExecutableHash); err != nil {
		return false, nil, err
	}
	guestHelper, err := service.guestHelperForPlan(approved)
	if err != nil {
		return false, nil, err
	}
	reportStartProgress(report, StartProgressImage)
	buildRoot := ""
	var stagedBuild *config.ImageBuild
	var approvedStagedDigest string
	if approved.Image.Reference == "" {
		build := config.ImageBuild{Context: approved.Image.Context, File: approved.Image.File}
		var stagedRoot, stagedDigest string
		if approved.Image.Standard {
			build = config.ImageBuild{Context: ".", File: "Containerfile"}
			stagedRoot, stagedDigest, err = stageStandardImage(ctx, approved.Project.CanonicalRoot)
		} else {
			stagedRoot, stagedDigest, err = stageBuildInput(ctx, approved.Project.CanonicalRoot, build)
		}
		if err != nil {
			return false, nil, model.NewError(model.CodeUnapproved, "build input changed while staging", err)
		}
		buildRoot = stagedRoot
		stagedBuild = &build
		approvedStagedDigest = stagedDigest
		defer func() {
			if err := os.RemoveAll(stagedRoot); err != nil {
				created = false
				urls = nil
				returnErr = errors.Join(returnErr, fmt.Errorf("remove staged build input: %w", err))
			}
		}()
		if stagedDigest != approved.Image.InputDigest {
			return false, nil, model.NewError(model.CodeUnapproved, "staged build input does not match the approved digest", nil)
		}
	}
	imageSpec, err := imageSpecForPlan(approved, buildRoot)
	if err != nil {
		return false, nil, err
	}
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	image, err := service.runtime.EnsureImage(ctx, imageSpec)
	if err != nil {
		return false, nil, err
	}
	if stagedBuild != nil {
		consumedDigest, digestErr := digestBuildInputInto(ctx, buildRoot, *stagedBuild, "")
		if digestErr != nil {
			return false, nil, model.NewError(model.CodeUnapproved, "staged build input changed while the image builder consumed it", digestErr)
		}
		if consumedDigest != approvedStagedDigest {
			return false, nil, model.NewError(model.CodeUnapproved, "staged build input changed while the image builder consumed it", nil)
		}
	}
	reportStartProgress(report, StartProgressResources)
	if err := service.createResource(ctx, manifest, 0, func(record state.ResourceRecord) (runtime.Resource, error) {
		return service.runtime.CreateNetwork(ctx, runtime.NetworkSpec{Name: record.Name, Labels: runtimeLabels(record.Labels)})
	}); err != nil {
		return false, nil, err
	}
	volumeNames := make(map[string]string, len(volumeResources))
	for _, volume := range approved.Volumes {
		index := volumeResources[volume.Name]
		if err := service.createResource(ctx, manifest, index, func(record state.ResourceRecord) (runtime.Resource, error) {
			return service.runtime.CreateVolume(ctx, runtime.VolumeSpec{Name: record.Name, Labels: runtimeLabels(record.Labels)})
		}); err != nil {
			return false, nil, err
		}
		volumeNames[volume.Name] = manifest.Resources[index].Name
	}
	current, err := service.revalidateApprovedPlan(ctx, request, approved.ExecutableHash)
	if err != nil {
		return false, nil, err
	}
	current.Sandbox.RunID = manifest.RunID
	mirrorIdentity := bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}
	leappMirrorSource, err := service.ensureLeappMirrorForPlan(ctx, current, mirrorIdentity)
	if err != nil {
		return false, nil, err
	}
	if leappMirrorSource != "" {
		defer func() {
			if returnErr != nil {
				rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				returnErr = errors.Join(returnErr, service.stopLeappMirror(rollbackCtx, mirrorIdentity))
			}
		}()
	}
	runtimePorts, err := publication.ReleaseForCreate()
	if err != nil {
		return false, nil, model.NewError(model.CodeConflict, "release port reservations for workspace create", err)
	}
	workspaceIndex := len(manifest.Resources) - 1
	workspaceSpec, err := workspaceSpecForPlan(current, image, manifest.Resources[workspaceIndex], manifest.Resources[0].Name, volumeNames, runtimePorts, service.user(), guestHelper, leappMirrorSource)
	if err != nil {
		return false, nil, err
	}
	reportStartProgress(report, StartProgressWorkspace)
	if err := service.createResource(ctx, manifest, workspaceIndex, func(state.ResourceRecord) (runtime.Resource, error) {
		return service.runtime.CreateWorkspace(ctx, workspaceSpec)
	}); err != nil {
		return false, nil, err
	}
	revalidatedMirror, err := service.ensureLeappMirrorForPlan(ctx, current, mirrorIdentity)
	if err != nil {
		return false, nil, err
	}
	if revalidatedMirror != leappMirrorSource {
		return false, nil, model.NewError(model.CodeAmbiguous, "Leapp mirror path changed during workspace creation", nil)
	}
	workspaceRecord := manifest.Resources[workspaceIndex]
	snapshot, inspectErr := service.runtime.Inspect(ctx, runtime.ResourceID(workspaceRecord.ExpectedID))
	if inspectErr != nil {
		return false, nil, inspectErr
	}
	classification := ownership.Classify(&workspaceRecord, &snapshot)
	if !classification.DeleteAllowed {
		return false, nil, model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	if err := service.runtime.StartWorkspace(ctx, snapshot); err != nil {
		return false, nil, err
	}
	snapshot, inspectErr = service.runtime.Inspect(ctx, runtime.ResourceID(workspaceRecord.ExpectedID))
	if inspectErr != nil {
		return false, nil, inspectErr
	}
	classification = ownership.Classify(&workspaceRecord, &snapshot)
	if !classification.DeleteAllowed || snapshot.State != "running" {
		return false, nil, model.NewError(model.CodeAmbiguous, "started workspace does not match the owned running snapshot", nil)
	}
	relayBindings, releaseErr := publication.ReleaseForRelay()
	if releaseErr != nil {
		return false, nil, model.NewError(model.CodeConflict, "release port reservations for host relay", releaseErr)
	}
	bindings, bindingErr := publication.Reconcile(snapshot.Ports)
	if bindingErr != nil {
		return false, nil, model.NewError(model.CodeAmbiguous, "published listeners differ from the approved publication plan", bindingErr)
	}
	if activeHostBridges == nil {
		return false, nil, model.NewError(model.CodeInternal, "host bridge session output is unavailable", nil)
	}
	identity := bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}
	current, hostBridges, _, err := service.ensurePersistentHostBridges(ctx, snapshot, current, identity, relayBindings, true)
	if err != nil {
		return false, nil, err
	}
	*activeHostBridges = hostBridges
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, hostBridges.Close())
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			returnErr = errors.Join(returnErr, service.stopPersistentHostBridges(rollbackCtx, identity))
			*activeHostBridges = nil
		}
	}()
	if err := service.recordHostBindings(ctx, manifest, bindings); err != nil {
		return false, nil, err
	}
	urls, urlErr := ports.RenderURLs(bindings)
	if urlErr != nil {
		return false, nil, model.NewError(model.CodeAmbiguous, "render verified host relay bindings", urlErr)
	}
	reportStartProgress(report, StartProgressServices)
	if guestGraphEnabled(current) {
		if err := service.guest.Reconcile(ctx, snapshot); err != nil {
			return false, nil, model.Wrap(model.CodeUnavailable, "reconcile created guest supervisor", err)
		}
		started, err := service.guest.Start(ctx, snapshot, current, 0)
		if err != nil {
			return false, nil, model.Wrap(model.CodeUnavailable, "start guest process graph", err)
		}
		if err := service.waitGuestReady(ctx, snapshot, started.Generation); err != nil {
			return false, nil, err
		}
	}
	return true, urls, nil
}

func (service *LifecycleService) waitGuestReady(ctx context.Context, workspace runtime.ResourceSnapshot, generation uint64) error {
	if generation == 0 {
		return model.NewError(model.CodeUnavailable, "guest process graph is not configured", nil)
	}
	readyContext, cancel := context.WithTimeout(ctx, lifecycleGuestReadyTimeout)
	defer cancel()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-readyContext.Done():
			return model.Wrap(model.CodeUnavailable, "wait for required guest process readiness", readyContext.Err())
		case <-timer.C:
		}
		status, err := service.guest.Status(readyContext, workspace)
		if err != nil {
			return model.Wrap(model.CodeUnavailable, "read guest process readiness", err)
		}
		if status.Generation != generation {
			return model.NewError(model.CodeConflict, "guest process generation changed during startup", nil)
		}
		ready, readinessErr := guestRequiredReady(status)
		if readinessErr != nil {
			if terminalRequiredGuestFailure(status) {
				return model.Wrap(model.CodeUnavailable, "required guest process failed before readiness", errTerminalRequiredGuestFailure)
			}
			return readinessErr
		}
		if ready {
			return nil
		}
		timer.Reset(lifecycleGuestPollInterval)
	}
}

func guestRequiredReady(status guestproto.StatusResult) (bool, error) {
	if status.Generation == 0 {
		return false, model.NewError(model.CodeUnavailable, "guest process graph is not configured", nil)
	}
	if status.Failed {
		return false, model.NewError(model.CodeUnavailable, "required guest process failed before readiness", nil)
	}
	for _, process := range status.Processes {
		if process.Required && !process.Ready {
			return false, nil
		}
	}
	return true, nil
}

func (service *LifecycleService) createResource(ctx context.Context, manifest *state.Manifest, index int, create func(state.ResourceRecord) (runtime.Resource, error)) error {
	record := manifest.Resources[index]
	resource, err := create(record)
	if err != nil {
		return err
	}
	if string(resource.ID) != record.ExpectedID || resource.Name != record.Name || string(resource.Kind) != record.Kind {
		return model.NewError(model.CodeUnavailable, "runtime returned a resource that does not match the write-ahead identity", nil)
	}
	manifest.Resources[index].RuntimeID = string(resource.ID)
	manifest.Resources[index].Created = true
	return service.replace(ctx, manifest)
}

func (service *LifecycleService) Stop(ctx context.Context, request StopRequest) (result StopResult, err error) {
	selected, err := requestedNamedSandbox(request.Sandbox)
	if err != nil {
		return result, err
	}
	if err := service.ready(ctx); err != nil {
		return result, err
	}
	projectID, err := projectIDForRoot(request.Root)
	if err != nil {
		return result, err
	}
	lock, err := service.locks.LockProject(ctx, projectID)
	if err != nil {
		return result, model.Wrap(model.CodeConflict, "lock project lifecycle", err)
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	var manifest state.Manifest
	if selected == "" {
		manifest, err = service.oneLiveManifest(ctx, projectID)
	} else {
		manifest, err = service.oneNamedCloneManifest(ctx, projectID, selected)
	}
	if err != nil {
		return result, err
	}
	result = StopResult{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID, State: manifest.State}
	recoverableFailedClone := manifest.Mode == model.ModeClone && manifest.State == model.StateFailed && manifest.UncapturedWork
	if manifest.State != model.StateRunning && manifest.State != model.StateStopped && !recoverableFailedClone {
		return result, model.NewError(model.CodeConflict, fmt.Sprintf("cannot stop sandbox in state %s", manifest.State), nil)
	}
	if err := service.stopPersistentHostBridges(ctx, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}); err != nil {
		return result, err
	}
	workspace, err := manifestResource(manifest, runtime.ResourceWorkspace, workspaceRole)
	if err != nil {
		return result, err
	}
	snapshot, found, err := service.findRuntimeResource(ctx, workspace)
	if err != nil {
		return result, err
	}
	if !found {
		return result, model.NewError(model.CodeUnavailable, "runtime workspace is missing", nil)
	}
	classification := ownership.Classify(&workspace, &snapshot)
	if !classification.DeleteAllowed {
		return result, model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	switch snapshot.State {
	case "running":
		if err := service.runtime.Stop(ctx, snapshot, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"}); err != nil {
			return result, err
		}
	case "stopped":
	default:
		return result, model.NewError(model.CodeUnavailable, fmt.Sprintf("runtime workspace has unsupported state %q", snapshot.State), nil)
	}
	if err := service.stopLeappMirror(ctx, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}); err != nil {
		return result, err
	}
	if manifest.State == model.StateRunning {
		if err := service.transition(ctx, &manifest, model.StateStopped, "stop", ""); err != nil {
			return result, err
		}
	}
	if !recoverableFailedClone {
		result.State = model.StateStopped
	}
	return result, nil
}

func (service *LifecycleService) Clean(ctx context.Context, request CleanRequest) (result CleanResult, err error) {
	if request.All && request.Sandbox != "" {
		return result, model.NewError(model.CodeInvalidInput, "cleanup cannot combine a sandbox selector with all projects", nil)
	}
	selected, selectorErr := requestedNamedSandbox(request.Sandbox)
	if selectorErr != nil {
		return result, selectorErr
	}
	if err := service.ready(ctx); err != nil {
		return result, err
	}
	timing := beginPhase(service.timingRecorder, service.timingClock, PhaseClean)
	defer timing.Stop()
	if !request.Confirmed {
		return result, model.NewError(model.CodeUnapproved, "cleanup requires explicit confirmation", nil)
	}
	if !request.All {
		projectID, projectErr := projectIDForRoot(request.Root)
		if projectErr != nil {
			return result, projectErr
		}
		if selected != "" {
			return service.cleanSandbox(ctx, projectID, selected, request.DiscardUnfetched)
		}
		return service.cleanProject(ctx, projectID, request.DiscardUnfetched)
	}
	manifests, listErr := service.manifests.ListAllManifests(ctx)
	if listErr != nil {
		return result, listErr
	}
	projectSet := make(map[model.ProjectID]struct{}, len(manifests))
	for _, manifest := range manifests {
		projectSet[manifest.ProjectID] = struct{}{}
	}
	orphanProjects, orphanInventoryErr := service.runtimeProjectIDs(ctx)
	if orphanInventoryErr != nil {
		return result, orphanInventoryErr
	}
	for _, projectID := range orphanProjects {
		projectSet[projectID] = struct{}{}
	}
	projectIDs := make([]model.ProjectID, 0, len(projectSet))
	for projectID := range projectSet {
		projectIDs = append(projectIDs, projectID)
	}
	sort.Slice(projectIDs, func(i, j int) bool { return projectIDs[i] < projectIDs[j] })
	for _, projectID := range projectIDs {
		cleaned, cleanErr := service.cleanProject(ctx, projectID, request.DiscardUnfetched)
		result.Projects += cleaned.Projects
		result.DeletedManifests += cleaned.DeletedManifests
		result.DeletedResources += cleaned.DeletedResources
		result.Preserved = append(result.Preserved, cleaned.Preserved...)
		err = errors.Join(err, cleanErr)
	}
	sort.Strings(result.Preserved)
	return result, err
}
func cloneCleanupSandboxNames(manifests []state.Manifest) []model.SandboxName {
	unique := make(map[model.SandboxName]struct{}, len(manifests))
	for _, manifest := range manifests {
		if manifest.Mode == model.ModeClone {
			unique[manifest.Sandbox] = struct{}{}
		}
	}
	names := make([]model.SandboxName, 0, len(unique))
	for name := range unique {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

func sameSandboxNames(first, second []model.SandboxName) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func unlockLifecycleLocks(locks []state.ProjectLock) error {
	var err error
	for index := len(locks) - 1; index >= 0; index-- {
		err = errors.Join(err, locks[index].Unlock())
	}
	return err
}

func (service *LifecycleService) lockProjectCloneCleanup(ctx context.Context, projectID model.ProjectID) (state.ProjectLock, []state.ProjectLock, []state.Manifest, error) {
	for {
		observed, err := service.manifests.ListProjectManifests(ctx, projectID)
		if err != nil {
			return nil, nil, nil, err
		}
		names := cloneCleanupSandboxNames(observed)
		sandboxLocks := make([]state.ProjectLock, 0, len(names))
		for _, name := range names {
			lock, lockErr := service.locks.LockSandbox(ctx, projectID, name)
			if lockErr != nil {
				return nil, nil, nil, errors.Join(model.Wrap(model.CodeConflict, "lock clone sandbox cleanup", lockErr), unlockLifecycleLocks(sandboxLocks))
			}
			sandboxLocks = append(sandboxLocks, lock)
		}
		projectLock, err := service.locks.LockProject(ctx, projectID)
		if err != nil {
			return nil, nil, nil, errors.Join(model.Wrap(model.CodeConflict, "lock project lifecycle", err), unlockLifecycleLocks(sandboxLocks))
		}
		current, listErr := service.manifests.ListProjectManifests(ctx, projectID)
		if listErr != nil {
			return nil, nil, nil, errors.Join(listErr, projectLock.Unlock(), unlockLifecycleLocks(sandboxLocks))
		}
		if sameSandboxNames(names, cloneCleanupSandboxNames(current)) {
			return projectLock, sandboxLocks, current, nil
		}
		if unlockErr := errors.Join(projectLock.Unlock(), unlockLifecycleLocks(sandboxLocks)); unlockErr != nil {
			return nil, nil, nil, unlockErr
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, model.Wrap(model.CodeUnavailable, "stabilize clone sandbox cleanup leases", err)
		}
	}
}

func (service *LifecycleService) cleanProject(ctx context.Context, projectID model.ProjectID, discardUnfetched bool) (result CleanResult, err error) {
	result.ProjectID = projectID
	result.Projects = 1
	lock, sandboxLocks, manifests, err := service.lockProjectCloneCleanup(ctx, projectID)
	if err != nil {
		return result, err
	}
	defer func() {
		err = errors.Join(err, lock.Unlock(), unlockLifecycleLocks(sandboxLocks))
	}()
	for index := range manifests {
		deleted, preserved, cleanErr := service.cleanManifest(ctx, &manifests[index], discardUnfetched)
		result.DeletedResources += deleted
		result.Preserved = append(result.Preserved, preserved...)
		if cleanErr != nil {
			err = errors.Join(err, cleanErr)
			continue
		}
		if deleteErr := service.deleteCleanedManifest(ctx, manifests[index]); deleteErr != nil {
			err = errors.Join(err, deleteErr)
			continue
		}
		result.DeletedManifests++
	}
	orphans, orphanErr := service.unmanifestedProjectResources(ctx, projectID, manifests)
	result.Preserved = append(result.Preserved, orphans...)
	err = errors.Join(err, orphanErr)
	sort.Strings(result.Preserved)
	return result, err
}

func (service *LifecycleService) cleanSandbox(ctx context.Context, projectID model.ProjectID, sandbox model.SandboxName, discardUnfetched bool) (result CleanResult, err error) {
	result.ProjectID = projectID
	result.Projects = 1
	sandboxLock, err := service.locks.LockSandbox(ctx, projectID, sandbox)
	if err != nil {
		return result, model.Wrap(model.CodeConflict, "lock clone sandbox cleanup", err)
	}
	defer func() { err = errors.Join(err, sandboxLock.Unlock()) }()
	lock, err := service.locks.LockProject(ctx, projectID)
	if err != nil {
		return result, model.Wrap(model.CodeConflict, "lock project lifecycle", err)
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	manifests, err := service.manifests.ListProjectManifests(ctx, projectID)
	if err != nil {
		return result, err
	}
	matches := matchingSandboxManifests(manifests, sandbox)
	if len(matches) == 0 {
		return result, nil
	}
	if len(matches) != 1 {
		return result, model.NewError(model.CodeAmbiguous, fmt.Sprintf("sandbox %s has multiple lifecycle manifests", sandbox), nil)
	}
	manifest := matches[0]
	if manifest.Mode != model.ModeClone {
		return result, model.NewError(model.CodeInvalidInput, fmt.Sprintf("sandbox %s is not a named clone", sandbox), nil)
	}
	deleted, preserved, cleanErr := service.cleanManifest(ctx, &manifest, discardUnfetched)
	result.DeletedResources = deleted
	result.Preserved = append(result.Preserved, preserved...)
	if cleanErr != nil {
		sort.Strings(result.Preserved)
		return result, cleanErr
	}
	if err := service.deleteCleanedManifest(ctx, manifest); err != nil {
		return result, err
	}
	result.DeletedManifests = 1
	return result, nil
}

func (service *LifecycleService) ensureNoOtherActiveSandboxManifest(ctx context.Context, manifest state.Manifest) error {
	manifests, err := service.manifests.ListProjectManifests(ctx, manifest.ProjectID)
	if err != nil {
		return err
	}
	for _, candidate := range manifests {
		if candidate.RunID == manifest.RunID || candidate.Sandbox != manifest.Sandbox || candidate.State == model.StateDeleted {
			continue
		}
		return model.NewError(model.CodeAmbiguous, fmt.Sprintf("sandbox %s has another nondeleted lifecycle manifest", manifest.Sandbox), nil)
	}
	return nil
}

func (service *LifecycleService) deleteCleanedManifest(ctx context.Context, manifest state.Manifest) error {
	if err := service.ensureNoOtherActiveSandboxManifest(ctx, manifest); err != nil {
		return err
	}

	if service.cleanSandboxAuth != nil {
		if err := service.cleanSandboxAuth(ctx, manifest.ProjectID, manifest.Sandbox); err != nil {
			if model.ErrorCodeOf(err) != "" {
				return err
			}
			return model.Wrap(model.CodeUnavailable, "remove sandbox authentication seed", err)
		}
	}
	return service.manifests.DeleteManifest(ctx, manifest.ProjectID, manifest.Sandbox, manifest.RunID)
}

func unfetchedResultEntries(manifest state.Manifest) []string {
	if manifest.Mode != model.ModeClone {
		return nil
	}
	entries := make([]string, 0, len(manifest.Git)+1)
	if manifest.UncapturedWork {
		entries = append(entries, fmt.Sprintf("clone sandbox %s has uncaptured repository work", manifest.Sandbox))
	}
	for _, record := range manifest.Git {
		if record.HasResultWork() && !record.ResultFetched() {
			entries = append(entries, fmt.Sprintf("clone sandbox %s repository %s has unfetched result work", manifest.Sandbox, record.Repository))
		}
	}
	sort.Strings(entries)
	return entries
}

func cloneCleanupGuard(manifest state.Manifest) ([]string, error) {
	entries := unfetchedResultEntries(manifest)
	var err error
	for _, entry := range entries {
		err = errors.Join(err, model.NewError(model.CodeDataLoss, entry, nil))
	}
	return entries, err
}
func (service *LifecycleService) captureStableCloneState(ctx context.Context, manifest *state.Manifest) error {
	if manifest.Mode != model.ModeClone {
		return nil
	}
	if service.cloneCleanupRecovery == nil {
		return model.NewError(model.CodeUnavailable, "clone result capture is unavailable", nil)
	}
	workspaceRecord, err := manifestResource(*manifest, runtime.ResourceWorkspace, workspaceRole)
	if err != nil {
		return err
	}
	workspace, found, err := service.findRuntimeResource(ctx, workspaceRecord)
	if err != nil {
		return err
	}
	if !found {
		return model.NewError(model.CodeUnavailable, "runtime workspace is unavailable for clone result capture", nil)
	}
	workspaceClassification := ownership.Classify(&workspaceRecord, &workspace)
	if workspaceClassification.Outcome != ownership.OutcomeOwned || !workspaceClassification.DeleteAllowed {
		return model.NewError(model.CodeAmbiguous, "runtime workspace ownership is ambiguous: "+workspaceClassification.Reason, nil)
	}
	if workspace.State != "running" && workspace.State != "stopped" {
		return model.NewError(model.CodeUnavailable, fmt.Sprintf("runtime workspace is unavailable for clone result capture in state %q", workspace.State), nil)
	}

	writers := make([]runtime.ResourceSnapshot, 0, 1)
	for _, record := range manifest.Resources {
		if record.Deleted || record.Kind != string(runtime.ResourceBrowser) {
			continue
		}
		snapshot, writerFound, findErr := service.findRuntimeResource(ctx, record)
		if findErr != nil {
			return findErr
		}
		if !writerFound {
			continue
		}
		classification := ownership.Classify(&record, &snapshot)
		if classification.Outcome != ownership.OutcomeOwned || !classification.DeleteAllowed {
			return model.NewError(model.CodeAmbiguous, record.Name+": "+classification.Reason, nil)
		}
		if snapshot.State != "running" && snapshot.State != "stopped" {
			return model.NewError(model.CodeUnavailable, fmt.Sprintf("clone writer %s is unavailable for result capture in state %q", record.Name, snapshot.State), nil)
		}
		writers = append(writers, snapshot)
	}
	for _, writer := range writers {
		if writer.State == "running" {
			if err := service.runtime.Stop(ctx, writer, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"}); err != nil {
				return model.Wrap(model.CodeUnavailable, "quiesce clone writer before result capture", err)
			}
		}
	}
	if workspace.State == "running" {
		if err := service.runtime.Stop(ctx, workspace, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"}); err != nil {
			return model.Wrap(model.CodeUnavailable, "quiesce clone workspace before result capture", err)
		}
	}
	if err := service.runtime.StartWorkspace(ctx, workspace); err != nil {
		return model.Wrap(model.CodeUnavailable, "restart clone workspace for result capture", err)
	}
	restarted, restartedFound, inspectErr := service.findRuntimeResource(ctx, workspaceRecord)
	if inspectErr != nil {
		return inspectErr
	}
	if !restartedFound {
		return model.NewError(model.CodeUnavailable, "restarted clone workspace is unavailable for result capture", nil)
	}
	restartedClassification := ownership.Classify(&workspaceRecord, &restarted)
	if restartedClassification.Outcome != ownership.OutcomeOwned || !restartedClassification.DeleteAllowed {
		return model.NewError(model.CodeAmbiguous, "restarted clone workspace ownership is ambiguous: "+restartedClassification.Reason, nil)
	}
	if restarted.State != "running" {
		return model.NewError(model.CodeUnavailable, fmt.Sprintf("restarted clone workspace is unavailable for result capture in state %q", restarted.State), nil)
	}
	captureErr := service.cloneCleanupRecovery(ctx, restarted, manifest)
	stopErr := service.runtime.Stop(context.WithoutCancel(ctx), restarted, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"})
	if err := errors.Join(captureErr, stopErr); err != nil {
		return err
	}
	if manifest.State == model.StateRunning {
		return service.transition(ctx, manifest, model.StateStopped, "clean", "")
	}
	return nil
}
func (service *LifecycleService) verifyFetchedCloneResults(ctx context.Context, manifest state.Manifest) error {
	if manifest.Mode != model.ModeClone {
		return nil
	}
	hasFetchedResult := false
	for _, record := range manifest.Git {
		if record.ResultFetched() {
			hasFetchedResult = true
			break
		}
	}
	if !hasFetchedResult {
		return nil
	}
	if service.cloneCleanupFetchedVerifier == nil {
		return model.NewError(model.CodeUnavailable, "fetched clone result verification is unavailable", nil)
	}
	return service.cloneCleanupFetchedVerifier(ctx, manifest)
}
func (service *LifecycleService) validateCloneCleanupIdentity(ctx context.Context, manifest state.Manifest) error {
	if manifest.Mode != model.ModeClone {
		return nil
	}
	if service.cloneCleanupIdentityValidator == nil {
		return model.NewError(model.CodeUnavailable, "clone repository identity validation is unavailable", nil)
	}
	return service.cloneCleanupIdentityValidator(ctx, manifest)
}

func (service *LifecycleService) cleanManifest(ctx context.Context, manifest *state.Manifest, discardUnfetched bool) (int, []string, error) {
	if manifest.State == model.StateDeleted {
		return 0, nil, nil
	}
	if err := service.validateCloneCleanupIdentity(ctx, *manifest); err != nil {
		return 0, nil, err
	}
	if manifest.Mode == model.ModeClone && !discardUnfetched && (manifest.State != model.StateCreating || manifest.UncapturedWork) {
		if captureErr := service.captureStableCloneState(ctx, manifest); captureErr != nil {
			preserved, guardErr := cloneCleanupGuard(*manifest)
			return 0, preserved, errors.Join(guardErr, captureErr)
		}
	}
	if !discardUnfetched {
		preserved, guardErr := cloneCleanupGuard(*manifest)
		if guardErr != nil {
			return 0, preserved, guardErr
		}
	}
	if !discardUnfetched {
		if err := service.verifyFetchedCloneResults(ctx, *manifest); err != nil {
			return 0, nil, err
		}
	}
	if err := service.stopPersistentHostBridges(ctx, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}); err != nil {
		return 0, []string{"host relay lease"}, err
	}
	if manifest.State != model.StateCleaning {
		if err := service.transition(ctx, manifest, model.StateCleaning, "clean", ""); err != nil {
			return 0, nil, err
		}
	}
	deleted := 0
	preserved := make([]string, 0)
	var cleanupErr error
	for index := len(manifest.Resources) - 1; index >= 0; index-- {
		record := &manifest.Resources[index]
		if record.Deleted {
			continue
		}
		snapshot, found, err := service.findRuntimeResource(ctx, *record)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			preserved = append(preserved, record.Name)
			continue
		}
		if !found {
			if record.Created {
				record.Deleted = true
			} else {
				record.Absent = true
			}
			if err := service.replace(ctx, manifest); err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
			}
			continue
		}
		classification := ownership.Classify(record, &snapshot)
		if !classification.DeleteAllowed {
			preserved = append(preserved, record.Name)
			cleanupErr = errors.Join(cleanupErr, model.NewError(model.CodeAmbiguous, record.Name+": "+classification.Reason, nil))
			continue
		}
		if snapshot.Kind == runtime.ResourceWorkspace || snapshot.Kind == runtime.ResourceBrowser {
			if snapshot.State == "running" {
				if err := service.runtime.Stop(ctx, snapshot, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"}); err != nil {
					preserved = append(preserved, record.Name)
					cleanupErr = errors.Join(cleanupErr, err)
					continue
				}
			}
		}
		if err := service.runtime.Delete(ctx, snapshot); err != nil {
			preserved = append(preserved, record.Name)
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		record.RuntimeID = string(snapshot.ID)
		record.Created = true
		record.Deleted = true
		deleted++
		if err := service.replace(ctx, manifest); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if err := service.stopLeappMirror(ctx, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
		preserved = append(preserved, "Leapp mirror")
	}
	if cleanupErr != nil {
		if err := service.transition(context.WithoutCancel(ctx), manifest, model.StateFailed, "clean", cleanupErr.Error()); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		return deleted, preserved, cleanupErr
	}
	if discardUnfetched {
		discardProtectedCloneWork(manifest)
	}
	if err := service.transition(ctx, manifest, model.StateDeleted, "clean", ""); err != nil {
		return deleted, preserved, err
	}
	return deleted, preserved, nil
}

func (service *LifecycleService) rollbackCreate(ctx context.Context, manifest *state.Manifest) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, preserved, err := service.cleanManifest(rollbackCtx, manifest, false)
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "rollback failed; preserved resources: "+strings.Join(preserved, ", "), err)
	}
	if err := service.manifests.DeleteManifest(rollbackCtx, manifest.ProjectID, manifest.Sandbox, manifest.RunID); err != nil {
		return model.Wrap(model.CodeInternal, "delete rolled-back manifest", err)
	}
	return nil
}

func (service *LifecycleService) List(ctx context.Context, request ListRequest) (ListResult, error) {
	if err := service.ready(ctx); err != nil {
		return ListResult{}, err
	}
	projectID, err := projectIDForRoot(request.Root)
	if err != nil {
		return ListResult{}, err
	}
	manifests, err := service.manifests.ListProjectManifests(ctx, projectID)
	if err != nil {
		return ListResult{}, err
	}
	var currentBridgePlan *plan.ExecutionPlan
	if service.inspection != nil {
		if inspected, inspectErr := service.inspection.Inspect(ctx, InspectRequest{Root: request.Root}); inspectErr == nil && inspected.Plan.Project.ID == projectID {
			currentBridgePlan = &inspected.Plan
		}
	}
	capabilities, capabilityErr := service.runtime.Probe(ctx)
	result := ListResult{Sandboxes: make([]SandboxSummary, 0, len(manifests))}
	for _, manifest := range manifests {
		summary := SandboxSummary{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID, Mode: manifest.Mode, State: manifest.State}
		summary.Warnings = append(summary.Warnings, unfetchedResultEntries(manifest)...)
		if urls, renderErr := ports.RenderURLs(publishedBindingsFromManifest(manifest)); renderErr != nil {
			summary.Warnings = append(summary.Warnings, "published ports: "+renderErr.Error())
		} else {
			summary.URLs = urls
		}
		privateRelay := currentBridgePlan != nil && manifest.Mode == model.ModeLive && hasHostBridgeGrants(*currentBridgePlan)
		publicationRelay := currentBridgePlan != nil && capabilityErr == nil && len(manifest.HostBindings) != 0 && ports.UsesFallback(currentBridgePlan.Ports, capabilities)
		if currentBridgePlan != nil && manifest.PlanHash == currentBridgePlan.ExecutableHash && (privateRelay || publicationRelay) {
			summary.Warnings = append(summary.Warnings, service.persistentHostBridgeWarnings(ctx, *currentBridgePlan, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}, publicationRelay)...)
		}
		mirrorPlan := currentBridgePlan
		if (mirrorPlan == nil || manifest.PlanHash != mirrorPlan.ExecutableHash) && service.inspection != nil {
			if inspected, inspectErr := service.inspection.Inspect(ctx, InspectRequest{Root: request.Root, SandboxName: string(manifest.Sandbox), Mode: string(manifest.Mode)}); inspectErr == nil && inspected.Plan.Project.ID == projectID {
				mirrorPlan = &inspected.Plan
			}
		}
		if mirrorPlan != nil && manifest.State == model.StateRunning && manifest.PlanHash == mirrorPlan.ExecutableHash {
			if authority, authorityErr := leappAuthorityForPlan(*mirrorPlan); authorityErr == nil && authority != nil {
				summary.Warnings = append(summary.Warnings, service.leappMirrorWarnings(ctx, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID})...)
			}
		}
		for _, record := range manifest.Resources {
			if record.Deleted {
				continue
			}
			summary.Resources++
			snapshot, found, lookupErr := service.findRuntimeResource(ctx, record)
			if lookupErr != nil {
				summary.Warnings = append(summary.Warnings, record.Name+": "+lookupErr.Error())
				continue
			}
			if !found {
				summary.Warnings = append(summary.Warnings, record.Name+": missing from runtime")
				continue
			}
			classification := ownership.Classify(&record, &snapshot)
			if classification.Outcome != ownership.OutcomeOwned {
				summary.Warnings = append(summary.Warnings, record.Name+": "+classification.Reason)
			}
		}
		sort.Strings(summary.Warnings)
		result.Sandboxes = append(result.Sandboxes, summary)
	}
	return result, nil
}

func (service *LifecycleService) Shell(ctx context.Context, request ShellRequest) (result ShellResult, err error) {
	if err := service.ready(ctx); err != nil {
		return result, err
	}
	timing := beginPhase(service.timingRecorder, service.timingClock, PhaseShell)
	defer timing.Stop()
	var projectID model.ProjectID
	var inspected *InspectResult
	if request.ApproveConfig != "" {
		approved, inspectErr := service.inspectApproved(ctx, StartRequest{Root: request.Root, ApproveConfig: request.ApproveConfig, Agent: request.Agent})
		if inspectErr != nil {
			return result, inspectErr
		}
		projectID = approved.Plan.Project.ID
		inspected = &approved
	} else {
		projectID, err = projectIDForRoot(request.Root)
		if err != nil {
			return result, err
		}
	}
	lock, err := service.locks.LockProject(ctx, projectID)
	if err != nil {
		return result, model.Wrap(model.CodeConflict, "lock project lifecycle", err)
	}
	locked := true
	unlock := func() error {
		if !locked {
			return nil
		}
		locked = false
		if unlockErr := lock.Unlock(); unlockErr != nil {
			return model.Wrap(model.CodeInternal, "unlock project lifecycle", unlockErr)
		}
		return nil
	}
	defer func() { err = errors.Join(err, unlock()) }()
	var hostBridges *hostBridgeSession
	if inspected != nil {
		started, startErr := service.startLocked(ctx, StartRequest{Root: request.Root, ApproveConfig: request.ApproveConfig, Agent: request.Agent}, *inspected, nil)
		if startErr != nil {
			return result, startErr
		}
		hostBridges = started.hostBridges
	}
	defer func() { err = errors.Join(err, hostBridges.Close()) }()
	manifest, err := service.oneLiveManifest(ctx, projectID)
	if err != nil {
		return result, err
	}
	if manifest.State != model.StateRunning {
		return result, model.NewError(model.CodeConflict, "shell requires a running live sandbox", nil)
	}
	current, planErr := service.revalidateApprovedPlan(ctx, StartRequest{Root: request.Root, ApproveConfig: manifest.PlanHash, Agent: request.Agent}, manifest.PlanHash)
	if planErr != nil {
		return result, planErr
	}
	snapshot, urls, err := service.inspectVerifiedWorkspace(ctx, manifest, current)
	if err != nil {
		return result, err
	}
	if hostBridges == nil && hasHostBridgeGrants(current) {
		current, hostBridges, err = service.activateHostBridges(ctx, snapshot, current)
		if err != nil {
			return result, err
		}
	}
	shellReady := ShellReady{URLs: append([]string(nil), urls...)}
	if guestGraphEnabled(current) {
		if service.guest == nil {
			return result, model.NewError(model.CodeUnavailable, "guest process service is unavailable", nil)
		}
		status, statusErr := service.guest.Status(ctx, snapshot)
		if statusErr != nil {
			return result, model.Wrap(model.CodeUnavailable, "read guest process readiness", statusErr)
		}
		if terminalRequiredGuestFailure(status) && manifest.State == model.StateRunning {
			if transitionErr := service.transition(ctx, &manifest, model.StateFailed, manifest.Operation, "required guest process failed"); transitionErr != nil {
				return result, transitionErr
			}
		}
		ready, readinessErr := guestRequiredReady(status)
		if readinessErr != nil {
			return result, readinessErr
		}
		if !ready {
			return result, model.NewError(model.CodeConflict, "required guest processes are not ready", nil)
		}
		shellReady.Processes = append([]guestproto.ProcessStatus(nil), status.Processes...)
	}
	if request.BeforeExec != nil {
		if beforeErr := request.BeforeExec(shellReady); beforeErr != nil {
			return result, model.Wrap(model.CodeInternal, "render shell readiness", beforeErr)
		}
	}
	argv := append([]string(nil), request.Argv...)
	if len(argv) == 0 {
		argv = []string{"/bin/zsh", "-il"}
	}
	shellEnvironment, environmentErr := mergeHostBridgeEnvironment(request.Env, hostBridges.Environment())
	if environmentErr != nil {
		return result, environmentErr
	}
	stagedEnvironment, stageErr := service.stageExecEnvironment(ctx, snapshot, manifest.RunID, shellEnvironment, request.SecretEnvironmentKeys)
	if stageErr != nil {
		return result, stageErr
	}
	if stagedEnvironment.guest != "" {
		defer func() {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), secretEnvironmentCleanupTimeout)
			defer cancelCleanup()
			err = errors.Join(err, service.cleanupGuestSecretEnvironment(cleanupCtx, snapshot, stagedEnvironment.guest))
		}()
		argv = append([]string{DefaultGuestHelperPath, "exec", "--env-file", string(stagedEnvironment.guest), "--"}, argv...)
	} else {
		argv = append([]string{DefaultGuestHelperPath, "exec", "--"}, argv...)
	}
	spec := runtime.ExecSpec{Argv: argv, Env: stagedEnvironment.ordinary, WorkingDir: workspaceGuestRoot, User: service.user(), Terminal: request.Terminal}
	if request.Terminal {
		if request.RunInteractive == nil {
			return result, model.NewError(model.CodeInvalidInput, "interactive shell runner is not configured", nil)
		}
		process, prepareErr := service.runtime.PrepareExec(ctx, snapshot, spec)
		if prepareErr != nil {
			return result, prepareErr
		}
		if unlockErr := unlock(); unlockErr != nil {
			return result, unlockErr
		}
		child := InteractiveChild{
			Argv:   append([]string{process.Executable}, process.Args...),
			Env:    append([]string(nil), process.Env...),
			Dir:    process.Dir,
			Stdin:  request.Stdin,
			Stdout: request.Stdout,
			Stderr: request.Stderr,
		}
		timing.Stop()
		result.Exit, err = request.RunInteractive(ctx, child)
		return result, err
	}
	if unlockErr := unlock(); unlockErr != nil {
		return result, unlockErr
	}
	timing.Stop()
	result.Exit, err = service.runtime.Exec(ctx, snapshot, spec, runtime.ExecIO{Stdin: request.Stdin, Stdout: request.Stdout, Stderr: request.Stderr})
	return result, err
}

func (service *LifecycleService) ready(ctx context.Context) error {
	if ctx == nil {
		return model.NewError(model.CodeInvalidInput, "lifecycle context is nil", nil)
	}
	if service == nil || service.inspection == nil || service.manifests == nil || service.locks == nil || service.runtime == nil {
		return model.NewError(model.CodeInternal, "lifecycle service is not configured", nil)
	}
	return nil
}

func (service *LifecycleService) inspectApproved(ctx context.Context, request StartRequest) (InspectResult, error) {
	sandbox, mode := lifecyclePlanIdentity(request)
	result, err := service.inspection.Inspect(ctx, InspectRequest{Root: request.Root, SandboxName: sandbox, Mode: string(mode), ApproveConfig: request.ApproveConfig, CLIOverrides: CLIOverrides{Agent: request.Agent, Browser: request.Browser}})
	if err != nil {
		return result, err
	}
	approvalMode := state.ApprovalModeNonInteractive
	if request.Interactive {
		approvalMode = state.ApprovalModeInteractive
	}
	approval := state.ApprovalRequest{
		Mode: approvalMode,
		Record: state.ApprovalRecord{
			Version:   state.ApprovalRecordVersion,
			ProjectID: result.Plan.Project.ID,
			Hash:      result.Plan.ExecutableHash,
		},
		ApprovedHash:   request.ApproveConfig,
		FinalConfirmed: request.FinalConfirmed,
	}
	if err := state.AuthorizeApproval(ctx, service.approvals, approval); err != nil {
		return result, err
	}
	return result, nil
}

func (service *LifecycleService) revalidateApprovedPlan(ctx context.Context, request StartRequest, expectedHash string) (plan.ExecutionPlan, error) {
	sandbox, mode := lifecyclePlanIdentity(request)
	result, err := service.inspection.Inspect(ctx, InspectRequest{Root: request.Root, SandboxName: sandbox, Mode: string(mode), CLIOverrides: CLIOverrides{Agent: request.Agent, Browser: request.Browser}})
	if err != nil {
		return plan.ExecutionPlan{}, err
	}
	if result.Plan.ExecutableHash != expectedHash {
		return plan.ExecutionPlan{}, model.NewError(model.CodeUnapproved, "executable plan authority changed after approval", nil)
	}
	for _, mount := range result.Plan.Mounts {
		if mount.SourceType != "host" {
			continue
		}
		authority, err := resolveHostMount(mount.Source)
		if err != nil || authority.CanonicalPath != mount.Source || authority.Identity != mount.SourceIdentity {
			return plan.ExecutionPlan{}, model.NewError(model.CodeUnapproved, "host mount identity changed after approval", err)
		}
	}
	for _, grant := range result.Plan.Bridges {
		if grant.Kind != "aws" {
			continue
		}
		opened, err := bridge.OpenApprovedLeappDirectory(bridge.LeappAuthority{
			DeclaredPath:  grant.Destination,
			CanonicalPath: grant.Destination,
			Identity:      grant.SourceIdentity,
		})
		if err != nil {
			return plan.ExecutionPlan{}, model.NewError(model.CodeUnapproved, "Leapp directory identity changed after approval", err)
		}
		if err := opened.Close(); err != nil {
			return plan.ExecutionPlan{}, model.Wrap(model.CodeUnavailable, "close validated Leapp directory", err)
		}
	}
	return result.Plan, nil
}

func lifecyclePlanIdentity(request StartRequest) (string, model.WorkspaceMode) {
	sandbox := request.Sandbox
	if sandbox == "" {
		sandbox = string(liveSandboxName)
	}
	mode := request.Mode
	if mode == "" {
		mode = model.ModeLive
	}
	return sandbox, mode
}

func plannedLiveManifest(execution plan.ExecutionPlan, runID model.RunID, now time.Time) (state.Manifest, map[string]int, error) {
	if execution.Mode != model.ModeLive || execution.Sandbox.Name != liveSandboxName {
		return state.Manifest{}, nil, fmt.Errorf("expected the main live plan")
	}
	manifest := state.Manifest{
		Version:       state.ManifestVersion,
		Generation:    1,
		ProjectID:     execution.Project.ID,
		CanonicalRoot: execution.Project.CanonicalRoot,
		Sandbox:       liveSandboxName,
		RunID:         runID,
		Mode:          model.ModeLive,
		PlanHash:      execution.ExecutableHash,
		State:         model.StatePlanned,
		Operation:     "create",
		Resources:     make([]state.ResourceRecord, 0, len(execution.Volumes)+2),
		CreatedAt:     now.UTC(),
		UpdatedAt:     now.UTC(),
	}
	networkIdentity, err := ownership.NewIdentity(manifest.ProjectID, manifest.Sandbox, manifest.RunID, runtime.ResourceNetwork, networkRole)
	if err != nil {
		return state.Manifest{}, nil, err
	}
	manifest.Resources = append(manifest.Resources, networkIdentity.ManifestRecord())
	volumeResources := make(map[string]int, len(execution.Volumes))
	for _, volume := range execution.Volumes {
		role := volumeRole(volume.Name)
		if _, duplicate := volumeResources[volume.Name]; duplicate {
			return state.Manifest{}, nil, fmt.Errorf("duplicate volume %q", volume.Name)
		}
		identity, err := ownership.NewIdentity(manifest.ProjectID, manifest.Sandbox, manifest.RunID, runtime.ResourceVolume, role)
		if err != nil {
			return state.Manifest{}, nil, err
		}
		record := identity.ManifestRecord()
		record.Persistent = volume.Persistent
		volumeResources[volume.Name] = len(manifest.Resources)
		manifest.Resources = append(manifest.Resources, record)
	}
	workspaceIdentity, err := ownership.NewIdentity(manifest.ProjectID, manifest.Sandbox, manifest.RunID, runtime.ResourceWorkspace, workspaceRole)
	if err != nil {
		return state.Manifest{}, nil, err
	}
	manifest.Resources = append(manifest.Resources, workspaceIdentity.ManifestRecord())
	if err := state.ValidateManifest(manifest); err != nil {
		return state.Manifest{}, nil, err
	}
	return manifest, volumeResources, nil
}

func (service *LifecycleService) PrepareStandardImage(ctx context.Context, execution plan.ExecutionPlan) (returnErr error) {
	if ctx == nil {
		return model.NewError(model.CodeInvalidInput, "prepare standard image: context is nil", nil)
	}
	if service == nil || service.runtime == nil {
		return model.NewError(model.CodeInternal, "prepare standard image runtime is unavailable", nil)
	}
	if !execution.Image.Standard || execution.Image.InputDigest == "" {
		return model.NewError(model.CodeInvalidInput, "prepare standard image requires a managed standard plan", nil)
	}
	stageRoot, stagedDigest, err := stageStandardImage(ctx, execution.Project.CanonicalRoot)
	if err != nil {
		return model.Wrap(model.CodeUnapproved, "stage standard image", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(stageRoot); cleanupErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove staged standard image: %w", cleanupErr))
		}
	}()
	if stagedDigest != execution.Image.InputDigest {
		return model.NewError(model.CodeUnapproved, "staged standard image does not match the approved digest", nil)
	}
	spec, err := imageSpecForPlan(execution, stageRoot)
	if err != nil {
		return err
	}
	if _, err := service.runtime.EnsureImage(ctx, spec); err != nil {
		return err
	}
	consumedDigest, err := digestBuildInputInto(ctx, stageRoot, config.ImageBuild{Context: ".", File: "Containerfile"}, "")
	if err != nil || consumedDigest != stagedDigest {
		return model.NewError(model.CodeUnapproved, "staged standard image changed while the image builder consumed it", err)
	}
	return nil
}

func imageSpecForPlan(execution plan.ExecutionPlan, buildRoot string) (runtime.ImageSpec, error) {
	spec := runtime.ImageSpec{Reference: execution.Image.Reference}
	if execution.Image.Reference != "" {
		return spec, nil
	}
	if execution.Image.Context == "" || execution.Image.File == "" || len(execution.Image.InputDigest) < 12 {
		return spec, model.NewError(model.CodeInvalidInput, "build image authority is incomplete", nil)
	}
	if buildRoot == "" || !filepath.IsAbs(buildRoot) || filepath.Clean(buildRoot) != buildRoot || pathWithin(execution.Project.CanonicalRoot, buildRoot) {
		return spec, model.NewError(model.CodeInvalidInput, "build image staging root is not a clean absolute path outside the project", nil)
	}
	contextName, fileName := execution.Image.Context, execution.Image.File
	if execution.Image.Standard {
		contextName, fileName = ".", "Containerfile"
	}
	contextPath, err := projectAbsolutePath(buildRoot, contextName)
	if err != nil {
		return spec, model.NewError(model.CodeInvalidInput, "staged build context is invalid", err)
	}
	filePath, err := projectAbsolutePath(buildRoot, fileName)
	if err != nil {
		return spec, model.NewError(model.CodeInvalidInput, "staged build file is invalid", err)
	}
	if execution.Image.Standard {
		spec.Reference = fmt.Sprintf("dsx.local/standard:%s", execution.Image.InputDigest[:12])
		spec.Labels = append(spec.Labels, runtime.Label{Key: "dev.dsx.standard-input", Value: execution.Image.InputDigest})
		spec.Reuse = true
	} else {
		spec.Reference = fmt.Sprintf("dsx.local/%s:%s", execution.Project.ID, execution.Image.InputDigest[:12])
	}
	spec.Context = runtime.HostPath(contextPath)
	spec.File = runtime.HostPath(filePath)
	spec.Target = execution.Image.Target
	for _, argument := range execution.Image.BuildArgs {
		spec.BuildArgs = append(spec.BuildArgs, runtime.Label{Key: argument.Key, Value: argument.Value})
	}
	return spec, nil
}

func validateRepositoryMountSource(source string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve current user home for repository mount policy: %w", err)
	}
	canonicalHome, err := filepath.EvalSymlinks(filepath.Clean(home))
	if err != nil {
		return fmt.Errorf("canonicalize current user home for repository mount policy: %w", err)
	}
	canonicalHome, err = filepath.Abs(canonicalHome)
	if err != nil {
		return fmt.Errorf("absolutize current user home for repository mount policy: %w", err)
	}
	if source == filepath.Clean(canonicalHome) {
		return errors.New("complete host home cannot be mounted as a repository")
	}
	sourceInfo, sourceErr := os.Stat(source)
	homeInfo, homeErr := os.Stat(canonicalHome)
	if sourceErr == nil && homeErr == nil && os.SameFile(sourceInfo, homeInfo) {
		return errors.New("complete host home cannot be mounted as a repository")
	}
	return nil
}

func workspaceSpecForPlan(execution plan.ExecutionPlan, image runtime.Image, record state.ResourceRecord, networkName string, volumeNames map[string]string, publishedPorts []runtime.PortRequest, user string, guestHelper runtime.HostPath, leappMirrorSource string) (runtime.WorkspaceSpec, error) {
	mounts := make([]runtime.Mount, 0, len(execution.Repositories)+len(execution.Mounts)+len(execution.Volumes)+2)
	for _, repository := range execution.Repositories {
		if repository.HostPath == "" || !filepath.IsAbs(repository.HostPath) || filepath.Clean(repository.HostPath) != repository.HostPath {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "repository host path is not a clean absolute path", nil)
		}
		if execution.Project.CanonicalRoot == "" || !pathWithin(execution.Project.CanonicalRoot, repository.HostPath) {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeUnapproved, "repository host path is outside the approved project root", nil)
		}
		if repository.GuestPath != "/workspace" && !strings.HasPrefix(repository.GuestPath, "/workspace/") {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "repository guest path is outside the workspace root", nil)
		}
		if err := rejectSymlinkComponents(repository.HostPath); err != nil {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeUnapproved, "repository path changed before workspace creation", err)
		}
		if err := validateRepositoryMountSource(repository.HostPath); err != nil {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeUnapproved, "repository source violates host home isolation", err)
		}
		mounts = append(mounts, runtime.Mount{Source: repository.HostPath, Target: repository.GuestPath, Type: "bind", Authority: runtime.MountAuthorityRepository})
	}
	for _, mount := range execution.Mounts {
		source := mount.Source
		mountType := "bind"
		authority := runtime.MountAuthorityConfiguredHost
		switch mount.SourceType {
		case "host":
			resolved, err := resolveHostMount(source)
			if err != nil || resolved.CanonicalPath != source || resolved.Identity != mount.SourceIdentity {
				return runtime.WorkspaceSpec{}, model.NewError(model.CodeUnapproved, "configured host mount authority changed before workspace creation", err)
			}
		case "volume":
			var found bool
			source, found = volumeNames[mount.Source]
			if !found {
				return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "mount references an unknown volume", nil)
			}
			mountType = "volume"
			authority = runtime.MountAuthorityVolume
		default:
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "mount has an unsupported source type", nil)
		}
		mounts = append(mounts, runtime.Mount{Source: source, Target: mount.Target, Type: mountType, ReadOnly: mount.ReadOnly, Authority: authority})
	}
	mountedTargets := make(map[string]struct{}, len(mounts))
	for _, mount := range mounts {
		mountedTargets[mount.Target] = struct{}{}
	}
	for _, volume := range execution.Volumes {
		if _, alreadyMounted := mountedTargets[volume.Target]; alreadyMounted {
			continue
		}
		name, found := volumeNames[volume.Name]
		if !found {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "volume resource is missing", nil)
		}
		mounts = append(mounts, runtime.Mount{Source: name, Target: volume.Target, Type: "volume", Authority: runtime.MountAuthorityVolume})
	}
	leapp, err := leappGrantForPlan(execution)
	if err != nil {
		return runtime.WorkspaceSpec{}, err
	}
	if err := validateLeappHostIsolation(leapp, mounts); err != nil {
		return runtime.WorkspaceSpec{}, err
	}
	if leapp != nil {
		if leappMirrorSource == "" || !filepath.IsAbs(leappMirrorSource) || filepath.Clean(leappMirrorSource) != leappMirrorSource || leappMirrorSource == leapp.Source {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeUnavailable, "private Leapp mirror source is unavailable", nil)
		}
		for _, mount := range mounts {
			if guestPathsOverlap(mount.Target, leapp.Target) {
				return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "mount target overlaps the reserved Leapp directory", nil)
			}
		}
		mounts = append(mounts, runtime.Mount{Source: leappMirrorSource, Target: leapp.Target, Type: "bind", ReadOnly: leapp.ReadOnly, Authority: runtime.MountAuthorityLeappMirror})
	} else if leappMirrorSource != "" {
		return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "Leapp mirror source exists without an AWS grant", nil)
	}
	if guestHelper == "" {
		return runtime.WorkspaceSpec{}, model.NewError(model.CodeUnavailable, "guest execution helper is unavailable", nil)
	}
	for _, mount := range mounts {
		if guestPathsOverlap(mount.Target, DefaultGuestHelperDirectory) {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "mount target overlaps the reserved guest helper directory", nil)
		}
	}
	mounts = append(mounts, runtime.Mount{Source: filepath.Dir(string(guestHelper)), Target: DefaultGuestHelperDirectory, Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityGuestHelper})
	entrypoint := []string{DefaultGuestHelperPath, "exec", "--", "/bin/sh", "-lc", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"}
	workspaceUser := user
	if guestGraphEnabled(execution) {
		uid, gid, valid := strings.Cut(user, ":")
		if !valid || uid == "" || gid == "" || uid == "0" || gid == "0" {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "guest supervisor requires a non-root numeric workspace UID:GID", nil)
		}
		if _, collision := mountedTargets[DefaultGuestHelperDirectory]; collision {
			return runtime.WorkspaceSpec{}, model.NewError(model.CodeInvalidInput, "guest helper target is reserved", nil)
		}
		entrypoint = []string{
			DefaultGuestHelperPath, "serve",
			"--socket", DefaultGuestSocketPath,
			"--child-uid", uid,
			"--child-gid", gid,
		}
		workspaceUser = "0:0"
	}
	environment := []string{
		"DSX_PROJECT_ID=" + string(execution.Project.ID),
		"DSX_SANDBOX=" + string(execution.Sandbox.Name),
		"DSX_RUN_ID=" + string(execution.Sandbox.RunID),
	}
	if leapp != nil {
		environment = append(environment, leapp.Environment...)
	}
	return runtime.WorkspaceSpec{
		Name:        record.Name,
		Image:       image,
		Entrypoint:  entrypoint,
		Env:         environment,
		WorkingDir:  workspaceGuestRoot,
		User:        workspaceUser,
		Mounts:      mounts,
		Networks:    []string{networkName},
		Ports:       append([]runtime.PortRequest(nil), publishedPorts...),
		Labels:      runtimeLabels(record.Labels),
		CPUs:        execution.Limits.CPUs,
		MemoryBytes: execution.Limits.MemoryBytes,
	}, nil
}

func validateLeappHostIsolation(leapp *bridge.LeappWorkspaceGrant, mounts []runtime.Mount) error {
	if leapp == nil {
		return nil
	}
	writable := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		if mount.Type == "bind" && !mount.ReadOnly {
			writable = append(writable, mount.Source)
		}
	}
	if err := hostsource.ValidateReadOnlyIsolation(leapp.Source, writable); err != nil {
		return model.NewError(model.CodeUnapproved, "Leapp read-only host source overlaps writable workspace authority", err)
	}
	return nil
}

func leappGrantForPlan(execution plan.ExecutionPlan) (*bridge.LeappWorkspaceGrant, error) {
	selected, err := selectedLeappPlanGrant(execution)
	if err != nil || selected == nil {
		return nil, err
	}
	if !selected.ReadOnly || selected.Port != 0 {
		return nil, model.NewError(model.CodeInvalidInput, "AWS bridge grant is not a read-only directory grant", nil)
	}
	grant, err := bridge.LeappGrant(bridge.LeappAuthority{
		DeclaredPath:  selected.Destination,
		CanonicalPath: selected.Destination,
		Identity:      selected.SourceIdentity,
	}, selected.Name)
	if err != nil {
		return nil, model.NewError(model.CodeInvalidInput, "AWS bridge grant authority is invalid", err)
	}
	return &grant, nil
}

func selectedLeappPlanGrant(execution plan.ExecutionPlan) (*plan.BridgeGrant, error) {
	var selected *plan.BridgeGrant
	for index := range execution.Bridges {
		grant := &execution.Bridges[index]
		if grant.Kind != "aws" {
			continue
		}
		if selected != nil {
			return nil, model.NewError(model.CodeInvalidInput, "execution plan contains multiple AWS bridge grants", nil)
		}
		selected = grant
	}
	return selected, nil
}

func leappAuthorityForPlan(execution plan.ExecutionPlan) (*bridge.LeappAuthority, error) {
	selected, err := selectedLeappPlanGrant(execution)
	if err != nil || selected == nil {
		return nil, err
	}
	authority := &bridge.LeappAuthority{
		DeclaredPath:  selected.Destination,
		CanonicalPath: selected.Destination,
		Identity:      selected.SourceIdentity,
	}
	return authority, nil
}

func (service *LifecycleService) leappMirrorPathForPlan(execution plan.ExecutionPlan, identity bridge.LeaseIdentity) (string, error) {
	authority, err := leappAuthorityForPlan(execution)
	if err != nil || authority == nil {
		return "", err
	}
	if service.leappMirrors == nil {
		return "", model.NewError(model.CodeUnavailable, "Leapp mirror service is unavailable", nil)
	}
	return service.leappMirrors.Path(identity)
}

func (service *LifecycleService) ensureLeappMirrorForPlan(ctx context.Context, execution plan.ExecutionPlan, identity bridge.LeaseIdentity) (string, error) {
	authority, err := leappAuthorityForPlan(execution)
	if err != nil || authority == nil {
		return "", err
	}
	if service.leappMirrors == nil {
		return "", model.NewError(model.CodeUnavailable, "Leapp mirror service is unavailable", nil)
	}
	path, err := service.leappMirrors.Ensure(ctx, identity, *authority)
	if err != nil {
		return "", model.Wrap(model.CodeUnavailable, "ensure private Leapp mirror", err)
	}
	return path, nil
}

func (service *LifecycleService) stopLeappMirror(ctx context.Context, identity bridge.LeaseIdentity) error {
	if service == nil || service.leappMirrors == nil {
		return nil
	}
	if err := service.leappMirrors.Stop(ctx, identity); err != nil {
		return model.Wrap(model.CodeUnavailable, "stop private Leapp mirror", err)
	}
	return nil
}

func (service *LifecycleService) leappMirrorWarnings(ctx context.Context, identity bridge.LeaseIdentity) []string {
	if service == nil || service.leappMirrors == nil {
		return []string{"Leapp mirror: unavailable"}
	}
	status, err := service.leappMirrors.Status(ctx, identity)
	if err != nil {
		return []string{"Leapp mirror: status unavailable"}
	}
	switch status.State {
	case "running":
		return nil
	case "error":
		if status.Failure != "" {
			return []string{"Leapp mirror: " + status.Failure}
		}
		return []string{"Leapp mirror: error"}
	default:
		return []string{"Leapp mirror: " + status.State}
	}
}
func (service *LifecycleService) guestHelperForPlan(execution plan.ExecutionPlan) (runtime.HostPath, error) {
	if service.guestHelperSource == nil {
		return "", model.NewError(model.CodeUnavailable, "guest execution helper is not configured", nil)
	}
	helper, err := service.guestHelperSource()
	if err != nil {
		return "", model.Wrap(model.CodeUnavailable, "resolve dsx-guest helper", err)
	}
	if err := validateGuestHelperMountSource(helper); err != nil {
		return "", model.Wrap(model.CodeUnavailable, "validate dsx-guest helper", err)
	}
	if filepath.Base(string(helper)) != "dsx-guest" {
		return "", model.NewError(model.CodeUnavailable, "guest helper must be installed as dsx-guest", nil)
	}
	if pathWithin(execution.Project.CanonicalRoot, string(helper)) {
		return "", model.NewError(model.CodeUnavailable, "guest helper must be installed outside the project workspace", nil)
	}
	return helper, nil
}

func guestGraphEnabled(execution plan.ExecutionPlan) bool {
	return len(execution.Setup) != 0 || len(execution.Processes) != 0
}

func guestPathsOverlap(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func validateGuestHelperSource(source runtime.HostPath) error {
	value := string(source)
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return errors.New("helper source must be a clean absolute path")
	}
	resolved, err := filepath.EvalSymlinks(value)
	if err != nil {
		return err
	}
	if resolved != value {
		return errors.New("helper source must not contain symlink components")
	}
	info, err := os.Stat(value)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return errors.New("helper source must be an executable regular file")
	}
	return nil
}
func portRequestsFromBindings(bindings []runtime.PortBinding) []runtime.PortRequest {
	requests := make([]runtime.PortRequest, 0, len(bindings))
	for _, binding := range bindings {
		hostPort := binding.HostPort
		requests = append(requests, runtime.PortRequest{
			HostIP: binding.HostIP, HostPort: &hostPort, GuestPort: binding.GuestPort, Protocol: binding.Protocol,
		})
	}
	return requests
}
func publishedBindingsFromManifest(manifest state.Manifest) []ports.PublishedBinding {
	result := make([]ports.PublishedBinding, len(manifest.HostBindings))
	for index, binding := range manifest.HostBindings {
		result[index] = ports.PublishedBinding{
			Name: binding.Name, HostIP: binding.HostIP, HostPort: binding.HostPort,
			GuestPort: binding.GuestPort, Protocol: binding.Protocol,
		}
	}
	return result
}

func fallbackManifestBindings(manifest state.Manifest, execution plan.ExecutionPlan, capabilities runtime.Capabilities) []ports.PublishedBinding {
	if !ports.UsesFallback(execution.Ports, capabilities) {
		return nil
	}
	return publishedBindingsFromManifest(manifest)
}

func manifestBindingRecords(bindings []ports.PublishedBinding) []state.HostBindingRecord {
	result := make([]state.HostBindingRecord, len(bindings))
	for index, binding := range bindings {
		result[index] = state.HostBindingRecord{
			Name: binding.Name, HostIP: binding.HostIP, HostPort: binding.HostPort,
			GuestPort: binding.GuestPort, Protocol: binding.Protocol,
		}
	}
	return result
}

func (service *LifecycleService) recordHostBindings(ctx context.Context, manifest *state.Manifest, bindings []ports.PublishedBinding) error {
	if len(bindings) == 0 && len(manifest.HostBindings) == 0 {
		return nil
	}
	previous := append([]state.HostBindingRecord(nil), manifest.HostBindings...)
	manifest.HostBindings = manifestBindingRecords(bindings)
	if err := service.replace(ctx, manifest); err != nil {
		manifest.HostBindings = previous
		return err
	}
	return nil
}

func requestedNamedSandbox(value string) (model.SandboxName, error) {
	if value == "" {
		return "", nil
	}
	sandbox, err := model.ParseSandboxName(value)
	if err != nil {
		return "", model.NewError(model.CodeInvalidInput, "invalid sandbox selector", err)
	}
	if sandbox == liveSandboxName {
		return "", model.NewError(model.CodeInvalidInput, "main is the live workspace and cannot be selected with --name", nil)
	}
	return sandbox, nil
}

func matchingSandboxManifests(manifests []state.Manifest, sandbox model.SandboxName) []state.Manifest {
	matches := make([]state.Manifest, 0, 1)
	for _, manifest := range manifests {
		if manifest.Sandbox == sandbox {
			matches = append(matches, manifest)
		}
	}
	return matches
}

func (service *LifecycleService) oneNamedCloneManifest(ctx context.Context, projectID model.ProjectID, sandbox model.SandboxName) (state.Manifest, error) {
	manifests, err := service.manifests.ListProjectManifests(ctx, projectID)
	if err != nil {
		return state.Manifest{}, err
	}
	matches := matchingSandboxManifests(manifests, sandbox)
	if len(matches) == 0 {
		return state.Manifest{}, model.NewError(model.CodeConflict, fmt.Sprintf("sandbox %s does not exist", sandbox), nil)
	}
	if len(matches) != 1 {
		return state.Manifest{}, model.NewError(model.CodeAmbiguous, fmt.Sprintf("sandbox %s has multiple lifecycle manifests", sandbox), nil)
	}
	if matches[0].Mode != model.ModeClone {
		return state.Manifest{}, model.NewError(model.CodeInvalidInput, fmt.Sprintf("sandbox %s is not a named clone", sandbox), nil)
	}
	return matches[0], nil
}

func discardProtectedCloneWork(manifest *state.Manifest) {
	manifest.UncapturedWork = false
	for index := range manifest.Git {
		record := &manifest.Git[index]
		if !record.HasResultWork() || record.ResultFetched() {
			continue
		}
		record.ResultCommit = ""
		record.ResultBundleDigest = ""
		record.FetchedCommit = ""
		record.FetchedHostRef = ""
	}
}

func (service *LifecycleService) oneLiveManifest(ctx context.Context, projectID model.ProjectID) (state.Manifest, error) {
	manifests, err := service.manifests.ListProjectManifests(ctx, projectID)
	if err != nil {
		return state.Manifest{}, err
	}
	active := activeManifests(manifests)
	if len(active) == 0 {
		return state.Manifest{}, model.NewError(model.CodeConflict, "project has no live sandbox", nil)
	}
	if len(active) != 1 || active[0].Mode != model.ModeLive || active[0].Sandbox != liveSandboxName {
		return state.Manifest{}, model.NewError(model.CodeAmbiguous, "project has multiple or non-live active manifests", nil)
	}
	return active[0], nil
}

func (service *LifecycleService) runtimeProjectIDs(ctx context.Context) ([]model.ProjectID, error) {
	ids := make(map[model.ProjectID]struct{})
	seen := make(map[runtime.ResourceID]struct{})
	for _, kind := range []runtime.ResourceKind{runtime.ResourceWorkspace, runtime.ResourceBrowser, runtime.ResourceNetwork, runtime.ResourceVolume} {
		resources, err := service.runtime.List(ctx, kind)
		if err != nil {
			return nil, err
		}
		for _, resource := range resources {
			if _, duplicate := seen[resource.ID]; duplicate {
				continue
			}
			seen[resource.ID] = struct{}{}
			for _, projectID := range snapshotProjectIDs(resource) {
				ids[projectID] = struct{}{}
			}
		}
	}
	result := make([]model.ProjectID, 0, len(ids))
	for projectID := range ids {
		result = append(result, projectID)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func (service *LifecycleService) unmanifestedProjectResources(ctx context.Context, projectID model.ProjectID, manifests []state.Manifest) ([]string, error) {
	expected := make(map[runtime.ResourceID]struct{})
	for _, manifest := range manifests {
		for _, resource := range manifest.Resources {
			expected[runtime.ResourceID(resource.ExpectedID)] = struct{}{}
		}
	}
	preserved := make([]string, 0)
	seen := make(map[runtime.ResourceID]struct{})
	for _, kind := range []runtime.ResourceKind{runtime.ResourceWorkspace, runtime.ResourceBrowser, runtime.ResourceNetwork, runtime.ResourceVolume} {
		resources, err := service.runtime.List(ctx, kind)
		if err != nil {
			return preserved, err
		}
		for _, resource := range resources {
			if _, duplicate := seen[resource.ID]; duplicate {
				continue
			}
			seen[resource.ID] = struct{}{}
			if !snapshotReferencesProject(resource, projectID) {
				continue
			}
			if _, authorized := expected[resource.ID]; authorized {
				continue
			}
			preserved = append(preserved, resource.Name)
		}
	}
	if len(preserved) == 0 {
		return nil, nil
	}
	sort.Strings(preserved)
	return preserved, model.NewError(model.CodeAmbiguous, "manifestless DSX-labeled resources were preserved: "+strings.Join(preserved, ", "), nil)
}

func snapshotReferencesProject(snapshot runtime.ResourceSnapshot, projectID model.ProjectID) bool {
	for _, candidate := range snapshotProjectIDs(snapshot) {
		if candidate == projectID {
			return true
		}
	}
	return false
}

func snapshotProjectIDs(snapshot runtime.ResourceSnapshot) []model.ProjectID {
	found := make(map[model.ProjectID]struct{}, 2)
	for _, label := range snapshot.Labels {
		if label.Key != state.OwnershipProjectLabel {
			continue
		}
		if projectID, err := model.ParseProjectID(label.Value); err == nil {
			found[projectID] = struct{}{}
		}
	}
	if strings.HasPrefix(snapshot.Name, "dsx-") {
		remainder := strings.TrimPrefix(snapshot.Name, "dsx-")
		if candidate, _, exists := strings.Cut(remainder, "-"); exists {
			if projectID, err := model.ParseProjectID(candidate); err == nil {
				found[projectID] = struct{}{}
			}
		}
	}
	result := make([]model.ProjectID, 0, len(found))
	for projectID := range found {
		result = append(result, projectID)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func activeManifests(manifests []state.Manifest) []state.Manifest {
	result := make([]state.Manifest, 0, len(manifests))
	for _, manifest := range manifests {
		if manifest.State != model.StateDeleted {
			result = append(result, manifest)
		}
	}
	return result
}

func requireLifecycleCapabilities(execution plan.ExecutionPlan, capabilities runtime.Capabilities) error {
	missing := make([]string, 0)
	for name, available := range map[string]bool{
		"service":    capabilities.ServiceHealthy,
		"inspection": capabilities.MachineReadableInspection,
		"labels":     capabilities.Labels,
		"networks":   capabilities.Networks,
		"volumes":    capabilities.Volumes,
		"copy":       capabilities.Copy,
	} {
		if !available {
			missing = append(missing, name)
		}
	}
	if execution.Image.Reference == "" && !capabilities.BuilderHealthy {
		missing = append(missing, "builder")
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return model.NewError(model.CodeUnavailable, "Apple runtime lacks required lifecycle capabilities: "+strings.Join(missing, ", "), nil)
	}
	return nil
}

func portPlanError(err error) error {
	switch {
	case errors.Is(err, ports.ErrInvalidRequest):
		return model.NewError(model.CodeInvalidInput, "invalid port publication plan", err)
	case errors.Is(err, ports.ErrConflict), errors.Is(err, ports.ErrReservationState):
		return model.NewError(model.CodeConflict, "port publication conflict", err)
	default:
		return model.NewError(model.CodeUnavailable, "port publication is unavailable", err)
	}
}

func (service *LifecycleService) findRuntimeResource(ctx context.Context, record state.ResourceRecord) (runtime.ResourceSnapshot, bool, error) {
	resource, err := service.runtime.Inspect(ctx, runtime.ResourceID(record.ExpectedID))
	if errors.Is(err, runtime.ErrResourceNotFound) {
		return runtime.ResourceSnapshot{}, false, nil
	}
	if err != nil {
		return runtime.ResourceSnapshot{}, false, err
	}
	return resource, true, nil
}

func manifestResource(manifest state.Manifest, kind runtime.ResourceKind, role string) (state.ResourceRecord, error) {
	for _, resource := range manifest.Resources {
		if resource.Kind == string(kind) && resource.Role == role {
			return resource, nil
		}
	}
	return state.ResourceRecord{}, model.NewError(model.CodeInternal, fmt.Sprintf("manifest has no %s resource", role), nil)
}

func projectIDForRoot(root string) (model.ProjectID, error) {
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", model.Wrap(model.CodeInvalidInput, "resolve project root", err)
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return "", model.Wrap(model.CodeInvalidInput, "canonicalize project root", err)
	}
	projectID, err := model.NewProjectID(canonical)
	if err != nil {
		return "", model.Wrap(model.CodeInvalidInput, "derive project identity", err)
	}
	return projectID, nil
}

func (service *LifecycleService) replace(ctx context.Context, manifest *state.Manifest) error {
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

func (service *LifecycleService) transition(ctx context.Context, manifest *state.Manifest, next model.SandboxState, operation, failure string) error {
	if manifest.State != next {
		if err := manifest.State.Transition(next); err != nil {
			return model.Wrap(model.CodeInternal, "transition manifest state", err)
		}
	}
	manifest.State = next
	manifest.Operation = operation
	manifest.Failure = boundedFailure(failure)
	return service.replace(ctx, manifest)
}

func boundedFailure(value string) string {
	const maximum = 4096
	value = strings.TrimSpace(value)
	if len(value) > maximum {
		return value[:maximum]
	}
	return value
}

func volumeRole(name string) string {
	digest := sha256.Sum256([]byte(name))
	prefix := strings.ToLower(name)
	var builder strings.Builder
	var last byte
	for _, character := range prefix {
		switch {
		case (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9'):
			builder.WriteRune(character)
			last = byte(character)
		case builder.Len() != 0 && last != '-':
			builder.WriteByte('-')
			last = '-'
		}
		if builder.Len() == 10 {
			break
		}
	}
	clean := strings.Trim(builder.String(), "-")
	if clean == "" {
		clean = "data"
	}
	return "vol-" + clean + "-" + hex.EncodeToString(digest[:4])
}

func runtimeLabels(labels []state.OwnershipLabel) []runtime.Label {
	result := make([]runtime.Label, len(labels))
	for index, label := range labels {
		result[index] = runtime.Label{Key: label.Key, Value: label.Value}
	}
	return result
}
