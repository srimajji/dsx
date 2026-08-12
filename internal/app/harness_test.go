package app

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
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
	roots := harnessRoots(runID)
	environment := rootEnvironment(roots, fakeHarnessAdapter{name: harness.Codex}.AuthLayout())
	if environment["FAKE_AUTH"] != roots.Auth {
		t.Fatalf("selected harness auth root = %q, want %q", environment["FAKE_AUTH"], roots.Auth)
	}
	if environment["HOME"] != roots.Home {
		t.Fatalf("agent HOME = %q, want ephemeral %q", environment["HOME"], roots.Home)
	}
	for _, forbidden := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "OPENCODE_CONFIG", "AWS_SHARED_CREDENTIALS_FILE"} {
		if _, found := environment[forbidden]; found {
			t.Fatalf("credential environment leaked unrelated key %q: %#v", forbidden, environment)
		}
	}
}
