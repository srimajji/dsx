package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/harness/codex"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

type agentSessionRuntime struct {
	*browserSessionRuntime
	workspaceCreates  int
	invocations       int
	commands          []string
	stopHook          func()
	invocationBlock   chan struct{}
	invocationEntered chan struct{}
	stopOnce          sync.Once
}

func (adapter *agentSessionRuntime) CreateWorkspace(context.Context, runtime.WorkspaceSpec) (runtime.Resource, error) {
	adapter.workspaceCreates++
	return runtime.Resource{}, errors.New("agent invocation attempted workspace creation")
}

func (adapter *agentSessionRuntime) PrepareExec(context.Context, runtime.ResourceSnapshot, runtime.ExecSpec) (runtime.ProcessSpec, error) {
	return runtime.ProcessSpec{Executable: "/bin/zsh", Args: []string{"-il"}}, nil
}

func (adapter *agentSessionRuntime) Exec(ctx context.Context, _ runtime.ResourceSnapshot, spec runtime.ExecSpec, streams runtime.ExecIO) (runtime.Exit, error) {
	command := strings.Join(spec.Argv, "\x00")
	adapter.commands = append(adapter.commands, command)
	switch {
	case strings.Contains(command, harnessAttestationPathForTest):
		data, err := os.ReadFile(filepath.Join("..", "..", "images", "agent", "harnesses.lock.json"))
		if err != nil {
			return runtime.Exit{}, err
		}
		if streams.Stdout != nil {
			_, _ = streams.Stdout.Write(data)
		}
	case strings.Contains(command, "\x00codex\x00--version"):
		if streams.Stdout != nil {
			_, _ = io.WriteString(streams.Stdout, "codex-cli 0.147.0\n")
		}
	case strings.Contains(command, "\x00export-file\x00--kind\x00auth\x00"):
		if streams.Stdout != nil {
			_, _ = io.WriteString(streams.Stdout, `{"auth_mode":"apikey","OPENAI_API_KEY":"refreshed"}`)
		}
	case strings.Contains(command, "one task"):
		adapter.invocations++
		if adapter.invocationEntered != nil {
			close(adapter.invocationEntered)
		}
		if adapter.invocationBlock != nil {
			select {
			case <-adapter.invocationBlock:
			case <-ctx.Done():
				return runtime.Exit{}, ctx.Err()
			}
		}
	}
	code := 0
	return runtime.Exit{Code: &code}, nil
}

func (adapter *agentSessionRuntime) Stop(ctx context.Context, snapshot runtime.ResourceSnapshot, policy runtime.StopPolicy) error {
	if snapshot.Kind == runtime.ResourceWorkspace && adapter.invocationBlock != nil {
		adapter.stopOnce.Do(func() { close(adapter.invocationBlock) })
	}
	if snapshot.Kind == runtime.ResourceWorkspace && adapter.stopHook != nil {
		adapter.stopHook()
	}
	return adapter.browserSessionRuntime.Stop(ctx, snapshot, policy)
}

func TestAgentRepeatedSessionsReuseWorkspaceAndDefaultToNoBrowser(t *testing.T) {
	service, access, workspaceRuntime := agentSessionServiceFixture(t)
	for range 2 {
		result, runErr := service.Run(context.Background(), AgentRunRequest{
			Root: access.Manifest.CanonicalRoot, Workspace: string(access.Manifest.Workspace), Prompt: "one task",
		})
		if runErr != nil {
			t.Fatal(runErr)
		}
		if result.Exit.Code == nil || *result.Exit.Code != 0 || result.Agent != "codex" {
			t.Fatalf("agent result = %#v", result)
		}
	}
	if workspaceRuntime.workspaceCreates != 0 {
		t.Fatalf("repeated agent sessions created %d workspaces", workspaceRuntime.workspaceCreates)
	}
	if workspaceRuntime.invocations != 2 {
		t.Fatalf("agent invocations = %d, want 2 against the existing workspace; calls=%q", workspaceRuntime.invocations, workspaceRuntime.commands)
	}
	if workspaceRuntime.creates != 0 || workspaceRuntime.deletes != 0 {
		t.Fatalf("default agent sessions created browser resources: creates=%d deletes=%d", workspaceRuntime.creates, workspaceRuntime.deletes)
	}
}

func TestAgentBlockedExecutionAllowsLifecycleAndRejectsMutations(t *testing.T) {
	service, access, workspaceRuntime := agentSessionServiceFixture(t)
	workspaceRuntime.invocationBlock = make(chan struct{})
	workspaceRuntime.invocationEntered = make(chan struct{})
	runDone := make(chan error, 1)
	go func() {
		_, err := service.Run(context.Background(), AgentRunRequest{
			Root: access.Manifest.CanonicalRoot, Workspace: string(access.Manifest.Workspace), Prompt: "one task",
		})
		runDone <- err
	}()
	<-workspaceRuntime.invocationEntered
	if _, err := service.workspaces.Update(context.Background(), WorkspaceUpdateRequest{
		Root: access.Manifest.CanonicalRoot, Workspace: access.Manifest.Workspace,
	}); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("Update() error = %v, want active-session conflict", err)
	}
	if _, err := service.workspaces.Remove(context.Background(), WorkspaceRemoveRequest{
		Root: access.Manifest.CanonicalRoot, Workspace: access.Manifest.Workspace, Confirmed: true,
	}); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("Remove() error = %v, want active-session conflict", err)
	}
	if _, err := service.workspaces.Stop(context.Background(), WorkspaceStopRequest{
		Root: access.Manifest.CanonicalRoot, Workspace: access.Manifest.Workspace,
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("Run() after Stop error = %v, want lifecycle conflict", err)
	}
	manifest, err := service.workspaces.oneWorkspaceManifest(context.Background(), access.Manifest.ProjectID, access.Manifest.Workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ActiveSession != nil {
		t.Fatalf("Stop left active session %#v", manifest.ActiveSession)
	}
}

func TestAgentCancellationClearsMatchingSession(t *testing.T) {
	service, access, workspaceRuntime := agentSessionServiceFixture(t)
	workspaceRuntime.invocationBlock = make(chan struct{})
	workspaceRuntime.invocationEntered = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		_, err := service.Run(ctx, AgentRunRequest{
			Root: access.Manifest.CanonicalRoot, Workspace: string(access.Manifest.Workspace), Prompt: "one task",
		})
		runDone <- err
	}()
	<-workspaceRuntime.invocationEntered
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() cancellation error = %v", err)
	}
	manifest, err := service.workspaces.oneWorkspaceManifest(context.Background(), access.Manifest.ProjectID, access.Manifest.Workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ActiveSession != nil {
		t.Fatalf("cancellation left active session %#v", manifest.ActiveSession)
	}
}

func TestAgentStaleCleanupDoesNotClearNewerSession(t *testing.T) {
	service, access, workspaceRuntime := agentSessionServiceFixture(t)
	workspaceRuntime.invocationBlock = make(chan struct{})
	workspaceRuntime.invocationEntered = make(chan struct{})
	runDone := make(chan error, 1)
	go func() {
		_, err := service.Run(context.Background(), AgentRunRequest{
			Root: access.Manifest.CanonicalRoot, Workspace: string(access.Manifest.Workspace), Prompt: "one task",
		})
		runDone <- err
	}()
	<-workspaceRuntime.invocationEntered
	current, unlock, err := service.workspaces.workspaceAccess(context.Background(), access.Manifest.CanonicalRoot, access.Manifest.Workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	newerID := model.RunID("01890f5c-7b00-7000-8000-000000000099")
	current.Manifest.ActiveSession = &state.SessionRecord{SessionID: newerID, Kind: "agent", Agent: "codex", StartedAt: time.Now().UTC()}
	if err := service.workspaces.replaceManifest(context.Background(), current.Manifest); err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	close(workspaceRuntime.invocationBlock)
	if err := <-runDone; model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("stale Run() error = %v, want conflict", err)
	}
	manifest, err := service.workspaces.oneWorkspaceManifest(context.Background(), access.Manifest.ProjectID, access.Manifest.Workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ActiveSession == nil || manifest.ActiveSession.SessionID != newerID {
		t.Fatalf("stale cleanup changed newer session: %#v", manifest.ActiveSession)
	}
}

func TestWorkspaceOpenReleasesLockForStop(t *testing.T) {
	service, access, workspaceRuntime := agentSessionServiceFixture(t)
	entered := make(chan struct{})
	stopped := make(chan struct{})
	var stopOnce sync.Once
	workspaceRuntime.stopHook = func() { stopOnce.Do(func() { close(stopped) }) }
	openDone := make(chan error, 1)
	go func() {
		_, err := service.workspaces.Open(context.Background(), WorkspaceOpenRequest{
			Root: access.Manifest.CanonicalRoot, Workspace: access.Manifest.Workspace, Terminal: true,
			RunInteractive: func(context.Context, InteractiveChild) (runtime.Exit, error) {
				close(entered)
				<-stopped
				code := 0
				return runtime.Exit{Code: &code}, nil
			},
		})
		openDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Open did not reach blocking execution")
	}
	if _, err := service.workspaces.Stop(context.Background(), WorkspaceStopRequest{
		Root: access.Manifest.CanonicalRoot, Workspace: access.Manifest.Workspace,
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-openDone; err != nil {
		t.Fatal(err)
	}
	manifest, err := service.workspaces.oneWorkspaceManifest(context.Background(), access.Manifest.ProjectID, access.Manifest.Workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ActiveSession != nil {
		t.Fatalf("Stop left Open session %#v", manifest.ActiveSession)
	}
}

func agentSessionServiceFixture(t *testing.T) (*AgentService, workspaceAccess, *agentSessionRuntime) {
	t.Helper()
	_, access, baseRuntime := browserSessionFixture(t)
	workspaceRuntime := &agentSessionRuntime{browserSessionRuntime: baseRuntime}
	access.Workspace.ImageDigest = "sha256:" + browserTestDigest
	workspaceRuntime.resources[access.Workspace.ID] = access.Workspace
	execution := plan.ExecutionPlan{
		ContractVersion: plan.ContractVersion,
		Project:         plan.ProjectIdentity{ID: access.Manifest.ProjectID, CanonicalRoot: access.Manifest.CanonicalRoot},
		Agents:          plan.AgentPlan{Allowed: []string{"codex"}, Default: "codex"},
		Image:           plan.ResolvedImage{Reference: fixtureAgentImageReference, InputDigest: browserTestDigest},
		ExecutableHash:  access.Manifest.PlanHash,
	}
	stateRepository := &browserStateRepository{manifest: *access.Manifest}
	workspaces := NewWorkspaceService(WorkspaceDependencies{
		ResolvePlan: func(context.Context, string) (plan.ExecutionPlan, error) { return execution, nil },
		Manifests:   stateRepository, Locks: stateRepository, Runtime: workspaceRuntime,
	})
	authRepository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	adapter := codex.New()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"canonical"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := authRepository.ImportProject(context.Background(), auth.Project{ID: access.Manifest.ProjectID, Harness: adapter.Name()}, source, false, adapter); err != nil {
		t.Fatal(err)
	}
	discovery, err := auth.NewHostDiscovery(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	authentication, err := NewAuthService(authRepository, discovery, nil, adapter)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentService(workspaces, authentication, adapter)
	if err != nil {
		t.Fatal(err)
	}
	service.agentImageReference = fixtureAgentImageReference
	return service, access, workspaceRuntime
}

const harnessAttestationPathForTest = "/usr/local/share/dsx/harnesses.lock.json"
