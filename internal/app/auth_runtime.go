package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"time"

	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/buildinfo"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

const authLoginGuestWorkspace = "/tmp/dsx-auth-workspace"

type RuntimeAuthSessionRunner struct {
	workspaces          *WorkspaceService
	repository          *auth.Repository
	adapters            map[harness.Name]harness.Adapter
	agentImageReference string
}

func NewRuntimeAuthSessionRunner(workspaces *WorkspaceService, repository *auth.Repository, adapters ...harness.Adapter) (*RuntimeAuthSessionRunner, error) {
	if workspaces == nil || workspaces.runtime == nil || repository == nil {
		return nil, errors.New("workspace runtime and authentication repository are required")
	}
	catalog := make(map[harness.Name]harness.Adapter, len(adapters))
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("nil authentication runtime adapter")
		}
		if _, duplicate := catalog[adapter.Name()]; duplicate {
			return nil, fmt.Errorf("duplicate authentication runtime adapter %q", adapter.Name())
		}
		catalog[adapter.Name()] = adapter
	}
	return &RuntimeAuthSessionRunner{
		workspaces: workspaces, repository: repository, adapters: catalog, agentImageReference: buildinfo.AgentImage,
	}, nil
}

func (runner *RuntimeAuthSessionRunner) Login(ctx context.Context, request AuthSessionRequest) (result AuthSessionResult, returnErr error) {
	if ctx == nil {
		return result, model.NewError(model.CodeInvalidInput, "authentication login context is nil", nil)
	}
	if runner.workspaces.approvals == nil {
		return result, model.NewError(model.CodeUnavailable, "authentication login approval authority is unavailable", nil)
	}
	adapter := runner.adapters[request.Agent]
	if adapter == nil || !reflect.DeepEqual(adapter.AuthLayout(), request.Layout) {
		return result, model.NewError(model.CodeUnavailable, "authentication login adapter authority changed", nil)
	}
	if request.Session.Project.Harness != request.Agent || request.Session.Root != request.CredentialRoot || request.Session.ReadOnlyRoot != request.ReadOnlyRoot {
		return result, model.NewError(model.CodeInvalidInput, "authentication login session authority does not match", nil)
	}
	canonicalRoot, err := canonicalProjectRoot(request.ProjectRoot)
	if err != nil {
		return result, err
	}
	execution, err := runner.workspaces.ApprovedProjectPlan(ctx, canonicalRoot)
	if err != nil {
		return result, err
	}
	if execution.Project.ID != request.Session.Project.ID || !agentAllowed(execution.Agents.Allowed, request.Agent) {
		return result, model.NewError(model.CodeUnapproved, "authentication login harness is not approved for the project", nil)
	}
	name, err := runtime.CanonicalAuthLoginName(canonicalRoot, string(request.Agent))
	if err != nil {
		return result, model.NewError(model.CodeInvalidInput, "derive authentication login resource name", err)
	}
	labels, err := runtime.AuthLoginOwnershipLabels(execution.Project.ID, request.Session.SessionID, string(request.Agent))
	if err != nil {
		return result, model.NewError(model.CodeInvalidInput, "derive authentication login ownership", err)
	}
	intent := auth.AuthLoginIntent{
		Version: auth.AuthLoginIntentVersion, Generation: 1,
		Project: request.Session.Project, SessionID: string(request.Session.SessionID), PlanHash: execution.ExecutableHash,
		State: auth.AuthLoginPlanned, VolumeName: name, ContainerName: name,
	}
	if err := runner.repository.CreateAuthLoginIntent(ctx, intent); err != nil {
		return result, model.NewError(model.CodeConflict, "persist authentication login intent", err)
	}

	var volume, container runtime.Resource
	cleanupBase := context.WithoutCancel(ctx)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(cleanupBase, authCleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, runner.cleanup(cleanupCtx, &intent, container, volume))
	}()

	image, err := runner.workspaces.ensureWorkspaceImage(ctx, execution)
	if err != nil {
		return result, model.NewError(model.CodeUnavailable, "ensure approved authentication login image", err)
	}
	intent.State = auth.AuthLoginCreating
	intent.Generation++
	if err := runner.repository.ReplaceAuthLoginIntent(ctx, intent, intent.Generation-1); err != nil {
		return result, err
	}
	volume, err = runner.workspaces.runtime.CreateAuthLoginVolume(ctx, runtime.AuthLoginVolumeSpec{
		Name: name, CanonicalRoot: canonicalRoot, Harness: string(request.Agent), Labels: labels,
	})
	if err != nil {
		return result, model.NewError(model.CodeUnavailable, "create private authentication login volume", err)
	}
	intent.VolumeID = string(volume.ID)
	intent.Generation++
	if err := runner.repository.ReplaceAuthLoginIntent(ctx, intent, intent.Generation-1); err != nil {
		return result, err
	}

	roots := harnessRoots(request.Session.SessionID)
	roots.Workspace = authLoginGuestWorkspace
	helperMount, err := runner.guestHelperMount()
	if err != nil {
		return result, err
	}
	uid, gid, err := guestUserIdentity(standardWorkspaceUser)
	if err != nil {
		return result, err
	}
	spec := runtime.AuthLoginSpec{
		Name: name, CanonicalRoot: canonicalRoot, Harness: string(request.Agent), Image: image,
		Entrypoint: []string{DefaultGuestHelperPath, "serve", "--socket", DefaultGuestSocketPath, "--child-uid", uid, "--child-gid", gid},
		WorkingDir: "/tmp", User: "0:0",
		AuthVolume:  runtime.Mount{Source: volume.Name, Target: roots.Auth, Type: "volume", Authority: runtime.MountAuthorityVolume},
		GuestHelper: helperMount, Labels: labels, CPUs: execution.Limits.CPUs, MemoryBytes: execution.Limits.MemoryBytes,
	}
	container, err = runner.workspaces.runtime.CreateAuthLogin(ctx, spec)
	if err != nil {
		return result, model.NewError(model.CodeUnavailable, "create disposable authentication login runtime", err)
	}
	intent.ContainerID = string(container.ID)
	intent.Generation++
	if err := runner.repository.ReplaceAuthLoginIntent(ctx, intent, intent.Generation-1); err != nil {
		return result, err
	}
	snapshot, err := runner.workspaces.runtime.Inspect(ctx, container.ID)
	if err != nil {
		return result, model.NewError(model.CodeUnavailable, "inspect disposable authentication login runtime", err)
	}
	if err := runner.workspaces.runtime.StartAuthLogin(ctx, snapshot); err != nil {
		return result, model.NewError(model.CodeUnavailable, "start disposable authentication login runtime", err)
	}
	snapshot, err = runner.workspaces.runtime.Inspect(ctx, container.ID)
	if err != nil {
		return result, model.NewError(model.CodeUnavailable, "reinspect disposable authentication login runtime", err)
	}
	intent.State = auth.AuthLoginRunning
	intent.Generation++
	if err := runner.repository.ReplaceAuthLoginIntent(ctx, intent, intent.Generation-1); err != nil {
		return result, err
	}

	agent := &AgentService{workspaces: runner.workspaces, agentImageReference: runner.agentImageReference}
	if err := agent.verifyHarnessBuildAttestation(ctx, snapshot, execution, adapter, func(stdout, stderr io.Writer) (runtime.Exit, error) {
		return agent.shell(ctx, snapshot, []string{"/bin/cat", "--", harness.BuildAttestationPath}, nil, false, nil, stdout, stderr, nil)
	}); err != nil {
		return result, err
	}
	if err := agent.prepareGuestRoots(ctx, snapshot, roots); err != nil {
		return result, err
	}
	if err := agent.mkdirGuest(ctx, snapshot, roots.ReadOnlyConfig); err != nil {
		return result, err
	}
	if err := agent.mkdirGuest(ctx, snapshot, roots.Workspace); err != nil {
		return result, err
	}
	if err := runner.copyCredentialsToGuest(ctx, snapshot, request.CredentialRoot, roots.Auth, request.Layout); err != nil {
		return result, err
	}
	reviewedConfiguration, err := runner.copyReviewedConfigurationToGuest(ctx, snapshot, request.ReadOnlyRoot, roots.ReadOnlyConfig, request.Layout)
	if err != nil {
		return result, err
	}

	artifact := adapter.Version()
	var versionStdout, versionStderr cappedBuffer
	versionStdout.limit = maxHarnessVersionOutput
	versionStderr.limit = maxHarnessVersionOutput
	versionExit, err := agent.shell(ctx, snapshot, []string{artifact.Executable, "--version"}, rootEnvironment(roots, request.Layout), false, nil, &versionStdout, &versionStderr, nil)
	if err != nil {
		return result, err
	}
	if versionExit.Code == nil || *versionExit.Code != 0 || versionExit.Signal != "" {
		return result, model.NewError(model.CodeUnavailable, "authentication harness version command failed", nil)
	}
	if err := adapter.ValidateVersion(versionStdout.String(), versionStderr.String()); err != nil {
		return result, model.NewError(model.CodeUnavailable, "validate authentication harness version", err)
	}
	flow, err := adapter.Login(harness.LoginRequest{Roots: roots, ReadOnlyConfig: reviewedConfiguration})
	if err != nil {
		return result, model.NewError(model.CodeUnavailable, "prepare structured authentication login", err)
	}
	if err := harness.ValidateExecSpec(flow.Exec); err != nil || !flow.Exec.Terminal || flow.Exec.Cwd != roots.Workspace {
		return result, model.NewError(model.CodeInvalidInput, "authentication login flow is invalid", err)
	}
	loginCtx := ctx
	cancel := func() {}
	if flow.CallbackTimeout > 0 {
		loginCtx, cancel = context.WithTimeout(ctx, time.Duration(flow.CallbackTimeout)*time.Second)
	}
	defer cancel()
	process, err := runner.workspaces.runtime.PrepareExec(loginCtx, snapshot, runtime.ExecSpec{
		Argv: flow.Exec.Argv, Env: harnessEnvironment(flow.Exec.Env), WorkingDir: runtime.GuestPath(flow.Exec.Cwd),
		User: standardWorkspaceUser, Terminal: true,
	})
	if err != nil {
		return result, model.NewError(model.CodeUnavailable, "prepare interactive authentication login", err)
	}
	exit, invocationErr := request.RunInteractive(loginCtx, InteractiveChild{
		Argv: append([]string{process.Executable}, process.Args...), Env: process.Env, Dir: process.Dir,
		Stdin: request.Stdin, Stdout: request.Stdout, Stderr: request.Stderr,
	})
	result.Exit = exit
	if invocationErr != nil || exit.Code == nil || *exit.Code != 0 || exit.Signal != "" {
		return result, invocationErr
	}
	if err := runner.copyCredentialsFromGuest(ctx, snapshot, roots.Auth, request.Session, adapter); err != nil {
		return result, err
	}
	return result, nil
}

func (runner *RuntimeAuthSessionRunner) guestHelperMount() (*runtime.Mount, error) {
	if runner.workspaces.guestHelperSource == nil {
		return nil, nil
	}
	source, err := runner.workspaces.guestHelperSource()
	if err != nil {
		return nil, model.NewError(model.CodeUnavailable, "resolve authentication login guest helper", err)
	}
	mount := runtime.Mount{Source: filepath.Dir(string(source)), Target: DefaultGuestHelperDirectory, Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityGuestHelper}
	return &mount, nil
}

func (runner *RuntimeAuthSessionRunner) copyCredentialsToGuest(ctx context.Context, snapshot runtime.ResourceSnapshot, sourceRoot, guestRoot string, layout harness.AuthLayout) error {
	for _, artifact := range layout.CredentialArtifacts {
		source := filepath.Join(sourceRoot, filepath.FromSlash(artifact))
		if err := harness.IsRegularPrivateFile(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return model.NewError(model.CodeUnavailable, "validate private authentication login seed", err)
		}
		info, err := os.Stat(source)
		if err != nil || info.Size() > layout.MaxArtifactBytes {
			return model.NewError(model.CodeUnavailable, "authentication login seed exceeds its bound", err)
		}
		destination := path.Join(guestRoot, artifact)
		if _, err := runner.workspaces.runtime.Exec(ctx, snapshot, runtime.ExecSpec{Argv: []string{"/bin/mkdir", "-p", path.Dir(destination)}, WorkingDir: "/tmp", User: "0:0"}, runtime.ExecIO{}); err != nil {
			return err
		}
		if err := runner.workspaces.runtime.CopyTo(ctx, snapshot, runtime.HostPath(source), runtime.GuestPath(destination)); err != nil {
			return model.NewError(model.CodeUnavailable, "stage authentication login seed", err)
		}
	}
	return nil
}

func (runner *RuntimeAuthSessionRunner) copyReviewedConfigurationToGuest(ctx context.Context, snapshot runtime.ResourceSnapshot, sourceRoot, guestRoot string, layout harness.AuthLayout) ([]string, error) {
	result := make([]string, 0, len(layout.ReadOnlyConfig))
	for _, artifact := range layout.ReadOnlyConfig {
		source := filepath.Join(sourceRoot, filepath.FromSlash(artifact))
		if err := harness.IsRegularPrivateFile(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, model.NewError(model.CodeUnavailable, "validate reviewed authentication configuration", err)
		}
		info, err := os.Stat(source)
		if err != nil || info.Size() > layout.MaxArtifactBytes {
			return nil, model.NewError(model.CodeUnavailable, "reviewed authentication configuration exceeds its bound", err)
		}
		destination := path.Join(guestRoot, artifact)
		if _, err := runner.workspaces.runtime.Exec(ctx, snapshot, runtime.ExecSpec{Argv: []string{"/bin/mkdir", "-p", path.Dir(destination)}, WorkingDir: "/tmp", User: "0:0"}, runtime.ExecIO{}); err != nil {
			return nil, err
		}
		if err := runner.workspaces.runtime.CopyTo(ctx, snapshot, runtime.HostPath(source), runtime.GuestPath(destination)); err != nil {
			return nil, model.NewError(model.CodeUnavailable, "stage reviewed authentication configuration", err)
		}
		result = append(result, destination)
	}
	return result, nil
}

func (runner *RuntimeAuthSessionRunner) copyCredentialsFromGuest(ctx context.Context, snapshot runtime.ResourceSnapshot, guestRoot string, session auth.ProjectSession, adapter harness.Adapter) error {
	layout := adapter.AuthLayout()
	staging, err := os.MkdirTemp(runner.workspaces.tempRoot, ".dsx-auth-login-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return err
	}
	for index, artifact := range layout.CredentialArtifacts {
		guestPath := path.Join(guestRoot, artifact)
		exit, err := runner.workspaces.runtime.Exec(ctx, snapshot, runtime.ExecSpec{Argv: []string{"/usr/bin/test", "-f", guestPath}, WorkingDir: "/tmp", User: standardWorkspaceUser}, runtime.ExecIO{})
		if err != nil {
			return err
		}
		if exit.Code == nil || exit.Signal != "" {
			return model.NewError(model.CodeUnavailable, "inspect refreshed authentication artifact", nil)
		}
		if *exit.Code != 0 {
			if index == 0 {
				return model.NewError(model.CodeUnavailable, "authentication login produced no credential artifact", nil)
			}
			continue
		}
		hostPath := filepath.Join(staging, filepath.FromSlash(artifact))
		if err := os.MkdirAll(filepath.Dir(hostPath), 0o700); err != nil {
			return err
		}
		if err := runner.workspaces.runtime.CopyFrom(ctx, snapshot, runtime.GuestPath(guestPath), runtime.HostPath(hostPath)); err != nil {
			return model.NewError(model.CodeUnavailable, "capture refreshed authentication artifact", err)
		}
		info, err := os.Lstat(hostPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() > layout.MaxArtifactBytes {
			return model.NewError(model.CodeUnavailable, "refreshed authentication artifact is unsafe", err)
		}
		if err := os.Chmod(hostPath, 0o600); err != nil {
			return err
		}
	}
	if err := runner.repository.RefreshProjectSession(ctx, session, staging, adapter); err != nil {
		return model.NewError(model.CodeUnavailable, "validate refreshed authentication artifacts", err)
	}
	return nil
}

func (runner *RuntimeAuthSessionRunner) cleanup(ctx context.Context, intent *auth.AuthLoginIntent, container, volume runtime.Resource) error {
	var result error
	if intent.Generation > 0 {
		expected := intent.Generation
		intent.Generation++
		intent.State = auth.AuthLoginCleaning
		result = errors.Join(result, runner.repository.ReplaceAuthLoginIntent(ctx, *intent, expected))
	}
	runID, runErr := model.ParseRunID(intent.SessionID)
	labels, labelErr := runtime.AuthLoginOwnershipLabels(intent.Project.ID, runID, string(intent.Project.Harness))
	result = errors.Join(result, runErr, labelErr)
	if runErr == nil && labelErr == nil && container.ID != "" {
		if string(container.ID) != intent.ContainerID {
			result = errors.Join(result, model.NewError(model.CodeAmbiguous, "authentication login container identity changed", nil))
		} else if snapshot, err := runner.ownedAuthLoginSnapshot(ctx, container.ID, intent.ContainerName, runtime.ResourceAuthLogin, labels); err == nil {
			if snapshot.State == "running" {
				result = errors.Join(result, runner.workspaces.runtime.Stop(ctx, snapshot, runtime.StopPolicy{TimeoutSeconds: workspaceStopSeconds, Signal: runtime.Signal("SIGTERM")}))
			}
			if current, inspectErr := runner.ownedAuthLoginSnapshot(ctx, container.ID, intent.ContainerName, runtime.ResourceAuthLogin, labels); inspectErr == nil {
				result = errors.Join(result, runner.workspaces.runtime.Delete(ctx, current))
			} else if !errors.Is(inspectErr, runtime.ErrResourceNotFound) {
				result = errors.Join(result, inspectErr)
			}
		} else if !errors.Is(err, runtime.ErrResourceNotFound) {
			result = errors.Join(result, err)
		}
	}
	if runErr == nil && labelErr == nil && volume.ID != "" {
		if string(volume.ID) != intent.VolumeID {
			result = errors.Join(result, model.NewError(model.CodeAmbiguous, "authentication login volume identity changed", nil))
		} else if snapshot, err := runner.ownedAuthLoginSnapshot(ctx, volume.ID, intent.VolumeName, runtime.ResourceVolume, labels); err == nil {
			result = errors.Join(result, runner.workspaces.runtime.Delete(ctx, snapshot))
		} else if !errors.Is(err, runtime.ErrResourceNotFound) {
			result = errors.Join(result, err)
		}
	}
	if result == nil {
		result = runner.repository.DeleteAuthLoginIntent(ctx, intent.Project, intent.SessionID)
	}
	return result
}

func (runner *RuntimeAuthSessionRunner) ownedAuthLoginSnapshot(ctx context.Context, id runtime.ResourceID, name string, kind runtime.ResourceKind, labels []runtime.Label) (runtime.ResourceSnapshot, error) {
	snapshot, err := runner.workspaces.runtime.Inspect(ctx, id)
	if err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	if snapshot.ID != id || snapshot.Name != name || snapshot.Kind != kind || !sameAuthLoginLabels(snapshot.Labels, labels) {
		return runtime.ResourceSnapshot{}, model.NewError(model.CodeAmbiguous, "authentication login resource ownership changed; preserved", nil)
	}
	return snapshot, nil
}

func sameAuthLoginLabels(observed, expected []runtime.Label) bool {
	if len(observed) != len(expected) {
		return false
	}
	values := make(map[string]string, len(observed))
	for _, label := range observed {
		if _, duplicate := values[label.Key]; duplicate {
			return false
		}
		values[label.Key] = label.Value
	}
	for _, label := range expected {
		if values[label.Key] != label.Value {
			return false
		}
	}
	return true
}

func canonicalProjectRoot(root string) (string, error) {
	if root == "" {
		root = "."
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", model.NewError(model.CodeInvalidInput, "resolve authentication project root", err)
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return "", model.NewError(model.CodeInvalidInput, "canonicalize authentication project root", err)
	}
	return canonical, nil
}

func agentAllowed(allowed []string, selected harness.Name) bool {
	for _, candidate := range allowed {
		if candidate == string(selected) {
			return true
		}
	}
	return false
}
