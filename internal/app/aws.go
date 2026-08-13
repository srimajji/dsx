package app

import (
	"context"
	"errors"
	"strings"

	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

const (
	AWSHostAvailable   = "available"
	AWSHostUnavailable = "unavailable"

	AWSMirrorDisabled = "disabled"
	AWSMirrorStopped  = "stopped"
	AWSMirrorCurrent  = "current"
	AWSMirrorDegraded = "degraded"
)

type AWSWorkspaceRequest struct {
	Root      string
	Workspace model.WorkspaceName
}

type AWSWorkspaceResult struct {
	Workspace        model.WorkspaceName `json:"workspace"`
	Enabled          bool                `json:"enabled"`
	HostAvailability string              `json:"host_availability"`
	MirrorHealth     string              `json:"mirror_health"`
	FailureCode      string              `json:"failure_code"`
}

type AWSService struct {
	workspaces *WorkspaceService
}

func NewAWSService(workspaces *WorkspaceService) *AWSService {
	return &AWSService{workspaces: workspaces}
}

func (service *AWSService) Enable(ctx context.Context, request AWSWorkspaceRequest) (AWSWorkspaceResult, error) {
	if service == nil || service.workspaces == nil {
		return AWSWorkspaceResult{}, model.NewError(model.CodeUnavailable, "AWS workspace service is unavailable", nil)
	}
	return service.workspaces.EnableAWS(ctx, request)
}

func (service *AWSService) Disable(ctx context.Context, request AWSWorkspaceRequest) (AWSWorkspaceResult, error) {
	if service == nil || service.workspaces == nil {
		return AWSWorkspaceResult{}, model.NewError(model.CodeUnavailable, "AWS workspace service is unavailable", nil)
	}
	return service.workspaces.DisableAWS(ctx, request)
}

func (service *AWSService) Status(ctx context.Context, request AWSWorkspaceRequest) (AWSWorkspaceResult, error) {
	if service == nil || service.workspaces == nil {
		return AWSWorkspaceResult{}, model.NewError(model.CodeUnavailable, "AWS workspace service is unavailable", nil)
	}
	return service.workspaces.AWSStatus(ctx, request)
}

func (service *WorkspaceService) EnableAWS(ctx context.Context, request AWSWorkspaceRequest) (result AWSWorkspaceResult, returnErr error) {
	access, unlock, err := service.workspaceAccess(ctx, request.Root, request.Workspace, false)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	if service.hostAWS == nil {
		return result, model.NewError(model.CodeUnavailable, "host AWS workspace manager is unavailable", nil)
	}
	if access.Manifest.AWSGrant == nil {
		return result, model.NewError(model.CodeConflict, "workspace plan does not grant host AWS access", nil)
	}
	authority, err := validateCurrentHostAWS(access.Plan)
	if err != nil {
		return result, err
	}
	if !access.Manifest.AWSGrant.Enabled {
		access.Manifest.AWSGrant.Enabled = true
		if err := service.transitionManifest(ctx, access.Manifest, access.Manifest.State, "aws-enable", ""); err != nil {
			return result, err
		}
	}
	if err := service.enableHostAWS(ctx, access.Manifest, authority); err != nil {
		return result, err
	}
	health := AWSMirrorCurrent
	if access.Manifest.State == model.StateStopped {
		if err := service.hostAWS.Disable(ctx, workspaceLeaseIdentity(*access.Manifest)); err != nil {
			return result, service.rollbackHostAWSEnable(ctx, access.Manifest, err)
		}
		health = AWSMirrorStopped
	}
	access.Manifest.Operation, access.Manifest.Failure = "", ""
	if err := service.replaceManifest(ctx, access.Manifest); err != nil {
		return result, errors.Join(err, service.rollbackHostAWSEnable(ctx, access.Manifest, err))
	}
	return AWSWorkspaceResult{Workspace: access.Manifest.Workspace, Enabled: true, HostAvailability: AWSHostAvailable, MirrorHealth: health}, nil
}

func (service *WorkspaceService) DisableAWS(ctx context.Context, request AWSWorkspaceRequest) (result AWSWorkspaceResult, returnErr error) {
	access, unlock, err := service.workspaceAccess(ctx, request.Root, request.Workspace, false)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	if _, err := approvedHostAWSAuthority(access.Plan); err != nil {
		return result, err
	}
	if access.Manifest.AWSGrant == nil {
		return result, model.NewError(model.CodeConflict, "workspace plan does not grant host AWS access", nil)
	}
	if !access.Manifest.AWSGrant.Enabled {
		return AWSWorkspaceResult{Workspace: access.Manifest.Workspace, HostAvailability: hostAWSAvailability(access.Plan), MirrorHealth: AWSMirrorDisabled}, nil
	}
	if service.hostAWS == nil {
		return result, model.NewError(model.CodeUnavailable, "host AWS workspace manager is unavailable", nil)
	}
	access.Manifest.AWSGrant.Enabled = false
	if err := service.transitionManifest(ctx, access.Manifest, access.Manifest.State, "aws-disable", ""); err != nil {
		return result, err
	}
	if err := service.hostAWS.Disable(ctx, workspaceLeaseIdentity(*access.Manifest)); err != nil {
		access.Manifest.Failure = "host AWS disable failed"
		_ = service.replaceManifest(context.WithoutCancel(ctx), access.Manifest)
		return result, model.Wrap(model.CodeUnavailable, "disable host AWS workspace publication", err)
	}
	access.Manifest.Operation, access.Manifest.Failure = "", ""
	if err := service.replaceManifest(ctx, access.Manifest); err != nil {
		return result, err
	}
	return AWSWorkspaceResult{Workspace: access.Manifest.Workspace, HostAvailability: hostAWSAvailability(access.Plan), MirrorHealth: AWSMirrorDisabled}, nil
}

func (service *WorkspaceService) AWSStatus(ctx context.Context, request AWSWorkspaceRequest) (result AWSWorkspaceResult, returnErr error) {
	access, unlock, err := service.workspaceAccess(ctx, request.Root, request.Workspace, false)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	if _, err := approvedHostAWSAuthority(access.Plan); err != nil {
		return result, err
	}
	if access.Manifest.AWSGrant == nil {
		return result, model.NewError(model.CodeConflict, "workspace plan does not grant host AWS access", nil)
	}
	result = AWSWorkspaceResult{
		Workspace:        access.Manifest.Workspace,
		Enabled:          access.Manifest.AWSGrant.Enabled,
		HostAvailability: hostAWSAvailability(access.Plan),
		MirrorHealth:     AWSMirrorDisabled,
	}
	if result.HostAvailability == AWSHostUnavailable {
		result.FailureCode = "host-unavailable"
	}
	if !result.Enabled {
		return result, nil
	}
	if service.hostAWS == nil {
		result.MirrorHealth, result.FailureCode = AWSMirrorDegraded, "manager-unavailable"
		return result, nil
	}
	status, err := service.hostAWS.Status(ctx, workspaceLeaseIdentity(*access.Manifest))
	if err != nil {
		result.MirrorHealth, result.FailureCode = AWSMirrorDegraded, "status-unavailable"
		return result, nil
	}
	if access.Manifest.State == model.StateStopped && status.State == AWSMirrorDisabled {
		result.MirrorHealth = AWSMirrorStopped
		return result, nil
	}
	switch status.State {
	case AWSMirrorCurrent:
		result.MirrorHealth = AWSMirrorCurrent
	case AWSMirrorStopped:
		result.MirrorHealth = AWSMirrorStopped
	case AWSMirrorDisabled:
		if access.Manifest.State == model.StateStopped {
			result.MirrorHealth = AWSMirrorStopped
		} else {
			result.MirrorHealth, result.FailureCode = AWSMirrorDegraded, "mirror-disabled"
		}
	case AWSMirrorDegraded:
		result.MirrorHealth = AWSMirrorDegraded
		result.FailureCode = safeHostAWSFailureCode(status.Failure)
	default:
		result.MirrorHealth, result.FailureCode = AWSMirrorDegraded, "mirror-degraded"
	}
	return result, nil
}

// PrepareWorkspaceExecution removes ambient AWS path overrides from an
// application execution and adds DSX's fixed default-only paths only when the
// durable workspace grant is enabled and its publication is current. Internal
// maintenance commands deliberately bypass this method.
func (service *WorkspaceService) PrepareWorkspaceExecution(ctx context.Context, manifest state.Manifest, spec runtime.ExecSpec) (runtime.ExecSpec, error) {
	spec.Env = withoutHostAWSEnvironment(spec.Env)
	if manifest.AWSGrant == nil || !manifest.AWSGrant.Enabled {
		return spec, nil
	}
	if service == nil || service.hostAWS == nil {
		return spec, model.NewError(model.CodeUnavailable, "host AWS workspace manager is unavailable", nil)
	}
	status, err := service.hostAWS.Status(ctx, workspaceLeaseIdentity(manifest))
	if err != nil {
		return spec, model.Wrap(model.CodeUnavailable, "inspect host AWS workspace publication", err)
	}
	if status.State != AWSMirrorCurrent {
		return spec, model.NewError(model.CodeUnavailable, "host AWS workspace publication is not current", nil)
	}
	spec.Env = append(spec.Env,
		"AWS_CONFIG_FILE="+bridge.HostAWSConfigGuestPath,
		"AWS_SHARED_CREDENTIALS_FILE="+bridge.HostAWSCredentialsGuestPath,
	)
	return spec, nil
}

func (service *WorkspaceService) enableHostAWS(ctx context.Context, manifest *state.Manifest, authority bridge.HostAWSAuthority) error {
	if _, err := service.hostAWS.Enable(ctx, workspaceLeaseIdentity(*manifest), authority); err != nil {
		return service.rollbackHostAWSEnable(ctx, manifest, err)
	}
	return nil
}

func (service *WorkspaceService) activateHostAWSForRuntime(ctx context.Context, manifest *state.Manifest, execution plan.ExecutionPlan) error {
	if manifest.AWSGrant == nil || !manifest.AWSGrant.Enabled {
		return nil
	}
	if service.hostAWS == nil {
		return model.NewError(model.CodeUnavailable, "host AWS workspace manager is unavailable", nil)
	}
	authority, err := validateCurrentHostAWS(execution)
	if err != nil {
		return service.rollbackHostAWSEnable(ctx, manifest, err)
	}
	return service.enableHostAWS(ctx, manifest, authority)
}

func (service *WorkspaceService) disableHostAWSForStoppedRuntime(ctx context.Context, manifest state.Manifest) error {
	if manifest.AWSGrant == nil || !manifest.AWSGrant.Enabled {
		return nil
	}
	if service.hostAWS == nil {
		return model.NewError(model.CodeUnavailable, "host AWS workspace manager is unavailable", nil)
	}
	return service.hostAWS.Disable(ctx, workspaceLeaseIdentity(manifest))
}

func (service *WorkspaceService) revokeAndRemoveHostAWS(ctx context.Context, manifest *state.Manifest) error {
	if manifest.AWSGrant == nil {
		return nil
	}
	if manifest.AWSGrant.Enabled {
		manifest.AWSGrant.Enabled = false
		if err := service.replaceManifest(ctx, manifest); err != nil {
			return err
		}
	}
	if service.hostAWS == nil {
		return model.NewError(model.CodeUnavailable, "host AWS workspace manager is unavailable", nil)
	}
	if err := service.hostAWS.Remove(ctx, workspaceLeaseIdentity(*manifest)); err != nil {
		return model.Wrap(model.CodeUnavailable, "remove host AWS workspace publication", err)
	}
	return nil
}

func (service *WorkspaceService) rollbackHostAWSEnable(ctx context.Context, manifest *state.Manifest, cause error) error {
	manifest.AWSGrant.Enabled = false
	manifest.Operation, manifest.Failure = "aws-disable", "host AWS enable failed"
	cleanupCtx := context.WithoutCancel(ctx)
	if err := service.replaceManifest(cleanupCtx, manifest); err != nil {
		return errors.Join(model.Wrap(model.CodeUnavailable, "enable host AWS workspace publication", cause), err)
	}
	if err := service.hostAWS.Disable(cleanupCtx, workspaceLeaseIdentity(*manifest)); err != nil {
		return errors.Join(model.Wrap(model.CodeUnavailable, "enable host AWS workspace publication", cause), model.Wrap(model.CodeUnavailable, "rollback host AWS workspace publication", err))
	}
	manifest.Operation = ""
	if err := service.replaceManifest(cleanupCtx, manifest); err != nil {
		return errors.Join(model.Wrap(model.CodeUnavailable, "enable host AWS workspace publication", cause), err)
	}
	return model.Wrap(model.CodeUnavailable, "enable host AWS workspace publication", cause)
}

func approvedHostAWSAuthority(execution plan.ExecutionPlan) (bridge.HostAWSAuthority, error) {
	capability := execution.AWS
	if capability.Mode != plan.AWSModeHostDefault {
		return bridge.HostAWSAuthority{}, model.NewError(model.CodeConflict, "project plan does not allow host-default AWS access", nil)
	}
	if capability.SourceDirectory == "" || capability.SourceIdentity == "" || capability.Destination != plan.AWSGuestDestination || !capability.ReadOnly || capability.EligibleProfile != plan.AWSDefaultProfile || capability.WorkspaceDefaultEnabled || capability.AuthorityModel != plan.AWSAuthorityDynamicHostDefault {
		return bridge.HostAWSAuthority{}, model.NewError(model.CodeAmbiguous, "approved host AWS capability is incomplete", nil)
	}
	return bridge.HostAWSAuthority{DeclaredPath: capability.SourceDirectory, CanonicalPath: capability.SourceDirectory, Identity: capability.SourceIdentity}, nil
}

func validateCurrentHostAWS(execution plan.ExecutionPlan) (bridge.HostAWSAuthority, error) {
	approved, err := approvedHostAWSAuthority(execution)
	if err != nil {
		return bridge.HostAWSAuthority{}, err
	}
	current, err := bridge.ResolveHostAWSDirectory(approved.DeclaredPath)
	if err != nil || current.CanonicalPath != approved.CanonicalPath || current.Identity != approved.Identity {
		return bridge.HostAWSAuthority{}, model.NewError(model.CodeUnavailable, "approved host AWS source is unavailable", err)
	}
	directory, err := bridge.OpenApprovedHostAWSDirectory(approved)
	if err != nil {
		return bridge.HostAWSAuthority{}, model.NewError(model.CodeUnavailable, "approved host AWS source is unavailable", err)
	}
	defer directory.Close()
	snapshot, err := directory.Snapshot()
	if err != nil {
		return bridge.HostAWSAuthority{}, model.NewError(model.CodeUnavailable, "approved host AWS default is unavailable", err)
	}
	_, availability, err := bridge.FilterHostDefaultSnapshot(snapshot)
	if err != nil || availability != bridge.HostDefaultAvailable {
		return bridge.HostAWSAuthority{}, model.NewError(model.CodeUnavailable, "approved host AWS default is unavailable", err)
	}
	return approved, nil
}

func hostAWSAvailability(execution plan.ExecutionPlan) string {
	if _, err := validateCurrentHostAWS(execution); err != nil {
		return AWSHostUnavailable
	}
	return AWSHostAvailable
}

func safeHostAWSFailureCode(failure string) string {
	switch failure {
	case "source_unavailable", "source_unsafe", "source_oversized", "source_identity_changed", "control_failed", "startup_failed":
		return failure
	default:
		return "mirror-degraded"
	}
}

func workspaceLeaseIdentity(manifest state.Manifest) bridge.LeaseIdentity {
	return bridge.LeaseIdentity{ProjectID: manifest.ProjectID, CanonicalRoot: manifest.CanonicalRoot, Workspace: manifest.Workspace, RunID: manifest.RunID}
}

func withoutHostAWSEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name := entry
		if index := strings.IndexByte(name, '='); index >= 0 {
			name = name[:index]
		}
		if name == "AWS_CONFIG_FILE" || name == "AWS_SHARED_CREDENTIALS_FILE" || name == "AWS_PROFILE" || name == "AWS_DEFAULT_PROFILE" {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}
