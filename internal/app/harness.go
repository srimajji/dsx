package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	agentimage "github.com/srimajji/dsx/images/agent"
	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/buildinfo"
	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
	"github.com/srimajji/dsx/internal/terminal"
)

const maxHarnessVersionOutput = 64 << 10

type AgentRunRequest struct {
	Root           string
	Workspace      string
	Agent          string
	Browser        bool
	Prompt         string
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
	RunInteractive InteractiveChildRunner
	BeforeExec     func(AgentRunResult) error
}

type AgentRunResult struct {
	Agent         harness.Name
	Version       string
	Exit          runtime.Exit
	AuthPromotion auth.Promotion
}

type AgentService struct {
	workspaces          *WorkspaceService
	auth                *AuthService
	adapters            map[harness.Name]harness.Adapter
	agentImageReference string
	now                 func() time.Time
	newRunID            func(time.Time) (model.RunID, error)
}

func NewAgentService(workspaces *WorkspaceService, authentication *AuthService, adapters ...harness.Adapter) (*AgentService, error) {
	if workspaces == nil || authentication == nil {
		return nil, errors.New("workspace and authentication services are required")
	}
	catalog := make(map[harness.Name]harness.Adapter, len(adapters))
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("nil harness adapter")
		}
		name := adapter.Name()
		if _, err := harness.ParseName(string(name)); err != nil {
			return nil, err
		}
		if _, duplicate := catalog[name]; duplicate {
			return nil, fmt.Errorf("duplicate harness adapter %q", name)
		}
		catalog[name] = adapter
	}
	return &AgentService{
		workspaces: workspaces, auth: authentication, adapters: catalog,
		agentImageReference: buildinfo.AgentImage, now: time.Now, newRunID: model.NewRunID,
	}, nil
}

func (service *AgentService) Run(ctx context.Context, request AgentRunRequest) (result AgentRunResult, returnErr error) {
	if ctx == nil {
		return result, model.NewError(model.CodeInvalidInput, "agent context is nil", nil)
	}
	if service == nil || service.workspaces == nil || service.auth == nil {
		return result, model.NewError(model.CodeUnavailable, "agent service is unavailable", nil)
	}
	workspaceName, err := model.ParseWorkspaceName(request.Workspace)
	if err != nil {
		return result, model.NewError(model.CodeInvalidInput, err.Error(), nil)
	}
	access, unlock, err := service.workspaces.workspaceAccess(ctx, request.Root, workspaceName, true)
	if err != nil {
		return result, err
	}
	locked := true
	releaseAccess := func() error {
		if !locked {
			return nil
		}
		locked = false
		return unlock()
	}
	defer func() { returnErr = errors.Join(returnErr, releaseAccess()) }()
	if access.Manifest.State != model.StateRunning {
		return result, model.NewError(model.CodeConflict, "agent requires a running workspace", nil)
	}
	if access.Manifest.ActiveSession != nil {
		return result, model.NewError(model.CodeConflict, "workspace has an active session", nil)
	}

	name, err := resolveAgent(request.Agent, access.Manifest.DefaultAgent, access.Plan.Agents.Default, access.Plan.Agents.Allowed)
	if err != nil {
		return result, err
	}
	adapter := service.adapters[name]
	if adapter == nil {
		return result, model.NewError(model.CodeUnavailable, fmt.Sprintf("harness %q is not installed", name), nil)
	}
	if request.Browser && access.Plan.Browser == nil {
		return result, model.NewError(model.CodeUnapproved, "browser is not approved for this project", nil)
	}
	invocationID, err := service.newRunID(service.now().UTC())
	if err != nil {
		return result, model.Wrap(model.CodeInternal, "generate agent session ID", err)
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
	workspaceAuth, err := service.auth.AcquireWorkspace(ctx, WorkspaceAuthRequest{
		ProjectID: access.Manifest.ProjectID, Workspace: workspaceName, Agent: name, SessionID: invocationID,
	})
	if err != nil {
		return result, err
	}
	cleanupBase := context.WithoutCancel(ctx)
	authHeld := true
	releaseAuth := func() error {
		if !authHeld {
			return nil
		}
		authHeld = false
		cleanupCtx, cancel := context.WithTimeout(cleanupBase, 30*time.Second)
		defer cancel()
		return service.auth.ReleaseWorkspace(cleanupCtx, workspaceAuth)
	}
	defer func() { returnErr = errors.Join(returnErr, releaseAuth()) }()

	if err := service.prepareGuestRoots(ctx, access.Workspace, roots); err != nil {
		return result, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(cleanupBase, 30*time.Second)
		defer cancel()
		_, cleanupErr := service.shell(cleanupCtx, access.Workspace, []string{"/bin/rm", "-rf", "--", path.Dir(roots.Home)}, nil, false, nil, nil, nil, nil)
		returnErr = errors.Join(returnErr, cleanupErr)
	}()
	if err := service.copyAuthToGuest(ctx, access.Workspace, workspaceAuth.Copy.Root, roots.Auth, workspaceAuth.Layout); err != nil {
		return result, err
	}
	readOnlyConfig, err := service.copyReadOnlyConfigToGuest(ctx, access.Workspace, workspaceAuth.Copy.ReadOnlyRoot, roots.ReadOnlyConfig, workspaceAuth.Layout)
	if err != nil {
		return result, err
	}
	if len(readOnlyConfig) != 0 {
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(cleanupBase, 30*time.Second)
			defer cancel()
			returnErr = errors.Join(returnErr, service.removeReadOnlyGuestRoot(cleanupCtx, access.Workspace, roots.ReadOnlyConfig))
		}()
	}
	if err := service.verifyHarnessBuildAttestation(ctx, access.Workspace, access.Plan, adapter, func(stdout, stderr io.Writer) (runtime.Exit, error) {
		return service.shell(ctx, access.Workspace, []string{"/bin/cat", "--", harness.BuildAttestationPath}, nil, false, nil, stdout, stderr, nil)
	}); err != nil {
		return result, err
	}
	artifact := adapter.Version()
	var versionStdout, versionStderr cappedBuffer
	versionStdout.limit = maxHarnessVersionOutput
	versionStderr.limit = maxHarnessVersionOutput
	versionExit, err := service.shell(ctx, access.Workspace, []string{artifact.Executable, "--version"}, rootEnvironment(roots, workspaceAuth.Layout), false, nil, &versionStdout, &versionStderr, nil)
	if err != nil {
		return result, err
	}
	if versionExit.Code == nil || *versionExit.Code != 0 || versionExit.Signal != "" {
		return result, model.NewError(model.CodeUnavailable, fmt.Sprintf("%s version command failed", name), nil)
	}
	if err := adapter.ValidateVersion(versionStdout.String(), versionStderr.String()); err != nil {
		return result, model.Wrap(model.CodeUnavailable, "validate pinned harness version", err)
	}

	var browser *browserSession
	var servers []harness.MCPServer
	if request.Browser {
		browser, err = service.createBrowserSession(ctx, access)
		if err != nil {
			return result, err
		}
		servers = []harness.MCPServer{browser.Server}
	}
	cleanupBrowser := func() error {
		if browser == nil {
			return nil
		}
		session := browser
		browser = nil
		cleanupCtx, cancel := context.WithTimeout(cleanupBase, browserCleanupTimeout)
		defer cancel()
		if locked {
			index, found := manifestResourceIndex(access.Manifest.Resources, runtime.ResourceBrowser)
			if !found || access.Manifest.Resources[index].Name != session.Record.Name {
				return model.NewError(model.CodeAmbiguous, "browser cleanup ownership changed before agent execution", nil)
			}
			return service.deleteBrowserWithAccess(cleanupCtx, access, index)
		}
		return service.deleteBrowserSession(cleanupCtx, session)
	}
	defer func() { returnErr = errors.Join(returnErr, cleanupBrowser()) }()

	mcpRequest := harness.MCPRequest{Roots: roots, Servers: servers}
	injection, err := adapter.EphemeralMCP(mcpRequest)
	if err != nil {
		return result, err
	}
	if err := service.installGeneratedFiles(ctx, access.Workspace, workspaceAuth.Copy.Root, roots, injection.Files); err != nil {
		return result, err
	}
	if err := service.verifyEffectiveMCP(ctx, access.Workspace, invocationID, adapter, mcpRequest, injection); err != nil {
		return result, err
	}
	interactive := request.Prompt == ""
	spec, err := adapter.Invocation(harness.InvocationRequest{
		Roots: roots, Prompt: request.Prompt, Interactive: interactive, ReadOnlyConfig: readOnlyConfig,
	})
	if err != nil {
		return result, err
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return result, model.Wrap(model.CodeInvalidInput, "validate harness invocation", err)
	}
	if spec.Cwd != roots.Workspace || spec.Terminal != interactive {
		return result, model.NewError(model.CodeInvalidInput, "harness invocation does not match the agent session mode", nil)
	}
	spec.Argv = insertHarnessArgs(spec.Argv, injection.Args)
	for key, value := range injection.Env {
		if spec.Env == nil {
			spec.Env = make(map[string]string)
		}
		spec.Env[key] = value
	}

	preparedExecution, err := service.workspaces.PrepareWorkspaceExecution(ctx, *access.Manifest, runtime.ExecSpec{
		Env: harnessEnvironment(spec.Env),
	})
	if err != nil {
		return result, err
	}
	spec.Env, err = environmentMap(preparedExecution.Env)
	if err != nil {
		return result, model.Wrap(model.CodeInternal, "prepare typed workspace execution environment", err)
	}
	result = AgentRunResult{Agent: name, Version: artifact.Version}
	if request.BeforeExec != nil {
		if err := request.BeforeExec(result); err != nil {
			return result, model.Wrap(model.CodeInternal, "render agent status", err)
		}
	}
	browserResource := ""
	if browser != nil {
		browserResource = browser.Record.ExpectedID
	}
	workspaceRunID := access.Manifest.RunID
	access.Manifest.ActiveSession = &state.SessionRecord{
		SessionID: invocationID, Kind: "agent", Agent: string(name),
		BrowserResource: browserResource, StartedAt: service.now().UTC(),
	}
	if err := service.workspaces.replaceManifest(ctx, access.Manifest); err != nil {
		return result, err
	}
	if err := releaseAccess(); err != nil {
		return result, err
	}
	sessionMarked := true
	defer func() {
		if !sessionMarked {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(cleanupBase, browserCleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, service.workspaces.clearMatchingSession(
			cleanupCtx, request.Root, workspaceName, workspaceRunID, invocationID,
		))
	}()
	exit, invocationErr := service.shellWithSecretEnvironment(
		ctx, access.Workspace, invocationID, spec.Argv, spec.Env, adapter.RedactionRules().EnvironmentKeys,
		interactive, request.Stdin, request.Stdout, request.Stderr, request.RunInteractive,
	)
	postAccess, postUnlock, postErr := service.workspaces.workspaceAccess(cleanupBase, request.Root, workspaceName, false)
	if postErr != nil {
		browserErr := cleanupBrowser()
		result.Exit = exit
		return result, errors.Join(invocationErr, browserErr, postErr)
	}
	if postAccess.Manifest.RunID != workspaceRunID || postAccess.Manifest.ActiveSession == nil || postAccess.Manifest.ActiveSession.SessionID != invocationID {
		unlockErr := postUnlock()
		browserErr := cleanupBrowser()
		result.Exit = exit
		return result, errors.Join(invocationErr, browserErr, unlockErr, model.NewError(model.CodeConflict, "agent session was ended by workspace lifecycle", nil))
	}
	access, unlock, locked = postAccess, postUnlock, true
	browserErr := cleanupBrowser()
	syncCtx, cancelSync := context.WithTimeout(cleanupBase, 30*time.Second)
	defer cancelSync()
	var pullErr error
	var promotion auth.Promotion
	var promotionErr error
	if browserErr == nil {
		pullErr = service.copyAuthFromGuest(syncCtx, access.Workspace, workspaceAuth.Copy, roots.Auth, adapter)
		if pullErr == nil {
			promotion, promotionErr = service.auth.PromoteWorkspace(syncCtx, workspaceAuth)
		}
		access.Manifest.ActiveSession = nil
		promotionErr = errors.Join(promotionErr, service.workspaces.replaceManifest(syncCtx, access.Manifest))
		sessionMarked = false
	} else {
		sessionMarked = false // retain the durable marker so Stop can retry ambiguous browser cleanup.
	}
	result.Exit = exit
	result.AuthPromotion = promotion
	return result, errors.Join(invocationErr, browserErr, pullErr, promotionErr)
}
func (service *AgentService) verifyEffectiveMCP(ctx context.Context, snapshot runtime.ResourceSnapshot, sessionID model.RunID, adapter harness.Adapter, request harness.MCPRequest, injection harness.ConfigInjection) error {
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
	exit, err := service.shellWithSecretEnvironment(ctx, snapshot, sessionID, spec.Argv, spec.Env, adapter.RedactionRules().EnvironmentKeys, false, nil, &stdout, &stderr, nil)
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

func resolveAgent(explicit, workspaceDefault, projectDefault string, allowed []string) (harness.Name, error) {
	selected := explicit
	if selected == "" {
		selected = workspaceDefault
	}
	if selected == "" {
		selected = projectDefault
	}
	name, err := harness.ParseName(selected)
	if err != nil {
		return "", model.NewError(model.CodeInvalidInput, "resolve agent: "+err.Error(), nil)
	}
	approved := false
	for _, candidate := range allowed {
		if candidate == string(name) {
			approved = true
			break
		}
	}
	if !approved {
		return "", model.NewError(model.CodeUnapproved, fmt.Sprintf("agent %q is not in agents.allowed", name), nil)
	}
	return name, nil
}

func harnessRoots(runID model.RunID) harness.RunRoots {
	base := "/tmp/dsx-run/" + string(runID)
	return harness.RunRoots{
		Workspace:      "/workspace",
		Home:           base + "/home",
		Auth:           base + "/auth",
		Config:         base + "/config",
		ReadOnlyConfig: "/tmp/dsx-readonly/" + string(runID),
		Data:           base + "/data",
		Cache:          base + "/cache",
		Temporary:      base + "/tmp",
	}
}

func (service *AgentService) prepareGuestRoots(ctx context.Context, snapshot runtime.ResourceSnapshot, roots harness.RunRoots) error {
	if err := harness.ValidateRoots(roots); err != nil {
		return err
	}
	for _, directory := range []string{roots.Home, roots.Auth, roots.Config, roots.Data, roots.Cache, roots.Temporary} {
		if err := service.mkdirGuest(ctx, snapshot, directory); err != nil {
			return err
		}
	}
	return nil
}

func (service *AgentService) verifyHarnessBuildAttestation(
	ctx context.Context,
	snapshot runtime.ResourceSnapshot,
	execution plan.ExecutionPlan,
	adapter harness.Adapter,
	read func(io.Writer, io.Writer) (runtime.Exit, error),
) error {
	if execution.Image.Standard {
		if execution.Image.Reference != "" ||
			execution.Image.Context != "@dsx/standard" ||
			execution.Image.File != agentimage.BuildFile ||
			execution.Image.InputDigest != agentimage.InputDigest() {
			return model.NewError(model.CodeUnapproved, "approved standard image build authority is inconsistent", nil)
		}
		if _, ok := pinnedImageDigest("local@" + snapshot.ImageDigest); !ok {
			return model.NewError(model.CodeUnavailable, "runtime workspace image digest is malformed", nil)
		}
	} else {
		if service.agentImageReference == "" || service.agentImageReference == "unknown" {
			return model.NewError(model.CodeUnavailable, "standard agent image attestation authority is unavailable", nil)
		}
		if execution.Image.Context != "" || execution.Image.File != "" || execution.Image.Reference != service.agentImageReference {
			return model.NewError(model.CodeUnapproved, "project or custom images cannot satisfy the standard harness artifact contract", nil)
		}
		approvedDigest, ok := pinnedImageDigest(execution.Image.Reference)
		if !ok || execution.Image.InputDigest != approvedDigest {
			return model.NewError(model.CodeUnapproved, "approved agent image reference and digest are inconsistent", nil)
		}
		if snapshot.ImageDigest != "sha256:"+approvedDigest {
			return model.NewError(model.CodeUnavailable, "runtime workspace image digest does not match the approved standard agent image", nil)
		}
	}

	var stdout, stderr cappedBuffer
	stdout.limit = harness.MaxBuildAttestationBytes
	stderr.limit = 4096
	exit, err := read(&stdout, &stderr)
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "read harness build attestation", err)
	}
	if stdout.exceeded || stderr.exceeded {
		return model.NewError(model.CodeUnavailable, "harness build attestation output exceeded its bound", nil)
	}
	if exit.Signal != "" || exit.Code == nil || *exit.Code != 0 || stderr.Len() != 0 {
		return model.NewError(model.CodeUnavailable, "harness build attestation file is unavailable", nil)
	}
	if err := harness.ValidateBuildAttestation(stdout.Bytes(), adapter.Name(), adapter.Version()); err != nil {
		return model.Wrap(model.CodeUnavailable, "verify frozen harness artifact", err)
	}
	return nil
}

func pinnedImageDigest(reference string) (string, bool) {
	const marker = "@sha256:"
	index := strings.LastIndex(reference, marker)
	if index <= 0 || index+len(marker)+64 != len(reference) {
		return "", false
	}
	digest := reference[index+len(marker):]
	for _, character := range digest {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return "", false
		}
	}
	return digest, true
}

func (service *AgentService) shell(ctx context.Context, snapshot runtime.ResourceSnapshot, argv []string, environment map[string]string, terminalMode bool, stdin io.Reader, stdout, stderr io.Writer, runInteractive InteractiveChildRunner) (runtime.Exit, error) {
	return service.shellWithSecretEnvironment(ctx, snapshot, "", argv, environment, nil, terminalMode, stdin, stdout, stderr, runInteractive)
}

func (service *AgentService) shellWithSecretEnvironment(ctx context.Context, snapshot runtime.ResourceSnapshot, sessionID model.RunID, argv []string, environment map[string]string, secretEnvironmentKeys []string, terminalMode bool, stdin io.Reader, stdout, stderr io.Writer, runInteractive InteractiveChildRunner) (exit runtime.Exit, returnErr error) {
	ordinary, secret, err := partitionExecEnvironment(environment, secretEnvironmentKeys)
	if err != nil {
		return exit, model.Wrap(model.CodeInvalidInput, "validate agent environment", err)
	}
	var secretPath runtime.GuestPath
	if len(secret) != 0 {
		if _, err := model.ParseRunID(string(sessionID)); err != nil {
			return exit, model.Wrap(model.CodeInvalidInput, "validate agent session ID", err)
		}
		contents, err := encodeSecretEnvironment(secret)
		if err != nil {
			return exit, model.Wrap(model.CodeInvalidInput, "encode agent secret environment", err)
		}
		secretPath, err = secretEnvironmentGuestPath(sessionID)
		if err != nil {
			return exit, model.Wrap(model.CodeInternal, "allocate agent secret environment path", err)
		}
		stage := runtime.ExecSpec{
			Argv:       []string{DefaultGuestHelperPath, "stage-env", "--path", string(secretPath)},
			WorkingDir: workspaceGuestRoot,
			User:       service.workspaces.user(),
		}
		stageExit, stageErr := service.workspaces.execWorkspace(ctx, snapshot, stage, runtime.ExecIO{Stdin: bytes.NewReader(contents)})
		if stageErr != nil || !successfulGuestCommand(stageExit) {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), secretEnvironmentCleanupTimeout)
			defer cancel()
			cleanupErr := service.cleanupAgentSecretEnvironment(cleanupCtx, snapshot, secretPath)
			if stageErr == nil {
				stageErr = model.NewError(model.CodeUnavailable, "stage guest secret environment failed", nil)
			}
			return exit, errors.Join(stageErr, cleanupErr)
		}
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), secretEnvironmentCleanupTimeout)
			defer cancel()
			returnErr = errors.Join(returnErr, service.cleanupAgentSecretEnvironment(cleanupCtx, snapshot, secretPath))
		}()
	}
	arguments := append([]string{DefaultGuestHelperPath, "exec"}, func() []string {
		if secretPath == "" {
			return []string{"--"}
		}
		return []string{"--env-file", string(secretPath), "--"}
	}()...)
	arguments = append(arguments, argv...)
	spec := runtime.ExecSpec{
		Argv: arguments, Env: harnessEnvironment(ordinary), WorkingDir: workspaceGuestRoot,
		User: service.workspaces.user(), Terminal: terminalMode,
	}
	if !terminalMode {
		return service.workspaces.execWorkspace(ctx, snapshot, spec, runtime.ExecIO{Stdin: stdin, Stdout: stdout, Stderr: stderr})
	}
	if runInteractive == nil {
		return exit, model.NewError(model.CodeInvalidInput, "interactive agent runner is not configured", nil)
	}
	process, err := service.workspaces.prepareWorkspaceExec(ctx, snapshot, spec)
	if err != nil {
		return exit, err
	}
	return runInteractive(ctx, InteractiveChild{
		Argv: append([]string{process.Executable}, process.Args...), Env: append([]string(nil), process.Env...),
		Dir: process.Dir, Stdin: stdin, Stdout: stdout, Stderr: stderr,
	})
}

func (service *AgentService) cleanupAgentSecretEnvironment(ctx context.Context, snapshot runtime.ResourceSnapshot, name runtime.GuestPath) error {
	if name == "" {
		return nil
	}
	spec := runtime.ExecSpec{
		Argv:       []string{DefaultGuestHelperPath, "exec", "--", "/bin/rm", "-rf", "--", string(name)},
		WorkingDir: workspaceGuestRoot, User: service.workspaces.user(),
	}
	exit, err := service.workspaces.execWorkspace(ctx, snapshot, spec, runtime.ExecIO{})
	if err != nil {
		return err
	}
	if !successfulGuestCommand(exit) {
		return model.NewError(model.CodeUnavailable, "remove guest secret environment failed", nil)
	}
	return nil
}

func (service *AgentService) copyAuthToGuest(ctx context.Context, snapshot runtime.ResourceSnapshot, sourceRoot, guestRoot string, layout harness.AuthLayout) error {
	for _, artifact := range layout.CredentialArtifacts {
		source := filepath.Join(sourceRoot, filepath.FromSlash(artifact))
		sourceInfo, err := os.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !sourceInfo.Mode().IsRegular() || sourceInfo.Mode().Perm() != 0o600 || sourceInfo.Size() < 0 || sourceInfo.Size() > layout.MaxArtifactBytes {
			return errors.New("authentication source must be a private bounded regular file")
		}
		file, err := os.Open(source)
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		if statErr != nil || !os.SameFile(sourceInfo, info) || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != sourceInfo.Size() {
			closeErr := file.Close()
			if statErr != nil {
				return errors.Join(statErr, closeErr)
			}
			return errors.Join(errors.New("authentication source changed before staging"), closeErr)
		}
		destination := path.Join(guestRoot, artifact)
		if err := service.mkdirGuest(ctx, snapshot, path.Dir(destination)); err == nil {
			err = service.stageGuestFile(ctx, snapshot, destination, layout.MaxArtifactBytes, file)
		}
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
	}
	return nil
}

func (service *AgentService) copyReadOnlyConfigToGuest(ctx context.Context, snapshot runtime.ResourceSnapshot, sourceRoot, guestRoot string, layout harness.AuthLayout) (result []string, returnErr error) {
	staged := make([]string, 0, len(layout.ReadOnlyConfig))
	rootMayExist := false
	defer func() {
		if returnErr != nil && rootMayExist {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			cleanupErr := service.removeReadOnlyGuestRoot(cleanupCtx, snapshot, guestRoot)
			cancel()
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	for _, artifact := range layout.ReadOnlyConfig {
		source := filepath.Join(sourceRoot, filepath.FromSlash(artifact))
		sourceInfo, err := os.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !sourceInfo.Mode().IsRegular() || sourceInfo.Mode().Perm() != 0o400 || sourceInfo.Size() < 0 || sourceInfo.Size() > layout.MaxArtifactBytes {
			return nil, errors.New("reviewed configuration source must be a bounded owner-readable regular file")
		}
		file, err := os.Open(source)
		if err != nil {
			return nil, err
		}
		info, statErr := file.Stat()
		if statErr != nil || !os.SameFile(sourceInfo, info) || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 || info.Size() != sourceInfo.Size() {
			closeErr := file.Close()
			if statErr != nil {
				return nil, errors.Join(statErr, closeErr)
			}
			return nil, errors.Join(errors.New("reviewed configuration source changed before staging"), closeErr)
		}
		destination := path.Join(guestRoot, artifact)
		rootMayExist = true
		err = service.stageReadOnlyGuestFile(ctx, snapshot, destination, layout.MaxArtifactBytes, file)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		staged = append(staged, destination)
	}
	return staged, nil
}

func (service *AgentService) copyAuthFromGuest(ctx context.Context, snapshot runtime.ResourceSnapshot, copy auth.WorkspaceCopy, guestRoot string, adapter harness.Adapter) error {
	layout := adapter.AuthLayout()
	staging, err := os.MkdirTemp(filepath.Dir(copy.Root), ".guest-auth-pull-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return err
	}
	for _, artifact := range layout.CredentialArtifacts {
		guestPath := path.Join(guestRoot, artifact)
		staged := filepath.Join(staging, filepath.FromSlash(artifact))
		if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
			return err
		}
		if _, err := service.exportGuestFile(ctx, snapshot, guestPath, staged, "auth", layout.MaxArtifactBytes); err != nil {
			return model.Wrap(model.CodeUnavailable, "export workspace authentication", err)
		}
	}
	if err := adapter.Seed(ctx, harness.SeedRequest{
		SourceRoot: staging, DestinationRoot: copy.Root,
		Artifacts: append([]string(nil), layout.CredentialArtifacts...), MaxArtifactBytes: layout.MaxArtifactBytes,
	}); err != nil {
		return model.Wrap(model.CodeUnavailable, "validate workspace authentication refresh", err)
	}
	return nil
}

var errGuestExportMissing = errors.New("guest export file is absent")

func (service *AgentService) exportGuestFile(ctx context.Context, snapshot runtime.ResourceSnapshot, guestPath, hostPath, kind string, maximumBytes int64) (bool, error) {
	if maximumBytes < 1 {
		return false, errors.New("guest export size limit is invalid")
	}
	options := runtime.BoundedFileOptions{MaximumBytes: maximumBytes, RejectNUL: kind == "auth", Mode: 0o600}
	err := runtime.ReceiveBoundedFile(runtime.HostPath(hostPath), options, func(output io.Writer) error {
		spec := runtime.ExecSpec{
			Argv: []string{
				DefaultGuestHelperPath, "export-file", "--kind", kind,
				"--max-bytes", strconv.FormatInt(maximumBytes, 10), "--path", guestPath,
			},
			WorkingDir: "/workspace",
			User:       service.workspaces.user(),
		}
		exit, err := service.workspaces.execWorkspace(ctx, snapshot, spec, runtime.ExecIO{Stdout: output, Stderr: io.Discard})
		if err != nil {
			return err
		}
		if exit.Signal == "" && exit.Code != nil && *exit.Code == 4 {
			return errGuestExportMissing
		}
		if !successfulGuestCommand(exit) {
			return errors.New("guest file export command failed")
		}
		return nil
	})
	if errors.Is(err, errGuestExportMissing) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (service *AgentService) produceGuestFile(ctx context.Context, snapshot runtime.ResourceSnapshot, guestPath, workingDirectory string, maximumBytes int64, command guestproto.CommandSpec) error {
	if maximumBytes < 1 {
		return errors.New("guest production size limit is invalid")
	}
	if err := command.Validate(); err != nil || command.Cwd != workingDirectory {
		return model.NewError(model.CodeInvalidInput, "unsafe guest producer command", err)
	}
	arguments := []string{
		DefaultGuestHelperPath, "produce-file",
		"--max-bytes", strconv.FormatInt(maximumBytes, 10),
		"--path", guestPath,
		"--cwd", workingDirectory,
		"--",
	}
	arguments = append(arguments, command.Argv...)
	var stderr cappedBuffer
	stderr.limit = 4096
	spec := runtime.ExecSpec{
		Argv:       arguments,
		Env:        append([]string(nil), command.Env...),
		WorkingDir: "/workspace",
		User:       service.workspaces.user(),
	}
	exit, err := service.workspaces.execWorkspace(ctx, snapshot, spec, runtime.ExecIO{Stderr: &stderr})
	if err != nil {
		return err
	}
	if !successfulGuestCommand(exit) {
		detail := terminal.SanitizeLine(strings.TrimSpace(stderr.String()))
		if detail == "" {
			detail = "no diagnostic"
		}
		return model.NewError(model.CodeUnavailable, "guest file producer failed: "+detail, nil)
	}
	return nil
}

func (service *AgentService) removeGuestExportFile(ctx context.Context, snapshot runtime.ResourceSnapshot, guestPath string) error {
	spec := runtime.ExecSpec{
		Argv:       []string{DefaultGuestHelperPath, "remove-export-file", "--path", guestPath},
		WorkingDir: "/workspace",
		User:       service.workspaces.user(),
	}
	exit, err := service.workspaces.execWorkspace(ctx, snapshot, spec, runtime.ExecIO{Stderr: io.Discard})
	if err != nil {
		return err
	}
	if !successfulGuestCommand(exit) {
		return errors.New("remove guest export file failed")
	}
	return nil
}

func (service *AgentService) installGeneratedFiles(ctx context.Context, snapshot runtime.ResourceSnapshot, stagingRoot string, roots harness.RunRoots, files []harness.GeneratedFile) error {
	for _, generated := range files {
		if generated.Mode != 0o600 || len(generated.Data) > 1<<20 || !allowedGeneratedPath(generated.Path, roots) {
			return model.NewError(model.CodeInvalidInput, "unsafe generated harness configuration", nil)
		}
		if err := service.mkdirGuest(ctx, snapshot, path.Dir(generated.Path)); err != nil {
			return err
		}
		if err := service.stageGuestFile(ctx, snapshot, generated.Path, 1<<20, bytes.NewReader(generated.Data)); err != nil {
			return err
		}
	}
	return nil
}

func (service *AgentService) mkdirGuest(ctx context.Context, snapshot runtime.ResourceSnapshot, directory string) error {
	if _, err := guestDirectoryChain(directory); err != nil {
		return err
	}
	spec := runtime.ExecSpec{
		Argv:       []string{DefaultGuestHelperPath, "ensure-dir", "--path", directory},
		WorkingDir: "/workspace",
		User:       service.workspaces.user(),
	}
	exit, err := service.workspaces.execWorkspace(ctx, snapshot, spec, runtime.ExecIO{})
	if err != nil {
		return err
	}
	if !successfulGuestCommand(exit) {
		return model.NewError(model.CodeUnavailable, "create or verify guest harness directory failed", nil)
	}
	return nil
}

func (service *AgentService) stageGuestFile(ctx context.Context, snapshot runtime.ResourceSnapshot, destination string, maximumBytes int64, input io.Reader) error {
	if input == nil || maximumBytes < 1 || !allowedGuestStagingPath(destination) {
		return model.NewError(model.CodeInvalidInput, "unsafe guest auth/config staging path", nil)
	}
	spec := runtime.ExecSpec{
		Argv:       []string{DefaultGuestHelperPath, "stage-file", "--max-bytes", strconv.FormatInt(maximumBytes, 10), "--path", destination},
		WorkingDir: "/workspace",
		User:       service.workspaces.user(),
	}
	exit, err := service.workspaces.execWorkspace(ctx, snapshot, spec, runtime.ExecIO{Stdin: input})
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "stage private auth/config file", err)
	}
	if !successfulGuestCommand(exit) {
		return model.NewError(model.CodeUnavailable, "stage private auth/config file failed", nil)
	}
	return nil
}

func (service *AgentService) stageReadOnlyGuestFile(ctx context.Context, snapshot runtime.ResourceSnapshot, destination string, maximumBytes int64, input io.Reader) error {
	if input == nil || maximumBytes < 1 || !allowedReadOnlyGuestStagingPath(destination) {
		return model.NewError(model.CodeInvalidInput, "unsafe read-only guest config staging path", nil)
	}
	uid, gid, found := strings.Cut(service.workspaces.user(), ":")
	numericUID, uidErr := strconv.ParseUint(uid, 10, 32)
	numericGID, gidErr := strconv.ParseUint(gid, 10, 32)
	if !found || uidErr != nil || gidErr != nil || numericUID == 0 || numericGID == 0 {
		return model.NewError(model.CodeInvalidInput, "read-only config staging requires a non-root numeric child UID:GID", errors.Join(uidErr, gidErr))
	}
	spec := runtime.ExecSpec{
		Argv: []string{
			DefaultGuestHelperPath, "stage-file", "--read-only",
			"--child-uid", uid, "--child-gid", gid, "--max-bytes", strconv.FormatInt(maximumBytes, 10), "--path", destination,
		},
		WorkingDir: "/workspace",
		User:       "0:0",
	}
	exit, err := service.workspaces.execWorkspace(ctx, snapshot, spec, runtime.ExecIO{Stdin: input})
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "stage root-owned read-only config file", err)
	}
	if !successfulGuestCommand(exit) {
		return model.NewError(model.CodeUnavailable, "stage root-owned read-only config file failed", nil)
	}
	return nil
}

func (service *AgentService) removeReadOnlyGuestRoot(ctx context.Context, snapshot runtime.ResourceSnapshot, root string) error {
	if !allowedReadOnlyGuestRoot(root) {
		return model.NewError(model.CodeInvalidInput, "unsafe read-only guest cleanup root", nil)
	}
	spec := runtime.ExecSpec{
		Argv:       []string{DefaultGuestHelperPath, "remove-read-only", "--path", root},
		WorkingDir: "/workspace",
		User:       "0:0",
	}
	exit, err := service.workspaces.execWorkspace(ctx, snapshot, spec, runtime.ExecIO{})
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "remove root-owned read-only config", err)
	}
	if !successfulGuestCommand(exit) {
		return model.NewError(model.CodeUnavailable, "remove root-owned read-only config failed", nil)
	}
	return nil
}

const guestHarnessRoot = "/tmp/dsx-run"

func allowedGuestStagingPath(name string) bool {
	clean := path.Clean(name)
	if clean != name {
		return false
	}
	parts := strings.Split(clean, "/")
	if len(parts) < 6 || parts[0] != "" || parts[1] != "tmp" || parts[2] != "dsx-run" ||
		(parts[4] != "auth" && parts[4] != "config" && parts[4] != "tmp") {
		return false
	}
	runID, err := model.ParseRunID(parts[3])
	return err == nil && string(runID) == parts[3]
}

func allowedReadOnlyGuestStagingPath(name string) bool {
	clean := path.Clean(name)
	if clean != name {
		return false
	}
	parts := strings.Split(clean, "/")
	if len(parts) < 5 || parts[0] != "" || parts[1] != "tmp" || parts[2] != "dsx-readonly" {
		return false
	}
	runID, err := model.ParseRunID(parts[3])
	return err == nil && string(runID) == parts[3]
}

func allowedReadOnlyGuestRoot(name string) bool {
	clean := path.Clean(name)
	if clean != name {
		return false
	}
	parts := strings.Split(clean, "/")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "tmp" || parts[2] != "dsx-readonly" {
		return false
	}
	runID, err := model.ParseRunID(parts[3])
	return err == nil && string(runID) == parts[3]
}

func guestDirectoryChain(directory string) ([]string, error) {
	clean := path.Clean(directory)
	if directory != clean || clean != guestHarnessRoot && !strings.HasPrefix(clean, guestHarnessRoot+"/") {
		return nil, model.NewError(model.CodeInvalidInput, "unsafe guest harness directory", nil)
	}
	chain := []string{guestHarnessRoot}
	relative := strings.TrimPrefix(clean, guestHarnessRoot)
	current := guestHarnessRoot
	for _, component := range strings.Split(strings.TrimPrefix(relative, "/"), "/") {
		if component == "" {
			continue
		}
		current = path.Join(current, component)
		chain = append(chain, current)
	}
	return chain, nil
}

func guestUserIdentity(user string) (string, string, error) {
	uid, gid, found := strings.Cut(user, ":")
	if !found || uid == "" || gid == "" {
		return "", "", model.NewError(model.CodeInvalidInput, "guest user must be numeric UID:GID", nil)
	}
	numericUID, uidErr := strconv.ParseUint(uid, 10, 32)
	numericGID, gidErr := strconv.ParseUint(gid, 10, 32)
	if uidErr != nil || gidErr != nil || numericUID == 0 || numericGID == 0 {
		return "", "", model.NewError(model.CodeInvalidInput, "guest user must be non-root numeric UID:GID", errors.Join(uidErr, gidErr))
	}
	return uid, gid, nil
}

func successfulGuestCommand(exit runtime.Exit) bool {
	return exit.Code != nil && *exit.Code == 0 && exit.Signal == ""
}

func rootEnvironment(roots harness.RunRoots, layout harness.AuthLayout) map[string]string {
	environment := map[string]string{"HOME": roots.Home, "XDG_CONFIG_HOME": roots.Config, "XDG_DATA_HOME": roots.Data, "XDG_CACHE_HOME": roots.Cache, "TMPDIR": roots.Temporary}
	for key, relative := range layout.Environment {
		environment[key] = path.Join(roots.Auth, relative)
	}
	return environment
}

func harnessEnvironment(environment map[string]string) []string {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+environment[key])
	}
	return result
}

func environmentMap(environment []string) (map[string]string, error) {
	if len(environment) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(environment))
	for _, assignment := range environment {
		key, value, found := strings.Cut(assignment, "=")
		if !found || !validExecEnvironmentName(key) {
			return nil, errors.New("invalid workspace execution environment")
		}
		if _, duplicate := result[key]; duplicate {
			return nil, errors.New("duplicate workspace execution environment")
		}
		result[key] = value
	}
	return result, nil
}

func cloneEnvironment(environment map[string]string) map[string]string {
	if environment == nil {
		return nil
	}
	clone := make(map[string]string, len(environment))
	for key, value := range environment {
		clone[key] = value
	}
	return clone
}

func insertHarnessArgs(argv, injected []string) []string {
	if len(argv) == 0 || len(injected) == 0 {
		return append([]string(nil), argv...)
	}
	result := make([]string, 0, len(argv)+len(injected))
	result = append(result, argv[0])
	result = append(result, injected...)
	return append(result, argv[1:]...)
}

func allowedGeneratedPath(candidate string, roots harness.RunRoots) bool {
	if !path.IsAbs(candidate) || path.Clean(candidate) != candidate {
		return false
	}
	for _, root := range []string{roots.Auth, roots.Config, roots.Temporary} {
		if candidate == root || strings.HasPrefix(candidate, root+"/") {
			return candidate != root
		}
	}
	return false
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *cappedBuffer) Write(data []byte) (int, error) {
	accepted := len(data)
	remaining := buffer.limit - buffer.Len()
	if len(data) > remaining {
		buffer.exceeded = true
	}
	if remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		_, _ = buffer.Buffer.Write(data[:remaining])
	}
	return accepted, nil
}
