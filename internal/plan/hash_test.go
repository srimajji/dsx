package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/model"
)

func TestExecutionPlanHashExcludesProjectIdentityAndAuthorityOrder(t *testing.T) {
	base := ExecutionPlan{
		ContractVersion: ContractVersion,
		Project:         ProjectIdentity{ID: model.ProjectID("aaaaaaaaaaaaaaaaaaaa"), CanonicalRoot: "/tmp/project-a"},
		Agents:          AgentPlan{Allowed: []string{"omp", "codex"}, Default: "codex"},
		Image:           ResolvedImage{Reference: "example.invalid/dsx@sha256:" + strings.Repeat("a", 64), InputDigest: strings.Repeat("a", 64)},
		Repositories:    []RepositoryPlan{{Name: "workspace", HostPath: "/tmp/project-a", GuestPath: "/workspace"}},
		Auth:            AuthPlan{Imports: []string{"omp", "codex"}},
		AWS: AWSCapability{
			Mode:                    AWSModeHostDefault,
			SourceDirectory:         "/Users/example/.aws",
			SourceIdentity:          "dev=42;ino=84",
			Destination:             AWSGuestDestination,
			ReadOnly:                true,
			EligibleProfile:         AWSDefaultProfile,
			WorkspaceDefaultEnabled: false,
			AuthorityModel:          AWSAuthorityDynamicHostDefault,
		},
		Bridges: []BridgeGrant{
			{Kind: "host", Name: "database", Destination: "database.internal", Port: 5432},
			{Kind: "internet", Name: "internet"},
		},
		Limits: ResourceLimits{CPUs: 4, MemoryBytes: 6 << 30, MaxConcurrentWorkspaces: 3},
	}
	first, err := HashExecutionPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	changedIdentity := base
	changedIdentity.Project = ProjectIdentity{ID: model.ProjectID("bbbbbbbbbbbbbbbbbbbb"), CanonicalRoot: "/tmp/project-b"}
	changedIdentity.Agents.Allowed = []string{"codex", "omp"}
	changedIdentity.Auth.Imports = []string{"codex", "omp"}
	changedIdentity.Bridges = []BridgeGrant{
		{Kind: "internet", Name: "internet"},
		{Kind: "host", Name: "database", Destination: "database.internal", Port: 5432},
	}
	second, err := HashExecutionPlan(changedIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("display identity or authority order changed reusable plan hash: %s != %s", first, second)
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

func TestExecutionPlanHashExcludesAWSAvailabilityAndCredentialContentMetadata(t *testing.T) {
	base := ExecutionPlan{
		ContractVersion: ContractVersion,
		AWS: AWSCapability{
			Mode:                    AWSModeHostDefault,
			SourceDirectory:         "/Users/example/.aws",
			SourceIdentity:          "dev=42;ino=84",
			Destination:             AWSGuestDestination,
			ReadOnly:                true,
			EligibleProfile:         AWSDefaultProfile,
			WorkspaceDefaultEnabled: false,
			AuthorityModel:          AWSAuthorityDynamicHostDefault,
		},
		Provenance: config.Provenance{
			"/aws/host_availability":         {Kind: "detected", Path: "available"},
			"/aws/credential_content_digest": {Kind: "detected", Path: "generation-a"},
		},
	}
	canonical, err := json.Marshal(ExecutableProjection(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, excluded := range []string{"host_availability", "credential_content_digest", "available", "generation-a"} {
		if strings.Contains(string(canonical), excluded) {
			t.Fatalf("executable projection included host AWS availability/content metadata %q: %s", excluded, canonical)
		}
	}
	first, err := HashExecutionPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	changedHostState := base
	changedHostState.Provenance = config.Provenance{
		"/aws/host_availability":         {Kind: "detected", Path: "unavailable"},
		"/aws/credential_content_digest": {Kind: "detected", Path: "generation-b"},
	}
	second, err := HashExecutionPlan(changedHostState)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("host AWS availability/content metadata changed reusable plan hash: %s != %s", first, second)
	}
}

func TestExecutionPlanHashIncludesEveryAWSAuthorityField(t *testing.T) {
	base := ExecutionPlan{
		ContractVersion: ContractVersion,
		AWS: AWSCapability{
			Mode:                    AWSModeHostDefault,
			SourceDirectory:         "/Users/example/.aws",
			SourceIdentity:          "dev=42;ino=84",
			Destination:             AWSGuestDestination,
			ReadOnly:                true,
			EligibleProfile:         AWSDefaultProfile,
			WorkspaceDefaultEnabled: false,
			AuthorityModel:          AWSAuthorityDynamicHostDefault,
		},
	}
	if projection := ExecutableProjection(base); projection.AWS != base.AWS {
		t.Fatalf("executable projection omitted AWS authority: %#v", projection.AWS)
	}
	assertNoAWSBridge(t, base.Bridges)
	original, err := HashExecutionPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*AWSCapability){
		"mode": func(capability *AWSCapability) {
			capability.Mode = AWSModeNone
		},
		"source directory": func(capability *AWSCapability) {
			capability.SourceDirectory = "/Users/example/other-aws"
		},
		"source identity": func(capability *AWSCapability) {
			capability.SourceIdentity = "dev=42;ino=85"
		},
		"destination": func(capability *AWSCapability) {
			capability.Destination = "/run/dsx/other-aws"
		},
		"read-only": func(capability *AWSCapability) {
			capability.ReadOnly = false
		},
		"eligible profile": func(capability *AWSCapability) {
			capability.EligibleProfile = "engineering"
		},
		"workspace default": func(capability *AWSCapability) {
			capability.WorkspaceDefaultEnabled = true
		},
		"authority model": func(capability *AWSCapability) {
			capability.AuthorityModel = "pinned"
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed.AWS)
			hash, err := HashExecutionPlan(changed)
			if err != nil {
				t.Fatal(err)
			}
			if hash == original {
				t.Fatalf("changing AWS %s did not change executable plan hash", name)
			}
		})
	}
}
