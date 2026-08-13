package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

func TestAWSApplicationWritesGrantBeforeHelperAndRollsBackBeforeCleanup(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	manager := newRecordingHostAWSManager()
	configureHostDefaultAWS(t, fixture, manager)
	createAWSWorkspace(t, fixture, "alpha")

	manager.enableErr = errors.New("injected enable failure")
	manager.onEnable = func(identity bridge.LeaseIdentity) {
		manifest := fixture.manifest(identity.Workspace)
		if manifest.AWSGrant == nil || !manifest.AWSGrant.Enabled || manifest.Operation != "aws-enable" {
			t.Fatalf("manifest at helper enable = %#v", manifest)
		}
	}
	manager.onDisable = func(identity bridge.LeaseIdentity) {
		manifest := fixture.manifest(identity.Workspace)
		if manifest.AWSGrant == nil || manifest.AWSGrant.Enabled {
			t.Fatalf("manifest at rollback cleanup = %#v", manifest)
		}
	}
	if _, err := fixture.service.EnableAWS(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"}); err == nil {
		t.Fatal("injected mirror enable failure was accepted")
	}
	manifest := fixture.manifest("alpha")
	if manifest.AWSGrant == nil || manifest.AWSGrant.Enabled || manifest.Operation != "" {
		t.Fatalf("rolled back AWS grant = %#v", manifest.AWSGrant)
	}
	if !slices.Equal(manager.events, []string{"prepare:alpha", "enable:alpha", "disable:alpha"}) {
		t.Fatalf("manager events = %v", manager.events)
	}
}

func TestAWSApplicationDisablesDurablyBeforeHelperCleanup(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	manager := newRecordingHostAWSManager()
	configureHostDefaultAWS(t, fixture, manager)
	createAWSWorkspace(t, fixture, "alpha")
	if _, err := fixture.service.EnableAWS(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"}); err != nil {
		t.Fatal(err)
	}
	manager.onDisable = func(identity bridge.LeaseIdentity) {
		manifest := fixture.manifest(identity.Workspace)
		if manifest.AWSGrant == nil || manifest.AWSGrant.Enabled || manifest.Operation != "aws-disable" {
			t.Fatalf("manifest at disable = %#v", manifest)
		}
	}
	result, err := fixture.service.DisableAWS(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Enabled || result.MirrorHealth != AWSMirrorDisabled {
		t.Fatalf("disable result = %#v", result)
	}
}

func TestAWSApplicationStoppedGrantPersistsAndRefreshesBeforeStart(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	manager := newRecordingHostAWSManager()
	configureHostDefaultAWS(t, fixture, manager)
	createAWSWorkspace(t, fixture, "alpha")

	if _, err := fixture.service.Stop(context.Background(), WorkspaceStopRequest{Root: fixture.root, Workspace: "alpha"}); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.EnableAWS(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Enabled || result.MirrorHealth != AWSMirrorStopped || manager.state("alpha").State != AWSMirrorDisabled {
		t.Fatalf("stopped enable result = %#v, manager = %#v", result, manager.state("alpha"))
	}
	if grant := fixture.manifest("alpha").AWSGrant; grant == nil || !grant.Enabled {
		t.Fatalf("stopped workspace did not persist enabled grant: %#v", grant)
	}

	fixture.runtime.startHook = func() {
		if manager.state("alpha").State != AWSMirrorCurrent {
			t.Fatalf("workspace started before a current AWS generation: %#v", manager.state("alpha"))
		}
	}
	if _, err := fixture.service.Start(context.Background(), WorkspaceStartRequest{Root: fixture.root, Workspace: "alpha"}); err != nil {
		t.Fatal(err)
	}
	beforeRestart := manager.enableCalls
	if _, err := fixture.service.Restart(context.Background(), WorkspaceRestartRequest{Root: fixture.root, Workspace: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if manager.enableCalls != beforeRestart+1 || fixture.manifest("alpha").AWSGrant == nil || !fixture.manifest("alpha").AWSGrant.Enabled {
		t.Fatalf("restart did not refresh and preserve AWS grant: enables=%d manifest=%#v", manager.enableCalls, fixture.manifest("alpha").AWSGrant)
	}
	if _, err := fixture.service.Stop(context.Background(), WorkspaceStopRequest{Root: fixture.root, Workspace: "alpha"}); err != nil {
		t.Fatal(err)
	}
	status, err := fixture.service.AWSStatus(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.MirrorHealth != AWSMirrorStopped || fixture.manifest("alpha").AWSGrant == nil || !fixture.manifest("alpha").AWSGrant.Enabled {
		t.Fatalf("stopped status = %#v, grant = %#v", status, fixture.manifest("alpha").AWSGrant)
	}
}

func TestAWSApplicationSiblingIsolationAndExecutionEnvironment(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	manager := newRecordingHostAWSManager()
	configureHostDefaultAWS(t, fixture, manager)
	createAWSWorkspace(t, fixture, "alpha")
	createAWSWorkspace(t, fixture, "beta")
	if _, err := fixture.service.EnableAWS(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"}); err != nil {
		t.Fatal(err)
	}

	alpha := fixture.manifest("alpha")
	beta := fixture.manifest("beta")
	if alpha.AWSGrant == nil || !alpha.AWSGrant.Enabled || beta.AWSGrant == nil || beta.AWSGrant.Enabled {
		t.Fatalf("sibling grants: alpha=%#v beta=%#v", alpha.AWSGrant, beta.AWSGrant)
	}
	alphaSpec, err := fixture.service.PrepareWorkspaceExecution(context.Background(), alpha, runtime.ExecSpec{Env: []string{"KEEP=yes", "AWS_PROFILE=named", "AWS_CONFIG_FILE=/host/config"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"KEEP=yes", "AWS_CONFIG_FILE=" + bridge.HostAWSConfigGuestPath, "AWS_SHARED_CREDENTIALS_FILE=" + bridge.HostAWSCredentialsGuestPath}
	if !slices.Equal(alphaSpec.Env, want) {
		t.Fatalf("enabled environment = %v, want %v", alphaSpec.Env, want)
	}
	statusCalls := manager.statusCalls
	betaSpec, err := fixture.service.PrepareWorkspaceExecution(context.Background(), beta, runtime.ExecSpec{Env: []string{"AWS_PROFILE=named", "AWS_SHARED_CREDENTIALS_FILE=/host/credentials", "KEEP=yes"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(betaSpec.Env, []string{"KEEP=yes"}) || manager.statusCalls != statusCalls {
		t.Fatalf("disabled sibling environment=%v status calls=%d", betaSpec.Env, manager.statusCalls-statusCalls)
	}
}

func TestAWSApplicationStatusUsesStableNonSecretValues(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	manager := newRecordingHostAWSManager()
	configureHostDefaultAWS(t, fixture, manager)
	createAWSWorkspace(t, fixture, "alpha")
	if _, err := fixture.service.EnableAWS(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"}); err != nil {
		t.Fatal(err)
	}

	current, err := fixture.service.AWSStatus(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if !current.Enabled || current.HostAvailability != AWSHostAvailable || current.MirrorHealth != AWSMirrorCurrent || current.FailureCode != "" {
		t.Fatalf("current status = %#v", current)
	}
	manager.setState("alpha", bridge.HostAWSMirrorStatus{State: AWSMirrorDegraded, Failure: "source_unsafe"})
	degraded, err := fixture.service.AWSStatus(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if degraded.MirrorHealth != AWSMirrorDegraded || degraded.FailureCode != "source_unsafe" {
		t.Fatalf("degraded status = %#v", degraded)
	}
	manager.setState("alpha", bridge.HostAWSMirrorStatus{State: AWSMirrorDegraded, Failure: "credential-bytes-must-not-escape"})
	sanitized, err := fixture.service.AWSStatus(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if sanitized.FailureCode != "mirror-degraded" {
		t.Fatalf("unrecognized failure was returned: %#v", sanitized)
	}
	manager.setState("alpha", bridge.HostAWSMirrorStatus{State: AWSMirrorCurrent})
	if err := os.Remove(filepath.Join(fixture.execution.AWS.SourceDirectory, "credentials")); err != nil {
		t.Fatal(err)
	}
	unavailable, err := fixture.service.AWSStatus(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if unavailable.HostAvailability != AWSHostUnavailable || unavailable.MirrorHealth != AWSMirrorCurrent || unavailable.FailureCode != "host-unavailable" {
		t.Fatalf("host-unavailable status = %#v", unavailable)
	}
}

func TestAWSApplicationPlanNoneRejectsWithoutSourceHelperOrEnvironment(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	manager := newRecordingHostAWSManager()
	fixture.service.hostAWS = manager
	createAWSWorkspace(t, fixture, "alpha")
	if _, err := fixture.service.EnableAWS(context.Background(), AWSWorkspaceRequest{Root: fixture.root, Workspace: "alpha"}); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("plan-none enable error = %v", err)
	}
	manifest := fixture.manifest("alpha")
	if manifest.AWSGrant != nil || len(manager.events) != 0 {
		t.Fatalf("plan-none grant=%#v manager events=%v", manifest.AWSGrant, manager.events)
	}
	spec, err := fixture.service.PrepareWorkspaceExecution(context.Background(), manifest, runtime.ExecSpec{Env: []string{"AWS_PROFILE=named", "AWS_CONFIG_FILE=/host/config", "KEEP=yes"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.Env, []string{"KEEP=yes"}) || len(manager.events) != 0 || manager.statusCalls != 0 {
		t.Fatalf("plan-none execution environment=%v events=%v status=%d", spec.Env, manager.events, manager.statusCalls)
	}
}

func createAWSWorkspace(t *testing.T, fixture *workspaceFixture, workspace model.WorkspaceName) {
	t.Helper()
	if _, err := fixture.service.Create(context.Background(), WorkspaceCreateRequest{Root: fixture.root, Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
}

type recordingHostAWSManager struct {
	events      []string
	states      map[string]bridge.HostAWSMirrorStatus
	enableCalls int
	statusCalls int
	enableErr   error
	disableErr  error
	removeErr   error
	onEnable    func(bridge.LeaseIdentity)
	onDisable   func(bridge.LeaseIdentity)
	onRemove    func(bridge.LeaseIdentity)
}

func newRecordingHostAWSManager() *recordingHostAWSManager {
	return &recordingHostAWSManager{states: map[string]bridge.HostAWSMirrorStatus{}}
}

func (manager *recordingHostAWSManager) Prepare(_ context.Context, identity bridge.LeaseIdentity) (string, error) {
	manager.events = append(manager.events, "prepare:"+string(identity.Workspace))
	manager.states[string(identity.Workspace)] = bridge.HostAWSMirrorStatus{State: AWSMirrorDisabled}
	return filepath.Join("/private/dsx/aws", string(identity.ProjectID), string(identity.Workspace), string(identity.RunID)), nil
}

func (manager *recordingHostAWSManager) Enable(_ context.Context, identity bridge.LeaseIdentity, _ bridge.HostAWSAuthority) (string, error) {
	manager.events = append(manager.events, "enable:"+string(identity.Workspace))
	manager.enableCalls++
	if manager.onEnable != nil {
		manager.onEnable(identity)
	}
	if manager.enableErr != nil {
		return "", manager.enableErr
	}
	manager.states[string(identity.Workspace)] = bridge.HostAWSMirrorStatus{State: AWSMirrorCurrent}
	return filepath.Join("/private/dsx/aws", string(identity.ProjectID), string(identity.Workspace), string(identity.RunID)), nil
}

func (manager *recordingHostAWSManager) Disable(_ context.Context, identity bridge.LeaseIdentity) error {
	manager.events = append(manager.events, "disable:"+string(identity.Workspace))
	if manager.onDisable != nil {
		manager.onDisable(identity)
	}
	if manager.disableErr != nil {
		return manager.disableErr
	}
	manager.states[string(identity.Workspace)] = bridge.HostAWSMirrorStatus{State: AWSMirrorDisabled}
	return nil
}

func (manager *recordingHostAWSManager) Remove(_ context.Context, identity bridge.LeaseIdentity) error {
	manager.events = append(manager.events, "remove:"+string(identity.Workspace))
	if manager.onRemove != nil {
		manager.onRemove(identity)
	}
	if manager.removeErr != nil {
		return manager.removeErr
	}
	delete(manager.states, string(identity.Workspace))
	return nil
}

func (manager *recordingHostAWSManager) Status(_ context.Context, identity bridge.LeaseIdentity) (bridge.HostAWSMirrorStatus, error) {
	manager.statusCalls++
	return manager.states[string(identity.Workspace)], nil
}

func (manager *recordingHostAWSManager) state(workspace model.WorkspaceName) bridge.HostAWSMirrorStatus {
	return manager.states[string(workspace)]
}

func (manager *recordingHostAWSManager) setState(workspace model.WorkspaceName, status bridge.HostAWSMirrorStatus) {
	manager.states[string(workspace)] = status
}

var _ bridge.HostAWSWorkspaceManager = (*recordingHostAWSManager)(nil)
