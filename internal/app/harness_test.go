package app

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

const fixtureAgentImageReference = "ghcr.io/example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeHarnessAdapter struct {
	name             harness.Name
	seedDestinations *[]string
}

func (adapter fakeHarnessAdapter) Name() harness.Name {
	if adapter.name == "" {
		return harness.Codex
	}
	return adapter.name
}
func (fakeHarnessAdapter) Version() harness.PinnedArtifact {
	return harness.PinnedArtifact{Version: "rust-v0.147.0", Executable: "codex"}
}
func (fakeHarnessAdapter) ValidateVersion(stdout, stderr string) error {
	if !strings.Contains(stdout+stderr, "shell-ok") {
		return errors.New("wrong fake version")
	}
	return nil
}
func (fakeHarnessAdapter) Preflight(_ context.Context, roots harness.RunRoots) ([]harness.Diagnostic, error) {
	return nil, harness.ValidateRoots(roots)
}
func (fakeHarnessAdapter) Invocation(request harness.InvocationRequest) (harness.ExecSpec, error) {
	arguments := []string{"/usr/local/bin/fake"}
	if request.Interactive {
		arguments = append(arguments, "interactive")
	} else {
		arguments = append(arguments, "run", request.Prompt)
	}
	return harness.ExecSpec{Argv: arguments, Env: rootEnvironment(request.Roots, fakeHarnessAdapter{}.AuthLayout()), Cwd: request.Roots.Workspace, Terminal: request.Interactive}, nil
}
func (fakeHarnessAdapter) AuthLayout() harness.AuthLayout {
	return harness.AuthLayout{Backend: harness.StorageFile, CredentialArtifacts: []string{"auth.json"}, MaxArtifactBytes: 1 << 20, Environment: map[string]string{"FAKE_AUTH": "."}}
}
func (adapter fakeHarnessAdapter) Seed(ctx context.Context, request harness.SeedRequest) error {
	if adapter.seedDestinations != nil {
		*adapter.seedDestinations = append(*adapter.seedDestinations, request.SourceRoot, request.DestinationRoot)
	}
	return harness.SeedArtifacts(ctx, request)
}
func (fakeHarnessAdapter) EphemeralMCP(request harness.MCPRequest) (harness.ConfigInjection, error) {
	if len(request.Servers) == 0 {
		return harness.ConfigInjection{}, nil
	}
	data, err := json.Marshal(request.Servers)
	if err != nil {
		return harness.ConfigInjection{}, err
	}
	return harness.ConfigInjection{Files: []harness.GeneratedFile{{Path: path.Join(request.Roots.Config, "mcp.json"), Mode: 0o600, Data: data}}, Args: []string{"--mcp", path.Join(request.Roots.Config, "mcp.json")}}, nil
}
func (fakeHarnessAdapter) Login(request harness.LoginRequest) (harness.LoginFlow, error) {
	return harness.LoginFlow{Exec: harness.ExecSpec{Argv: []string{"/usr/local/bin/fake", "login"}, Cwd: request.Roots.Workspace, Terminal: true}}, nil
}
func (fakeHarnessAdapter) RedactionRules() harness.RedactionRules {
	return harness.RedactionRules{EnvironmentKeys: []string{"FAKE_TOKEN"}}
}

type harnessHostAWSManager struct {
	status      bridge.HostAWSMirrorStatus
	statusCalls int
}

func (*harnessHostAWSManager) Prepare(context.Context, bridge.LeaseIdentity) (string, error) {
	return "", errors.New("unexpected Prepare")
}

func (*harnessHostAWSManager) Enable(context.Context, bridge.LeaseIdentity, bridge.HostAWSAuthority) (string, error) {
	return "", errors.New("unexpected Enable")
}

func (*harnessHostAWSManager) Disable(context.Context, bridge.LeaseIdentity) error {
	return errors.New("unexpected Disable")
}

func (*harnessHostAWSManager) Remove(context.Context, bridge.LeaseIdentity) error {
	return errors.New("unexpected Remove")
}

func (manager *harnessHostAWSManager) Status(context.Context, bridge.LeaseIdentity) (bridge.HostAWSMirrorStatus, error) {
	manager.statusCalls++
	return manager.status, nil
}

func TestResolveAgentPrecedenceAndAllowlist(t *testing.T) {
	tests := []struct {
		name             string
		explicit         string
		workspaceDefault string
		projectDefault   string
		allowed          []string
		want             harness.Name
		wantCode         model.ErrorCode
	}{
		{name: "explicit override", explicit: "codex", workspaceDefault: "claude", projectDefault: "omp", allowed: []string{"omp", "claude", "codex"}, want: harness.Codex},
		{name: "workspace default", workspaceDefault: "claude", projectDefault: "omp", allowed: []string{"omp", "claude"}, want: harness.Claude},
		{name: "project default", projectDefault: "omp", allowed: []string{"omp"}, want: harness.OMP},
		{name: "unapproved explicit", explicit: "codex", workspaceDefault: "omp", projectDefault: "omp", allowed: []string{"omp"}, wantCode: model.CodeUnapproved},
		{name: "missing defaults", allowed: []string{"omp"}, wantCode: model.CodeInvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveAgent(test.explicit, test.workspaceDefault, test.projectDefault, test.allowed)
			if got != test.want {
				t.Fatalf("resolveAgent() agent = %q, want %q", got, test.want)
			}
			if test.wantCode == "" && err != nil {
				t.Fatalf("resolveAgent() error = %v", err)
			}
			if test.wantCode != "" && model.ErrorCodeOf(err) != test.wantCode {
				t.Fatalf("resolveAgent() error = %v, want code %q", err, test.wantCode)
			}
		})
	}
}

func TestInvocationOverrideDoesNotMutateDefaults(t *testing.T) {
	allowed := []string{"omp", "codex"}
	workspaceDefault := "omp"
	projectDefault := "codex"
	selected, err := resolveAgent("codex", workspaceDefault, projectDefault, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if selected != harness.Codex || workspaceDefault != "omp" || projectDefault != "codex" || !reflect.DeepEqual(allowed, []string{"omp", "codex"}) {
		t.Fatalf("resolution mutated durable defaults: selected=%q workspace=%q project=%q allowed=%#v", selected, workspaceDefault, projectDefault, allowed)
	}
}

func TestSelectedHarnessEnvironmentContainsOnlyItsCredentialRoot(t *testing.T) {
	runID, err := model.ParseRunID("01890f5c-7b00-7000-8000-000000000041")
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"AWS_CONFIG_FILE":             "/host/aws/config",
		"AWS_SHARED_CREDENTIALS_FILE": "/host/aws/credentials",
		"AWS_PROFILE":                 "default",
		"AWS_DEFAULT_PROFILE":         "default",
	} {
		t.Setenv(key, value)
	}
	roots := harnessRoots(runID)
	environment := rootEnvironment(roots, fakeHarnessAdapter{name: harness.Codex}.AuthLayout())
	if environment["FAKE_AUTH"] != roots.Auth {
		t.Fatalf("selected harness auth root = %q, want %q", environment["FAKE_AUTH"], roots.Auth)
	}
	if environment["HOME"] != roots.Home {
		t.Fatalf("agent HOME = %q, want ephemeral %q", environment["HOME"], roots.Home)
	}
	for _, forbidden := range []string{
		"CODEX_HOME", "CLAUDE_CONFIG_DIR", "OPENCODE_CONFIG",
		"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
	} {
		if _, found := environment[forbidden]; found {
			t.Fatalf("credential environment leaked unrelated key %q: %#v", forbidden, environment)
		}
	}
}

func TestHarnessEnvironmentUsesOnlyTypedReadyWorkspaceAWSGrant(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := state.Manifest{
		ProjectID: projectID, CanonicalRoot: root, Workspace: model.WorkspaceName("agent-aws"),
		RunID: model.RunID("01890f5c-7b00-7000-8000-000000000061"),
	}
	ambient := []string{
		"SAFE_AGENT_SETTING=kept",
		"AWS_CONFIG_FILE=/host/aws/config",
		"AWS_SHARED_CREDENTIALS_FILE=/host/aws/credentials",
		"AWS_PROFILE=named",
		"AWS_DEFAULT_PROFILE=named",
	}
	for _, assignment := range ambient[1:] {
		key, value, _ := strings.Cut(assignment, "=")
		t.Setenv(key, value)
	}

	t.Run("enabled and current", func(t *testing.T) {
		manager := &harnessHostAWSManager{status: bridge.HostAWSMirrorStatus{State: AWSMirrorCurrent}}
		workspaces := NewWorkspaceService(WorkspaceDependencies{HostAWS: manager})
		enabled := manifest
		enabled.AWSGrant = &state.AWSGrantRecord{Enabled: true}
		prepared, err := workspaces.PrepareWorkspaceExecution(context.Background(), enabled, runtime.ExecSpec{Env: append([]string(nil), ambient...)})
		if err != nil {
			t.Fatal(err)
		}
		environment, err := environmentMap(prepared.Env)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"SAFE_AGENT_SETTING":          "kept",
			"AWS_CONFIG_FILE":             bridge.HostAWSConfigGuestPath,
			"AWS_SHARED_CREDENTIALS_FILE": bridge.HostAWSCredentialsGuestPath,
		}
		if !reflect.DeepEqual(environment, want) || manager.statusCalls != 1 {
			t.Fatalf("enabled harness environment = %#v, status calls %d; want %#v, 1", environment, manager.statusCalls, want)
		}
	})

	t.Run("enabled but not current", func(t *testing.T) {
		manager := &harnessHostAWSManager{status: bridge.HostAWSMirrorStatus{State: AWSMirrorStopped}}
		workspaces := NewWorkspaceService(WorkspaceDependencies{HostAWS: manager})
		enabled := manifest
		enabled.AWSGrant = &state.AWSGrantRecord{Enabled: true}
		prepared, err := workspaces.PrepareWorkspaceExecution(context.Background(), enabled, runtime.ExecSpec{Env: append([]string(nil), ambient...)})
		if err == nil {
			t.Fatal("enabled harness accepted a non-current AWS publication")
		}
		environment, mapErr := environmentMap(prepared.Env)
		if mapErr != nil {
			t.Fatal(mapErr)
		}
		want := map[string]string{"SAFE_AGENT_SETTING": "kept"}
		if !reflect.DeepEqual(environment, want) || manager.statusCalls != 1 {
			t.Fatalf("non-current harness environment = %#v, status calls %d; want %#v, 1", environment, manager.statusCalls, want)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		manager := &harnessHostAWSManager{status: bridge.HostAWSMirrorStatus{State: AWSMirrorCurrent}}
		workspaces := NewWorkspaceService(WorkspaceDependencies{HostAWS: manager})
		disabled := manifest
		disabled.AWSGrant = &state.AWSGrantRecord{}
		prepared, err := workspaces.PrepareWorkspaceExecution(context.Background(), disabled, runtime.ExecSpec{Env: append([]string(nil), ambient...)})
		if err != nil {
			t.Fatal(err)
		}
		environment, err := environmentMap(prepared.Env)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"SAFE_AGENT_SETTING": "kept"}
		if !reflect.DeepEqual(environment, want) || manager.statusCalls != 0 {
			t.Fatalf("disabled harness environment = %#v, status calls %d; want %#v, 0", environment, manager.statusCalls, want)
		}
	})
}
