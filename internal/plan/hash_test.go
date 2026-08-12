package plan

import (
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/model"
)

func TestExecutionPlanHashExcludesProjectAndOrdersAuthority(t *testing.T) {
	base := ExecutionPlan{
		ContractVersion: ContractVersion,
		Project:         ProjectIdentity{ID: model.ProjectID("aaaaaaaaaaaaaaaaaaaa"), CanonicalRoot: "/tmp/project-a"},
		Agents:          AgentPlan{Allowed: []string{"omp", "codex"}, Default: "codex"},
		Image:           ResolvedImage{Reference: "example.invalid/dsx@sha256:" + strings.Repeat("a", 64), InputDigest: strings.Repeat("a", 64)},
		Repositories:    []RepositoryPlan{{Name: "workspace", HostPath: "/tmp/project-a", GuestPath: "/workspace"}},
		Auth:            AuthPlan{Imports: []string{"omp", "codex"}},
		Limits:          ResourceLimits{CPUs: 4, MemoryBytes: 6 << 30, MaxConcurrentWorkspaces: 3},
	}
	first, err := HashExecutionPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	changedIdentity := base
	changedIdentity.Project = ProjectIdentity{ID: model.ProjectID("bbbbbbbbbbbbbbbbbbbb"), CanonicalRoot: "/tmp/project-b"}
	changedIdentity.Agents.Allowed = []string{"codex", "omp"}
	changedIdentity.Auth.Imports = []string{"codex", "omp"}
	second, err := HashExecutionPlan(changedIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("display identity or ordering changed reusable plan hash: %s != %s", first, second)
	}
	changedAuthority := base
	changedAuthority.Agents.Default = "omp"
	third, err := HashExecutionPlan(changedAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("agent default did not change executable plan hash")
	}
}
