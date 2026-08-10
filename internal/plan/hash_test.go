package plan

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/state"
)

func TestHashExcludesRuntimeDisplayAndSecretValues(t *testing.T) {
	base := hashTestPlan()
	want := mustHash(t, base)

	changed := hashTestPlan()
	changed.Project.CanonicalRoot = "/another/root"
	changed.Project.ID = "bbbbbbbbbbbbbbbbbbbb"
	changed.Sandbox.Name = "different"
	changed.Sandbox.RunID = "01988aa0-2d00-7000-8000-000000000002"
	changed.Ownership.ResourceName = "derived-at-runtime"
	changed.Ownership.Labels = []KeyValue{{Key: "run", Value: "other"}}
	changed.Provenance = config.Provenance{"/setup/0/argv/0": {Kind: "project", Path: "elsewhere.jsonc", Line: 99, Column: 4}}
	changed.ExecutableHash = "display-only"
	changed.Setup[0].Env[1].Value = "a different resolved secret value"

	if got := mustHash(t, changed); got != want {
		t.Fatalf("runtime/display/provenance/secret-only changes changed hash: got %s want %s", got, want)
	}
	projection := ExecutableProjection(changed)
	for _, environment := range projection.Setup[0].Env {
		if environment.Secret && environment.Value != "" {
			t.Fatalf("secret value entered executable projection: %#v", environment)
		}
	}
}

func TestHashChangesForExecutableAuthority(t *testing.T) {
	tests := map[string]func(*ExecutionPlan){
		"mode":                 func(plan *ExecutionPlan) { plan.Mode = model.ModeClone },
		"agent":                func(plan *ExecutionPlan) { plan.Agent = "omp" },
		"image digest":         func(plan *ExecutionPlan) { plan.Image.InputDigest = digest("b") },
		"image reference":      func(plan *ExecutionPlan) { plan.Image.Reference = "example/image@sha256:" + digest("c") },
		"build input":          func(plan *ExecutionPlan) { plan.Image.BuildArgs[0].Value = "release" },
		"repository source":    func(plan *ExecutionPlan) { plan.Repositories[0].SourceCommit = digest("d") },
		"setup argv":           func(plan *ExecutionPlan) { plan.Setup[0].Argv[1] = "ci" },
		"setup shell":          func(plan *ExecutionPlan) { plan.Setup[0].Shell = "echo changed" },
		"setup shell path":     func(plan *ExecutionPlan) { plan.Setup[0].ShellPath = "/bin/bash" },
		"nonsecret env":        func(plan *ExecutionPlan) { plan.Setup[0].Env[0].Value = "production" },
		"secret reference":     func(plan *ExecutionPlan) { plan.Setup[0].Env[1].Reference = "secret://other" },
		"process command":      func(plan *ExecutionPlan) { plan.Processes[0].Command.Argv[0] = "other-server" },
		"process graph":        func(plan *ExecutionPlan) { plan.Processes[0].DependsOn = append(plan.Processes[0].DependsOn, "cache") },
		"process required":     func(plan *ExecutionPlan) { plan.Processes[0].Required = false },
		"process terminal":     func(plan *ExecutionPlan) { plan.Processes[0].Terminal = !plan.Processes[0].Terminal },
		"process health":       func(plan *ExecutionPlan) { plan.Processes[0].Health.TimeoutMS++ },
		"mount":                func(plan *ExecutionPlan) { plan.Mounts[0].ReadOnly = !plan.Mounts[0].ReadOnly },
		"volume":               func(plan *ExecutionPlan) { plan.Volumes[0].Persistent = !plan.Volumes[0].Persistent },
		"auth reference":       func(plan *ExecutionPlan) { plan.Auth[0].Profile = "other-profile" },
		"port":                 func(plan *ExecutionPlan) { plan.Ports[0].GuestPort++ },
		"network grant":        func(plan *ExecutionPlan) { plan.Bridges[0].Destination = "other.internal" },
		"browser grant":        func(plan *ExecutionPlan) { plan.Browser.Enabled = false },
		"browser image digest": func(plan *ExecutionPlan) { plan.Browser.ImageDigest = digest("e") },
		"limit":                func(plan *ExecutionPlan) { plan.Limits.MemoryBytes++ },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			base := hashTestPlan()
			wantDifferentFrom := mustHash(t, base)
			mutate(&base)
			if got := mustHash(t, base); got == wantDifferentFrom {
				t.Fatalf("executable change did not alter hash %s", got)
			}
		})
	}
}

func TestHashNormalizesSetLikeSlicesWithoutMutatingPlan(t *testing.T) {
	base := hashTestPlan()
	want := mustHash(t, base)
	changed := hashTestPlan()
	slices.Reverse(changed.Image.BuildArgs)
	slices.Reverse(changed.Repositories)
	slices.Reverse(changed.Processes)
	slices.Reverse(changed.Processes[0].DependsOn)
	slices.Reverse(changed.Processes[0].Command.Env)
	slices.Reverse(changed.Mounts)
	slices.Reverse(changed.Volumes)
	slices.Reverse(changed.Auth)
	slices.Reverse(changed.Ports)
	slices.Reverse(changed.Bridges)

	beforeFirstMount := changed.Mounts[0]
	if got := mustHash(t, changed); got != want {
		t.Fatalf("set ordering changed hash: got %s want %s", got, want)
	}
	if changed.Mounts[0] != beforeFirstMount {
		t.Fatal("hashing mutated source plan")
	}
}

func TestHashParsedJSONCCommentsStableThenGrantChanges(t *testing.T) {
	first := []byte(`{
  "schemaVersion": 1,
  "workspace": {"root":".","members":[]},
  "image": {"ref":"example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "setup": [], "processes": {}, "volumes": {}, "mounts": [],
  "agents": {"default":"codex","allowed":["codex"]}, "authProfiles": {},
  "ports": [], "browser": {"enabled":false}, "aws": {"mode":"none"},
  "network": {"internet":true,"hostGrants":[]},
  "resources": {"cpus":2,"memory":"2GiB","maxConcurrentClones":1}
}`)
	second := []byte(`{
  // A comment is not executable authority.
  "schemaVersion": 1,
  "workspace": {"root":".","members":[]},
  "image": {"ref":"example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "setup": [], "processes": {}, "volumes": {}, "mounts": [],
  "agents": {"default":"codex","allowed":["codex"]}, "authProfiles": {},
  "ports": [], "browser": {"enabled":false}, "aws": {"mode":"none"},
  "network": {"internet":true,"hostGrants":[]},
  "resources": {"cpus":2,"memory":"2GiB","maxConcurrentClones":1}
}`)
	left := resolveHashFixture(t, first)
	right := resolveHashFixture(t, second)
	if left.ExecutableHash != right.ExecutableHash {
		t.Fatalf("JSONC comment changed hash: %s != %s", left.ExecutableHash, right.ExecutableHash)
	}
	if right.ExecutableHash == "" {
		t.Fatal("ResolvePlan did not set ExecutableHash")
	}
	t.Logf("comment-only plans stable at %s", left.ExecutableHash)
	right.Bridges[0].ReadOnly = true
	changed := mustHash(t, right)
	if changed == left.ExecutableHash {
		t.Fatalf("executable grant mutation kept hash %s", changed)
	}
	if err := state.AuthorizeApproval(context.Background(), nil, state.ApprovalRequest{
		Mode:         state.ApprovalModeNonInteractive,
		Record:       state.ApprovalRecord{ProjectID: left.Project.ID, Hash: changed},
		ApprovedHash: changed,
	}); err != nil {
		t.Fatalf("exact approval failed: %v", err)
	}
	t.Logf("grant mutation changed hash to %s; exact approval accepted", changed)
	if err := state.AuthorizeApproval(context.Background(), nil, state.ApprovalRequest{
		Mode:         state.ApprovalModeNonInteractive,
		Record:       state.ApprovalRecord{ProjectID: left.Project.ID, Hash: changed},
		ApprovedHash: left.ExecutableHash,
	}); model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("stale approval error = %v (code %q), want unapproved", err, model.ErrorCodeOf(err))
	}
}

func resolveHashFixture(t *testing.T, source []byte) ExecutionPlan {
	t.Helper()
	validated, diagnostics := config.ParseBytes("fixture.jsonc", source)
	if len(diagnostics) != 0 {
		t.Fatalf("parse fixture: %#v", diagnostics)
	}
	projectID, err := model.ParseProjectID("aaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	resolved, resolveDiagnostics, err := NewResolver().Resolve(context.Background(), ResolveInput{
		Config:   validated,
		Project:  ProjectIdentity{ID: projectID, CanonicalRoot: "/project"},
		Sandbox:  SandboxIdentity{Name: "main", RunID: "01988aa0-2d00-7000-8000-000000000001"},
		Mode:     model.ModeLive,
		Defaults: DefaultValues{Agent: "codex", Internet: true, CPUs: 2, MemoryBytes: 2 << 30, MaxConcurrentClones: 1},
	})
	if err != nil || len(resolveDiagnostics) != 0 {
		t.Fatalf("resolve fixture: diagnostics=%#v err=%v", resolveDiagnostics, err)
	}
	return resolved
}

func mustHash(t *testing.T, plan ExecutionPlan) string {
	t.Helper()
	hash, err := HashExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func hashTestPlan() ExecutionPlan {
	hostPort := uint16(8080)
	return ExecutionPlan{
		ContractVersion: ContractVersion,
		Project:         ProjectIdentity{ID: "aaaaaaaaaaaaaaaaaaaa", CanonicalRoot: "/project"},
		Sandbox:         SandboxIdentity{Name: "main", RunID: "01988aa0-2d00-7000-8000-000000000001"},
		Mode:            model.ModeLive,
		Agent:           "codex",
		Image: ResolvedImage{
			Reference: "example/image@sha256:" + digest("a"), InputDigest: digest("a"),
			BuildArgs: []KeyValue{{Key: "z", Value: "last"}, {Key: "mode", Value: "debug"}},
		},
		Repositories: []RepositoryPlan{
			{Name: "web", HostPath: "/project/web", GuestPath: "/workspace/web", SourceRef: "main", SourceCommit: digest("1"), TrackedDigest: digest("2")},
			{Name: "api", HostPath: "/project/api", GuestPath: "/workspace/api", SourceRef: "main", SourceCommit: digest("3"), TrackedDigest: digest("4")},
		},
		Setup: []ResolvedCommand{{
			Argv: []string{"npm", "install"}, Shell: "echo setup", ShellPath: "/bin/sh", Cwd: "/workspace",
			Env: []EnvGrant{{Name: "MODE", Value: "development"}, {Name: "TOKEN", Value: "resolved-secret", Reference: "secret://token", Secret: true}},
		}},
		Processes: []ResolvedProcess{
			{Name: "web", Command: ResolvedCommand{Argv: []string{"server"}, Cwd: "/workspace/web", Env: []EnvGrant{{Name: "PORT", Value: "3000"}}}, DependsOn: []string{"db", "assets"}, Required: true, Health: &ResolvedHealth{Kind: "http", Target: "http://127.0.0.1:3000/health", IntervalMS: 1000, TimeoutMS: 500, Retries: 3}},
			{Name: "assets", Command: ResolvedCommand{Argv: []string{"builder"}, Cwd: "/workspace"}},
		},
		Mounts:  []ResolvedMount{{SourceType: "workspace", Source: "/project", Target: "/workspace", ReadOnly: false}, {SourceType: "host", Source: "/data", Target: "/data", ReadOnly: true}},
		Volumes: []ResolvedVolume{{Name: "cache", Target: "/cache", Scope: "sandbox", Persistent: true}, {Name: "deps", Target: "/deps", Scope: "project"}},
		Auth:    []ResolvedAuthGrant{{Harness: "codex", Profile: "main", Persistence: "global"}, {Harness: "omp", Profile: "work", Persistence: "sandbox"}},
		Ports: []PortRequest{
			{Name: "web", GuestPort: 3000, Protocol: "tcp", HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: &hostPort},
			{Name: "debug", GuestPort: 9229, Protocol: "tcp", HostIP: netip.MustParseAddr("127.0.0.1")},
		},
		Browser:    &BrowserPlan{Enabled: true, ImageDigest: digest("9")},
		Bridges:    []BridgeGrant{{Kind: "host", Name: "db", Destination: "db.internal", Port: 5432}, {Kind: "internet", Name: "internet"}},
		Limits:     ResourceLimits{CPUs: 4, MemoryBytes: 8 << 30, MaxConcurrentClones: 2},
		Ownership:  OwnershipPlan{Labels: []KeyValue{{Key: "run", Value: "id"}}, ResourceName: "derived"},
		Provenance: config.Provenance{"/mode": {Kind: "detected", Path: "/project"}},
	}
}

func digest(character string) string {
	value := ""
	for len(value) < 64 {
		value += character
	}
	return value[:64]
}
