package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

const authCleanupTimeout = 30 * time.Second

type AuthStatusRequest struct {
	Root string
}

type AuthAgentStatus struct {
	Agent      harness.Name        `json:"agent"`
	Configured bool                `json:"configured"`
	HostImport auth.HostImportState `json:"host_import"`
}

type AuthStatusResult struct {
	ProjectID model.ProjectID   `json:"project_id"`
	Agents    []AuthAgentStatus `json:"agents"`
}

type AuthImportRequest struct {
	Root     string
	Agent    string
	Approved bool
}

type AuthImportResult struct {
	Agent  harness.Name `json:"agent"`
	Digest string       `json:"digest"`
}

type AuthLoginRequest struct {
	Root           string
	Agent          string
	Approved       bool
	Interactive    bool
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
	RunInteractive InteractiveChildRunner
}

type AuthLoginResult struct {
	Agent     harness.Name   `json:"agent"`
	Exit      runtime.Exit   `json:"exit"`
	Promotion auth.Promotion `json:"promotion"`
}

type AuthRefreshRequest struct {
	Root     string
	Agent    string
	Approved bool
}

type AuthPurgeRequest struct {
	Root     string
	Agent    string
	Approved bool
}

// AuthSessionRequest gives the runtime-side login orchestrator one private
// project credential copy. The runner must never mount the host home or another
// harness's credential root and must copy refreshed artifacts back before return.
type AuthSessionRequest struct {
	ProjectRoot     string
	Agent           harness.Name
	Session         auth.ProjectSession
	CredentialRoot  string
	ReadOnlyRoot    string
	Layout          harness.AuthLayout
	Interactive     bool
	Stdin           io.Reader
	Stdout          io.Writer
	Stderr          io.Writer
	RunInteractive  InteractiveChildRunner
}

type AuthSessionResult struct {
	Exit runtime.Exit
}

type AuthSessionRunner interface {
	Login(context.Context, AuthSessionRequest) (AuthSessionResult, error)
}

type WorkspaceAuthRequest struct {
	ProjectID model.ProjectID
	Workspace model.WorkspaceName
	Agent     harness.Name
	SessionID model.RunID
}

type WorkspaceAuth struct {
	Copy   auth.WorkspaceCopy
	Layout harness.AuthLayout
}

// AuthService manages only canonical project credentials and isolated copies.
type AuthService struct {
	repository *auth.Repository
	discovery  *auth.HostDiscovery
	runner     AuthSessionRunner
	adapters   map[harness.Name]harness.Adapter
	now        func() time.Time
}

func NewAuthService(repository *auth.Repository, discovery *auth.HostDiscovery, runner AuthSessionRunner, adapters ...harness.Adapter) (*AuthService, error) {
	if repository == nil || discovery == nil {
		return nil, errors.New("authentication repository and host discovery are required")
	}
	catalog := make(map[harness.Name]harness.Adapter, len(adapters))
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("nil authentication harness adapter")
		}
		name := adapter.Name()
		if _, err := harness.ParseName(string(name)); err != nil {
			return nil, err
		}
		if _, duplicate := catalog[name]; duplicate {
			return nil, fmt.Errorf("duplicate authentication harness adapter %q", name)
		}
		if err := harness.ValidateAuthLayout(adapter.AuthLayout()); err != nil {
			return nil, fmt.Errorf("invalid %s authentication layout: %w", name, err)
		}
		catalog[name] = adapter
	}
	return &AuthService{repository: repository, discovery: discovery, runner: runner, adapters: catalog, now: time.Now}, nil
}

func (service *AuthService) Status(ctx context.Context, request AuthStatusRequest) (AuthStatusResult, error) {
	projectID, err := projectIDForRoot(request.Root)
	if err != nil {
		return AuthStatusResult{}, err
	}
	names := make([]harness.Name, 0, len(service.adapters))
	for name := range service.adapters {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	result := AuthStatusResult{ProjectID: projectID, Agents: make([]AuthAgentStatus, 0, len(names))}
	for _, name := range names {
		adapter := service.adapters[name]
		status, statusErr := service.repository.ProjectStatus(ctx, auth.Project{ID: projectID, Harness: name}, adapter)
		if statusErr != nil {
			return AuthStatusResult{}, model.NewError(model.CodeUnavailable, "read project authentication status", statusErr)
		}
		result.Agents = append(result.Agents, AuthAgentStatus{
			Agent: name, Configured: status.Configured,
			HostImport: service.discovery.Status(ctx, name, adapter.AuthLayout().MaxArtifactBytes),
		})
	}
	return result, nil
}

func (service *AuthService) Import(ctx context.Context, request AuthImportRequest) (AuthImportResult, error) {
	if !request.Approved {
		return AuthImportResult{}, model.NewError(model.CodeUnapproved, "authentication import requires explicit approval", nil)
	}
	return service.importHost(ctx, request.Root, request.Agent, false)
}

func (service *AuthService) Refresh(ctx context.Context, request AuthRefreshRequest) (AuthImportResult, error) {
	if !request.Approved {
		return AuthImportResult{}, model.NewError(model.CodeUnapproved, "authentication refresh requires explicit approval", nil)
	}
	return service.importHost(ctx, request.Root, request.Agent, true)
}

func (service *AuthService) importHost(ctx context.Context, root, agent string, replace bool) (AuthImportResult, error) {
	name, adapter, err := service.adapter(agent)
	if err != nil {
		return AuthImportResult{}, err
	}
	if name == harness.Claude {
		return AuthImportResult{}, model.NewError(model.CodeInvalidInput, "Claude host authentication is not portable; use dsx auth login --agent claude", nil)
	}
	projectID, err := projectIDForRoot(root)
	if err != nil {
		return AuthImportResult{}, err
	}
	source, err := service.discovery.Discover(name)
	if err != nil {
		return AuthImportResult{}, model.NewError(model.CodeUnavailable, "supported host credentials are unavailable", err)
	}
	digest, err := service.repository.ImportHost(ctx, auth.Project{ID: projectID, Harness: name}, source, replace, adapter)
	if err != nil {
		return AuthImportResult{}, model.NewError(model.CodeUnavailable, "import supported host credentials", err)
	}
	return AuthImportResult{Agent: name, Digest: digest}, nil
}

func (service *AuthService) Login(ctx context.Context, request AuthLoginRequest) (result AuthLoginResult, returnErr error) {
	if !request.Approved {
		return result, model.NewError(model.CodeUnapproved, "authentication login requires explicit approval", nil)
	}
	if !request.Interactive || request.Stdin == nil || request.Stdout == nil || request.Stderr == nil || request.RunInteractive == nil {
		return result, model.NewError(model.CodeInvalidInput, "authentication login requires an interactive terminal", nil)
	}
	name, adapter, err := service.adapter(request.Agent)
	if err != nil {
		return result, err
	}
	if name != harness.Claude {
		return result, model.NewError(model.CodeInvalidInput, "host-portable harness credentials use dsx auth import or refresh", nil)
	}
	if service.runner == nil {
		return result, model.NewError(model.CodeUnavailable, "DSX authentication login is unavailable", nil)
	}
	projectID, err := projectIDForRoot(request.Root)
	if err != nil {
		return result, err
	}
	sessionID, err := model.NewRunID(service.now().UTC())
	if err != nil {
		return result, model.NewError(model.CodeInternal, "generate authentication session ID", err)
	}
	session, err := service.repository.AcquireProjectSession(ctx, auth.Project{ID: projectID, Harness: name}, sessionID, true, adapter)
	if err != nil {
		return result, model.NewError(model.CodeUnavailable, "prepare DSX authentication login", err)
	}
	cleanupBase := context.WithoutCancel(ctx)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(cleanupBase, authCleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, service.repository.ReleaseProjectSession(cleanupCtx, session))
	}()

	runResult, err := service.runner.Login(ctx, AuthSessionRequest{
		ProjectRoot: request.Root, Agent: name, Session: session, CredentialRoot: session.Root, ReadOnlyRoot: session.ReadOnlyRoot,
		Layout: adapter.AuthLayout(), Interactive: request.Interactive, Stdin: request.Stdin, Stdout: request.Stdout,
		Stderr: request.Stderr, RunInteractive: request.RunInteractive,
	})
	result = AuthLoginResult{Agent: name, Exit: runResult.Exit}
	if err != nil || runResult.Exit.Code == nil || *runResult.Exit.Code != 0 || runResult.Exit.Signal != "" {
		return result, err
	}
	promotion, err := service.repository.PromoteProjectSession(ctx, session, adapter)
	result.Promotion = promotion
	if err != nil {
		return result, model.NewError(model.CodeUnavailable, "persist DSX authentication login", err)
	}
	if promotion.Conflict {
		return result, model.NewError(model.CodeConflict, "authentication changed concurrently; refreshed candidate was preserved", nil)
	}
	return result, nil
}

func (service *AuthService) Purge(ctx context.Context, request AuthPurgeRequest) error {
	if !request.Approved {
		return model.NewError(model.CodeUnapproved, "authentication purge requires explicit confirmation", nil)
	}
	name, _, err := service.adapter(request.Agent)
	if err != nil {
		return err
	}
	projectID, err := projectIDForRoot(request.Root)
	if err != nil {
		return err
	}
	if err := service.repository.PurgeProject(ctx, auth.Project{ID: projectID, Harness: name}); err != nil {
		if errors.Is(err, auth.ErrActiveCopies) {
			return model.NewError(model.CodeConflict, "active authentication copies must be stopped before purge", err)
		}
		return model.NewError(model.CodeUnavailable, "purge project authentication", err)
	}
	return nil
}

func (service *AuthService) AcquireWorkspace(ctx context.Context, request WorkspaceAuthRequest) (WorkspaceAuth, error) {
	adapter := service.adapters[request.Agent]
	if adapter == nil {
		return WorkspaceAuth{}, model.NewError(model.CodeUnavailable, "selected harness is not installed", nil)
	}
	copy, err := service.repository.AcquireWorkspace(ctx, auth.Workspace{
		ProjectID: request.ProjectID, Name: request.Workspace, Harness: request.Agent,
	}, request.SessionID, adapter)
	if err != nil {
		return WorkspaceAuth{}, model.NewError(model.CodeUnavailable, "prepare workspace authentication", err)
	}
	return WorkspaceAuth{Copy: copy, Layout: adapter.AuthLayout()}, nil
}

func (service *AuthService) PromoteWorkspace(ctx context.Context, workspace WorkspaceAuth) (auth.Promotion, error) {
	adapter := service.adapters[workspace.Copy.Workspace.Harness]
	if adapter == nil {
		return auth.Promotion{}, model.NewError(model.CodeUnavailable, "selected harness is not installed", nil)
	}
	promotion, err := service.repository.PromoteWorkspace(ctx, workspace.Copy, adapter)
	if err != nil {
		return promotion, model.NewError(model.CodeUnavailable, "promote workspace authentication", err)
	}
	if promotion.Conflict {
		return promotion, model.NewError(model.CodeConflict, "authentication changed concurrently; refreshed candidate was preserved", nil)
	}
	return promotion, nil
}

func (service *AuthService) ReleaseWorkspace(ctx context.Context, workspace WorkspaceAuth) error {
	if err := service.repository.ReleaseWorkspace(ctx, workspace.Copy); err != nil {
		return model.NewError(model.CodeUnavailable, "release workspace authentication", err)
	}
	return nil
}

func (service *AuthService) adapter(value string) (harness.Name, harness.Adapter, error) {
	name, err := harness.ParseName(value)
	if err != nil {
		return "", nil, model.NewError(model.CodeInvalidInput, err.Error(), nil)
	}
	adapter := service.adapters[name]
	if adapter == nil {
		return "", nil, model.NewError(model.CodeUnavailable, fmt.Sprintf("harness %q is not installed", name), nil)
	}
	return name, adapter, nil
}
