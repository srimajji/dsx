package plan

import (
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/model"
)

func TestResolvePlanProducesReusableWorkspaceDefaults(t *testing.T) {
	digest := strings.Repeat("a", 64)
	validated := config.ValidatedConfig{Document: config.ConfigDocument{
		Workspace: config.WorkspaceConfig{Root: "."},
		Image:     config.ImageConfig{Ref: "example.invalid/dsx@sha256:" + digest},
		Agents:    config.AgentConfig{Allowed: []string{"omp", "codex"}, Default: "omp"},
		Auth:      config.AuthConfig{Imports: []string{"opencode", "codex"}},
		Resources: config.ResourceLimits{CPUs: 4, Memory: "6GiB", MaxConcurrentWorkspaces: 3},
	}}
	resolved, err := ResolvePlan(ResolveInput{
		Config:    validated,
		Project:   ProjectIdentity{ID: model.ProjectID("aaaaaaaaaaaaaaaaaaaa"), CanonicalRoot: "/tmp/project"},
		Defaults:  DefaultValues{DefaultAgent: "codex", Internet: true, CPUs: 4, MemoryBytes: 6 << 30, MaxConcurrentWorkspaces: 1},
		Authority: AuthorityInputs{StandardImageDigest: digest},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContractVersion != ContractVersion || resolved.Agents.Default != "omp" {
		t.Fatalf("resolved identity/agents = %#v", resolved)
	}
	if got := resolved.Agents.Allowed; len(got) != 2 || got[0] != "codex" || got[1] != "omp" {
		t.Fatalf("allowed agents = %#v", got)
	}
	if got := resolved.Auth.Imports; len(got) != 2 || got[0] != "codex" || got[1] != "opencode" {
		t.Fatalf("auth imports = %#v", got)
	}
	if resolved.Limits.MaxConcurrentWorkspaces != 3 || len(resolved.Repositories) != 1 || resolved.Repositories[0].GuestPath != "/workspace" {
		t.Fatalf("workspace defaults = %#v", resolved)
	}
	if len(resolved.ExecutableHash) != 64 {
		t.Fatalf("executable hash = %q", resolved.ExecutableHash)
	}
}
