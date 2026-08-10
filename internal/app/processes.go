package app

import (
	"context"
	"errors"

	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/ports"
	"github.com/srimajji/dsx/internal/runtime"
)

type GuestController interface {
	Reconcile(context.Context, runtime.ResourceSnapshot) error
	Start(context.Context, runtime.ResourceSnapshot, plan.ExecutionPlan, uint64) (guestproto.StartResult, error)
	Status(context.Context, runtime.ResourceSnapshot) (guestproto.StatusResult, error)
	Shutdown(context.Context, runtime.ResourceSnapshot) error
}
type ProcessStatusRequest struct {
	Root string
}

type ProcessStatusResult struct {
	URLs       []string                   `json:"urls,omitempty"`
	Generation uint64                     `json:"generation"`
	Failed     bool                       `json:"failed"`
	Processes  []guestproto.ProcessStatus `json:"processes"`
	Warnings   []string                   `json:"warnings,omitempty"`
}

type ProcessLogsRequest struct {
	Root   string
	Target string
}

type ProcessLogsResult struct {
	Target       string `json:"target"`
	Log          string `json:"log"`
	DroppedBytes uint64 `json:"dropped_bytes"`
}

func (service *LifecycleService) ProcessStatus(ctx context.Context, request ProcessStatusRequest) (result ProcessStatusResult, err error) {
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
	locked := true
	defer func() {
		if locked {
			err = errors.Join(err, model.Wrap(model.CodeInternal, "unlock project lifecycle", lock.Unlock()))
		}
	}()
	manifest, err := service.oneLiveManifest(ctx, projectID)
	if err != nil {
		return result, err
	}
	current, err := service.revalidateApprovedPlan(ctx, StartRequest{Root: request.Root, ApproveConfig: manifest.PlanHash}, manifest.PlanHash)
	if err != nil {
		return result, err
	}
	workspace, urls, err := service.inspectVerifiedWorkspace(ctx, manifest, current)
	if err != nil {
		return result, err
	}
	capabilities, capabilityErr := service.runtime.Probe(ctx)
	publicationRelay := capabilityErr == nil && len(manifest.HostBindings) != 0 && ports.UsesFallback(current.Ports, capabilities)
	warnings := service.persistentHostBridgeWarnings(ctx, current, bridge.LeaseIdentity{ProjectID: manifest.ProjectID, Sandbox: manifest.Sandbox, RunID: manifest.RunID}, publicationRelay)
	if !guestGraphEnabled(current) {
		return ProcessStatusResult{URLs: append([]string(nil), urls...), Processes: []guestproto.ProcessStatus{}, Warnings: warnings}, nil
	}
	if service.guest == nil {
		return result, model.NewError(model.CodeUnavailable, "guest process service is unavailable", nil)
	}
	if unlockErr := lock.Unlock(); unlockErr != nil {
		return result, model.Wrap(model.CodeInternal, "unlock project lifecycle", unlockErr)
	}
	locked = false
	status, err := service.guest.Status(ctx, workspace)
	if err != nil {
		return result, err
	}
	if terminalRequiredGuestFailure(status) {
		failureLock, lockErr := service.locks.LockProject(ctx, projectID)
		if lockErr != nil {
			return result, model.Wrap(model.CodeConflict, "lock failed guest lifecycle", lockErr)
		}
		latest, found, loadErr := service.manifests.LoadManifest(ctx, manifest.ProjectID, manifest.Sandbox, manifest.RunID)
		if loadErr == nil && !found {
			loadErr = model.NewError(model.CodeConflict, "running sandbox manifest disappeared while recording guest failure", nil)
		}
		if loadErr == nil && latest.State == model.StateRunning {
			loadErr = service.transition(ctx, &latest, model.StateFailed, latest.Operation, "required guest process failed")
		}
		unlockErr := failureLock.Unlock()
		if loadErr != nil || unlockErr != nil {
			return result, errors.Join(loadErr, model.Wrap(model.CodeInternal, "unlock failed guest lifecycle", unlockErr))
		}
	}
	result.Generation = status.Generation
	result.URLs = append([]string(nil), urls...)
	result.Failed = status.Failed
	result.Processes = append([]guestproto.ProcessStatus(nil), status.Processes...)
	result.Warnings = warnings
	return result, nil
}

func terminalRequiredGuestFailure(status guestproto.StatusResult) bool {
	for _, process := range status.Processes {
		if !process.Required {
			continue
		}
		if process.State == guestproto.StateFailed || process.State == guestproto.StateExited {
			return true
		}
	}
	return false
}

func (service *LifecycleService) ProcessLogs(ctx context.Context, request ProcessLogsRequest) (ProcessLogsResult, error) {
	if request.Target == "" {
		return ProcessLogsResult{}, model.NewError(model.CodeInvalidInput, "process target is required", nil)
	}
	status, err := service.ProcessStatus(ctx, ProcessStatusRequest{Root: request.Root})
	if err != nil {
		return ProcessLogsResult{}, err
	}
	for _, process := range status.Processes {
		if process.ID == request.Target {
			return ProcessLogsResult{Target: process.ID, Log: process.Log, DroppedBytes: process.LogDropped}, nil
		}
	}
	return ProcessLogsResult{}, model.NewError(model.CodeInvalidInput, "process target is not configured", nil)
}
