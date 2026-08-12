package plan

import (
	"reflect"
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
	if !reflect.DeepEqual(resolved.AWS, AWSCapability{Mode: AWSModeNone}) {
		t.Fatalf("default AWS capability = %#v", resolved.AWS)
	}
	assertNoAWSBridge(t, resolved.Bridges)
	if len(resolved.ExecutableHash) != 64 {
		t.Fatalf("executable hash = %q", resolved.ExecutableHash)
	}
}

func TestResolvePlanProducesHostDefaultCapabilityWithoutAWSBridgeGrant(t *testing.T) {
	input := awsResolveInput()
	resolved, err := ResolvePlan(input)
	if err != nil {
		t.Fatal(err)
	}
	want := AWSCapability{
		Mode:                    AWSModeHostDefault,
		SourceDirectory:         "/Users/example/.aws",
		SourceIdentity:          "dev=42;ino=84",
		Destination:             AWSGuestDestination,
		ReadOnly:                true,
		EligibleProfile:         AWSDefaultProfile,
		WorkspaceDefaultEnabled: false,
		AuthorityModel:          AWSAuthorityDynamicHostDefault,
	}
	if !reflect.DeepEqual(resolved.AWS, want) {
		t.Fatalf("AWS capability = %#v, want %#v", resolved.AWS, want)
	}
	assertNoAWSBridge(t, resolved.Bridges)
	for _, field := range []string{
		"/aws/mode",
		"/aws/source_directory",
		"/aws/source_identity",
		"/aws/destination",
		"/aws/read_only",
		"/aws/eligible_profile",
		"/aws/workspace_default_enabled",
		"/aws/authority_model",
	} {
		if _, found := resolved.Provenance[field]; !found {
			t.Errorf("AWS authority provenance missing %q", field)
		}
	}
}

func TestResolvePlanRequiresExactHostDefaultSourceAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*ResolveInput){
		"missing": func(input *ResolveInput) {
			input.Authority.HostDefaultAWSDirectory = nil
		},
		"missing canonical source": func(input *ResolveInput) {
			input.Authority.HostDefaultAWSDirectory.CanonicalPath = ""
		},
		"missing source identity": func(input *ResolveInput) {
			input.Authority.HostDefaultAWSDirectory.Identity = ""
		},
		"configured directory mismatch": func(input *ResolveInput) {
			input.Authority.HostDefaultAWSDirectory.DeclaredPath = "/Users/example/other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := awsResolveInput()
			mutate(&input)
			if _, err := ResolvePlan(input); err == nil {
				t.Fatal("ResolvePlan accepted invalid host-default source authority")
			}
		})
	}

	input := awsResolveInput()
	input.Config.Document.AWS = config.AWSConfig{Mode: AWSModeNone}
	if _, err := ResolvePlan(input); err == nil {
		t.Fatal("ResolvePlan accepted host AWS authority while capability was disabled")
	}
}

func awsResolveInput() ResolveInput {
	digest := strings.Repeat("a", 64)
	return ResolveInput{
		Config: config.ValidatedConfig{Document: config.ConfigDocument{
			Workspace: config.WorkspaceConfig{Root: "."},
			Image:     config.ImageConfig{Ref: "example.invalid/dsx@sha256:" + digest},
			Agents:    config.AgentConfig{Allowed: []string{"codex"}, Default: "codex"},
			AWS:       config.AWSConfig{Mode: AWSModeHostDefault, Directory: "/Users/example/.aws"},
		}},
		Project: ProjectIdentity{ID: model.ProjectID("aaaaaaaaaaaaaaaaaaaa"), CanonicalRoot: "/tmp/project"},
		Defaults: DefaultValues{
			DefaultAgent: "codex", CPUs: 2, MemoryBytes: 2 << 30, MaxConcurrentWorkspaces: 1,
		},
		Authority: AuthorityInputs{
			StandardImageDigest: digest,
			HostDefaultAWSDirectory: &HostMountAuthority{
				DeclaredPath:  "/Users/example/.aws",
				CanonicalPath: "/Users/example/.aws",
				Identity:      "dev=42;ino=84",
			},
		},
	}
}

func assertNoAWSBridge(t *testing.T, bridges []BridgeGrant) {
	t.Helper()
	for _, bridge := range bridges {
		if bridge.Kind == "aws" {
			t.Fatalf("AWS capability leaked into BridgeGrant: %#v", bridge)
		}
	}
}
