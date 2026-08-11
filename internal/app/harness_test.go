package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	agentimage "github.com/srimajji/dsx/images/agent"
	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
)

const fixtureAgentImageReference = "ghcr.io/example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeHarnessAdapter struct {
	seedDestinations *[]string
}

func (fakeHarnessAdapter) Name() harness.Name { return harness.Codex }
func (fakeHarnessAdapter) Version() harness.PinnedArtifact {
	return harness.PinnedArtifact{
		Version:    "rust-v0.147.0",
		Source:     "https://github.com/openai/codex/releases/download/rust-v0.147.0/codex-package-aarch64-unknown-linux-musl.tar.gz",
		Digest:     "sha256:89cbf79bd5ae6f9c58da47e8079f311c84219350c9c43c070d42f3e9b2a81401",
		Executable: "codex",
	}
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
	environment := map[string]string{"FAKE_AUTH": request.Roots.Auth}
	for key, value := range request.Environment {
		environment[key] = value
	}
	return harness.ExecSpec{Argv: arguments, Env: environment, Cwd: request.Roots.Workspace, Terminal: request.Interactive}, nil
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
	return harness.RedactionRules{EnvironmentKeys: []string{"FAKE_TOKEN", "OPENCODE_CONFIG_CONTENT"}}
}

type rejectingHarnessAdapter struct {
	fakeHarnessAdapter
}

func (rejectingHarnessAdapter) Seed(ctx context.Context, request harness.SeedRequest) error {
	data, err := os.ReadFile(filepath.Join(request.SourceRoot, "auth.json"))
	if err == nil && strings.Contains(string(data), "refreshed") {
		return errors.New("corrupt refreshed credential")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return harness.SeedArtifacts(ctx, request)
}

type verifyingHarnessAdapter struct {
	fakeHarnessAdapter
	reject bool
}

func (verifyingHarnessAdapter) MCPVerification(request harness.MCPRequest, _ harness.ConfigInjection) (harness.ExecSpec, error) {
	return harness.ExecSpec{
		Argv: []string{"/usr/local/bin/fake", "debug", "config"},
		Env:  map[string]string{"OPENCODE_CONFIG_CONTENT": `{"mcp":{}}`},
		Cwd:  request.Roots.Workspace,
	}, nil
}

func (adapter verifyingHarnessAdapter) ValidateEffectiveMCP(_ harness.MCPRequest, stdout, _ string) error {
	if adapter.reject || !strings.Contains(stdout, "shell-ok") {
		return errors.New("hostile inherited MCP registry")
	}
	return nil
}

func TestHarnessRefusesInexactMCPRegistryBeforeAgentExecution(t *testing.T) {
	lifecycle, root, _, fakeRuntime, _ := lifecycleFixture(t)
	writeHarnessAuthConfig(t, root, "global")
	hash := inspectLifecycleHash(t, lifecycle.inspection, root)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, verifyingHarnessAdapter{reject: true})
	if err != nil {
		t.Fatal(err)
	}
	service.agentImageReference = fixtureAgentImageReference
	_, err = service.Run(context.Background(), HarnessRunRequest{
		Root: root, ApproveConfig: hash, Agent: "codex", Prompt: "must not start",
	})
	if err == nil || !strings.Contains(err.Error(), "refuse inexact effective MCP registry") {
		t.Fatalf("Run() error = %v", err)
	}
	calls := strings.Join(fakeRuntime.calls, "\n")
	if !strings.Contains(calls, "debug config") || strings.Contains(calls, " run must not start") {
		t.Fatalf("prelaunch verification calls = %s", calls)
	}
}

func TestHarnessServiceOneShotCopiesAuthInjectsMCPAndPromotes(t *testing.T) {
	lifecycle, root, _, fakeRuntime, _ := lifecycleFixture(t)
	writeHarnessAuthConfig(t, root, "global")
	hash := inspectLifecycleHash(t, lifecycle.inspection, root)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	seedDestinations := make([]string, 0)
	adapter := fakeHarnessAdapter{seedDestinations: &seedDestinations}
	service, err := NewHarnessService(lifecycle, repository, adapter)
	if err != nil {
		t.Fatal(err)
	}
	service.agentImageReference = fixtureAgentImageReference
	result, err := service.Run(context.Background(), HarnessRunRequest{
		Root: root, ApproveConfig: hash, Agent: "codex", Prompt: "one argument; not a shell", Stdout: os.Stdout,
		MCPServers: []harness.MCPServer{{Name: "browser", URL: "http://browser:8931/mcp"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Exit.Code == nil || *result.Exit.Code != 0 || result.Version != "rust-v0.147.0" || result.AuthPromotion.Digest == "" {
		t.Fatalf("Run() = %#v", result)
	}
	calls := strings.Join(fakeRuntime.calls, "\n")
	if !strings.Contains(calls, "exec:/usr/local/libexec/dsx/dsx-guest exec -- /usr/local/bin/fake --mcp ") || !strings.Contains(calls, " run one argument; not a shell") {
		t.Fatalf("exact one-shot invocation absent:\n%s", calls)
	}
	if !strings.Contains(calls, " export-file --kind auth ") || !strings.Contains(calls, "/config/mcp.json") || strings.Contains(calls, "copy:") {
		t.Fatalf("bounded auth export or MCP staging absent:\n%s", calls)
	}
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	scopedSuffix := filepath.Join("sandboxes", string(projectID), "main", "codex", "default")
	foundScopedRun := false
	for _, destination := range seedDestinations {
		if strings.Contains(destination, string(filepath.Separator)+"runs"+string(filepath.Separator)) && strings.HasSuffix(destination, scopedSuffix) {
			foundScopedRun = true
			break
		}
	}
	if !foundScopedRun {
		t.Fatalf("live global authentication copy was not sandbox-addressable: %#v", seedDestinations)
	}
	copy, err := repository.Prepare(context.Background(), auth.Profile{Harness: harness.Codex, Name: "default"}, model.RunID("00000000-0000-7000-8000-000000000099"), fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := os.ReadFile(filepath.Join(copy.Root, "auth.json"))
	if err != nil || !strings.Contains(string(credential), "refreshed") {
		t.Fatalf("promoted credential = %q, %v", credential, err)
	}
}

func TestHarnessServiceStagesRedactedEnvironmentOutsideHostInvocation(t *testing.T) {
	lifecycle, root, _, fakeRuntime, _ := lifecycleFixture(t)
	writeHarnessAuthConfig(t, root, "sandbox")
	hash := inspectLifecycleHash(t, lifecycle.inspection, root)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	service.agentImageReference = fixtureAgentImageReference
	secret := `{"headers":{"Authorization":"Bearer open-code-secret"}}`
	result, err := service.Run(context.Background(), HarnessRunRequest{
		Root: root, ApproveConfig: hash, Agent: "codex", Prompt: "safe prompt",
		Environment: map[string]string{"OPENCODE_CONFIG_CONTENT": secret, "ORDINARY": "visible"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Exit.Code == nil || *result.Exit.Code != 0 {
		t.Fatalf("Run() exit = %#v", result.Exit)
	}
	var staged runtime.ExecSpec
	for _, spec := range fakeRuntime.execSpecs {
		if len(spec.Argv) == 4 && spec.Argv[1] == "stage-env" && spec.Argv[2] == "--path" {
			staged = spec
			break
		}
	}
	if len(staged.Argv) != 4 {
		t.Fatalf("guest secret staging spec absent: %#v", fakeRuntime.execSpecs)
	}
	expectedContents := []byte("OPENCODE_CONFIG_CONTENT=" + secret + "\x00")
	foundContents := false
	for _, input := range fakeRuntime.execInputs {
		if bytes.Equal(input, expectedContents) {
			foundContents = true
			break
		}
	}
	if !foundContents {
		t.Fatalf("staged secret environment absent from bounded guest stdin: %#v", fakeRuntime.execInputs)
	}
	guestDestination := staged.Argv[3]
	for _, copy := range fakeRuntime.copies {
		if strings.Contains(copy.destination, "/env-") || bytes.Contains(copy.contents, []byte(secret)) {
			t.Fatalf("secret environment used host copy staging: %#v", copy)
		}
	}
	var invocation runtime.ExecSpec
	for _, spec := range fakeRuntime.execSpecs {
		if strings.Contains(strings.Join(spec.Argv, "\x00"), "\x00--env-file\x00") {
			invocation = spec
			break
		}
	}
	joinedArgv := strings.Join(invocation.Argv, "\x00")
	joinedEnv := strings.Join(invocation.Env, "\x00")
	if !strings.Contains(joinedArgv, "\x00--env-file\x00"+guestDestination+"\x00--\x00") {
		t.Fatalf("invocation argv = %#v", invocation.Argv)
	}
	if strings.Contains(joinedArgv, secret) || strings.Contains(joinedEnv, secret) || strings.Contains(joinedEnv, "OPENCODE_CONFIG_CONTENT=") {
		t.Fatalf("secret entered runtime invocation: argv=%#v env=%#v", invocation.Argv, invocation.Env)
	}
	if !strings.Contains(joinedEnv, "ORDINARY=visible") {
		t.Fatalf("ordinary environment missing: %#v", invocation.Env)
	}
	for _, spec := range append(append([]runtime.ExecSpec(nil), fakeRuntime.execSpecs...), fakeRuntime.preparedSpecs...) {
		if strings.Contains(strings.Join(spec.Argv, "\x00"), secret) || strings.Contains(strings.Join(spec.Env, "\x00"), secret) {
			t.Fatalf("secret entered host argv/env: %#v", spec)
		}
	}
	if !strings.Contains(strings.Join(fakeRuntime.calls, "\n"), "exec:/usr/local/libexec/dsx/dsx-guest exec -- /bin/rm -rf -- "+guestDestination) {
		t.Fatalf("guest cleanup absent: %#v", fakeRuntime.calls)
	}
}

func TestHarnessServiceInteractiveUsesPreparedChildAndPropagatesExit(t *testing.T) {
	lifecycle, root, _, _, _ := lifecycleFixture(t)
	writeHarnessAuthConfig(t, root, "global")
	hash := inspectLifecycleHash(t, lifecycle.inspection, root)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	service.agentImageReference = fixtureAgentImageReference
	called := false
	secret := "interactive-header-secret"
	result, err := service.Run(context.Background(), HarnessRunRequest{
		Root: root, ApproveConfig: hash, Agent: "codex", Interactive: true, Environment: map[string]string{"FAKE_TOKEN": secret, "TERM": "xterm-256color"},
		RunInteractive: func(_ context.Context, child InteractiveChild) (runtime.Exit, error) {
			called = true
			joined := strings.Join(child.Argv, "\x00")
			if !strings.Contains(joined, "\x00exec\x00--env-file\x00/tmp/dsx-run/") || !strings.Contains(joined, "\x00--\x00/usr/local/bin/fake\x00interactive") {
				t.Fatalf("interactive child = %q", joined)
			}
			if strings.Contains(joined, secret) || strings.Contains(strings.Join(child.Env, "\x00"), secret) {
				t.Fatalf("secret entered interactive host child: %#v", child)
			}
			code := 23
			return runtime.Exit{Code: &code}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called || result.Exit.Code == nil || *result.Exit.Code != 23 {
		t.Fatalf("interactive result = %#v called=%v", result, called)
	}
}

func TestHarnessServiceRefusesUndeclaredAuthBeforeRuntimeMutation(t *testing.T) {
	lifecycle, root, _, fakeRuntime, hash := lifecycleFixture(t)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), HarnessRunRequest{
		Root: root, ApproveConfig: hash, Agent: "codex", Profile: "undeclared",
	})
	if model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("Run() error = %v (code %q), want unapproved", err, model.ErrorCodeOf(err))
	}
	if len(fakeRuntime.calls) != 0 {
		t.Fatalf("undeclared auth grant reached runtime: %#v", fakeRuntime.calls)
	}
}

func TestHarnessServiceSandboxAuthPersistsWithoutGlobalProfile(t *testing.T) {
	lifecycle, root, _, _, _ := lifecycleFixture(t)
	writeHarnessAuthConfig(t, root, "sandbox")
	hash := inspectLifecycleHash(t, lifecycle.inspection, root)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	service.agentImageReference = fixtureAgentImageReference
	result, err := service.Run(context.Background(), HarnessRunRequest{
		Root: root, ApproveConfig: hash, Agent: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AuthPromotion.Digest == "" || result.AuthPromotion.Conflict {
		t.Fatalf("sandbox auth was not durably promoted: %#v", result.AuthPromotion)
	}
	if _, err := repository.Prepare(context.Background(), auth.Profile{Harness: harness.Codex, Name: "default"}, model.RunID("00000000-0000-7000-8000-000000000097"), fakeHarnessAdapter{}); err == nil {
		t.Fatal("sandbox run created a global authentication seed")
	}
}

func TestHarnessServiceAllocatesInvocationIDSeparateFromWorkspaceOwnership(t *testing.T) {
	lifecycle, root, _, fakeRuntime, _ := lifecycleFixture(t)
	writeHarnessAuthConfig(t, root, "global")
	hash := inspectLifecycleHash(t, lifecycle.inspection, root)
	ids := []model.RunID{
		"01890f5c-7b00-7000-8000-000000000031",
		"01890f5c-7b00-7000-8000-000000000032",
	}
	index := 0
	lifecycle.newRunID = func(time.Time) (model.RunID, error) {
		value := ids[index]
		index++
		return value, nil
	}
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	service.agentImageReference = fixtureAgentImageReference
	if _, err := service.Run(context.Background(), HarnessRunRequest{Root: root, ApproveConfig: hash, Agent: "codex"}); err != nil {
		t.Fatal(err)
	}
	if index != 2 {
		t.Fatalf("run ID allocations = %d, want workspace plus invocation", index)
	}
	invocationRoot := "/tmp/dsx-run/" + string(ids[1])
	for _, spec := range fakeRuntime.execSpecs {
		if len(spec.Argv) >= 4 && spec.Argv[1] == "ensure-dir" && strings.HasPrefix(spec.Argv[3], invocationRoot+"/") {
			return
		}
	}
	t.Fatalf("guest harness roots did not use fresh invocation ID: %#v", fakeRuntime.execSpecs)
}

func TestHarnessServiceInvalidRefreshDoesNotPromote(t *testing.T) {
	lifecycle, root, _, _, _ := lifecycleFixture(t)
	writeHarnessAuthConfig(t, root, "global")
	hash := inspectLifecycleHash(t, lifecycle.inspection, root)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	adapter := rejectingHarnessAdapter{}
	if _, err := repository.Ensure(context.Background(), auth.Profile{Harness: harness.Codex, Name: "default"}, adapter); err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, adapter)
	if err != nil {
		t.Fatal(err)
	}
	service.agentImageReference = fixtureAgentImageReference
	result, err := service.Run(context.Background(), HarnessRunRequest{
		Root: root, ApproveConfig: hash, Agent: "codex",
	})
	if err == nil {
		t.Fatal("invalid refreshed credentials were accepted")
	}
	if result.AuthPromotion != (auth.Promotion{}) {
		t.Fatalf("invalid refresh was promoted: %#v", result.AuthPromotion)
	}
	copy, err := repository.Prepare(context.Background(), auth.Profile{Harness: harness.Codex, Name: "default"}, model.RunID("00000000-0000-7000-8000-000000000098"), adapter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(copy.Root, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid refresh mutated global profile: %v", err)
	}
}

func TestHarnessServiceSelectedAgentRequiresItsApprovedHash(t *testing.T) {
	lifecycle, root, _, fakeRuntime, _ := lifecycleFixture(t)
	writeLifecycleConfig(t, root, `,"agents":{"default":"claude","allowed":["claude","codex"]}`)
	configuredAgentHash := inspectLifecycleHash(t, lifecycle.inspection, root)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), HarnessRunRequest{
		Root: root, ApproveConfig: configuredAgentHash, Agent: "codex", Prompt: "must not run",
	})
	if model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("Run() error = %v (code %q), want unapproved", err, model.ErrorCodeOf(err))
	}
	if len(fakeRuntime.calls) != 0 {
		t.Fatalf("stale configured-agent hash reached runtime: %#v", fakeRuntime.calls)
	}
}

func TestManagedStandardImageSatisfiesHarnessAttestation(t *testing.T) {
	service := &HarnessService{}
	execution := plan.ExecutionPlan{Image: plan.ResolvedImage{
		Context:     "@dsx/standard",
		File:        agentimage.BuildFile,
		InputDigest: agentimage.InputDigest(),
		Standard:    true,
	}}
	lock, err := os.ReadFile("../../images/agent/harnesses.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	err = service.verifyHarnessBuildAttestation(
		context.Background(),
		runtime.ResourceSnapshot{ImageDigest: "sha256:" + strings.Repeat("b", 64)},
		execution,
		fakeHarnessAdapter{},
		func(stdout, _ io.Writer) (runtime.Exit, error) {
			if _, writeErr := stdout.Write(lock); writeErr != nil {
				return runtime.Exit{}, writeErr
			}
			code := 0
			return runtime.Exit{Code: &code}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestHarnessBuildAttestationRejectsWrongRuntimeImageBeforeReadingGuestFile(t *testing.T) {
	service := &HarnessService{agentImageReference: fixtureAgentImageReference}
	execution := plan.ExecutionPlan{Image: plan.ResolvedImage{
		Reference:   fixtureAgentImageReference,
		InputDigest: strings.Repeat("a", 64),
	}}
	readCalled := false
	err := service.verifyHarnessBuildAttestation(
		context.Background(),
		runtime.ResourceSnapshot{ImageDigest: "sha256:" + strings.Repeat("b", 64)},
		execution,
		fakeHarnessAdapter{},
		func(io.Writer, io.Writer) (runtime.Exit, error) {
			readCalled = true
			code := 0
			return runtime.Exit{Code: &code}, nil
		},
	)
	if model.ErrorCodeOf(err) != model.CodeUnavailable || readCalled {
		t.Fatalf("verification error = %v, readCalled = %t", err, readCalled)
	}
}

func TestHarnessServiceRejectsWrongApprovedImageBeforeHarnessOrAuthMutation(t *testing.T) {
	lifecycle, root, _, fakeRuntime, _ := lifecycleFixture(t)
	writeHarnessAuthConfig(t, root, "global")
	hash := inspectLifecycleHash(t, lifecycle.inspection, root)
	authRoot := filepath.Join(t.TempDir(), "auth")
	repository, err := auth.NewRepository(authRoot)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	service.agentImageReference = "ghcr.io/dsx/standard@sha256:" + strings.Repeat("b", 64)
	if _, err := service.Run(context.Background(), HarnessRunRequest{Root: root, ApproveConfig: hash, Agent: "codex"}); model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("Run() error = %v (code %q), want unapproved image", err, model.ErrorCodeOf(err))
	}
	assertNoHarnessOrAuthMutation(t, fakeRuntime, authRoot)
}

func TestHarnessServiceRejectsUntrustedBuildAttestationBeforeHarnessOrAuthMutation(t *testing.T) {
	tests := []struct {
		name        string
		attestation []byte
		exitCode    int
	}{
		{name: "spoofed attestation", attestation: []byte(`{\"schemaVersion\":1,\"platform\":\"linux/arm64\"}`)},
		{name: "missing file", exitCode: 1},
		{name: "oversized output", attestation: make([]byte, harness.MaxBuildAttestationBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lifecycle, root, _, fakeRuntime, _ := lifecycleFixture(t)
			writeHarnessAuthConfig(t, root, "global")
			hash := inspectLifecycleHash(t, lifecycle.inspection, root)
			authRoot := filepath.Join(t.TempDir(), "auth")
			repository, err := auth.NewRepository(authRoot)
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
			if err != nil {
				t.Fatal(err)
			}
			service.agentImageReference = fixtureAgentImageReference
			fakeRuntime.execOutput = func(spec runtime.ExecSpec) ([]byte, []byte) {
				if isHarnessAttestationRead(spec) {
					if test.exitCode != 0 {
						return nil, []byte("unavailable")
					}
					return test.attestation, nil
				}
				return []byte("shell-ok\n"), nil
			}
			if test.exitCode != 0 {
				code := test.exitCode
				fakeRuntime.execExit.Code = &code
			}
			if _, err := service.Run(context.Background(), HarnessRunRequest{Root: root, ApproveConfig: hash, Agent: "codex"}); model.ErrorCodeOf(err) != model.CodeUnavailable {
				t.Fatalf("Run() error = %v (code %q), want unavailable attestation", err, model.ErrorCodeOf(err))
			}
			assertNoHarnessOrAuthMutation(t, fakeRuntime, authRoot)
		})
	}
}

func assertNoHarnessOrAuthMutation(t *testing.T, fakeRuntime *lifecycleRuntime, authRoot string) {
	t.Helper()
	calls := strings.Join(fakeRuntime.calls, "\n")
	for _, forbidden := range []string{"ensure-dir", "copy:", "codex --version", "/usr/local/bin/fake"} {
		if strings.Contains(calls, forbidden) {
			t.Fatalf("attestation failure reached harness/auth mutation %q: %s", forbidden, calls)
		}
	}
	entries, err := os.ReadDir(authRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("attestation failure mutated host auth repository: %#v", entries)
	}
}

func TestGuestHarnessDirectoriesUseDescriptorHelperAndUnsafePathsRefused(t *testing.T) {
	lifecycle, _, _, fakeRuntime, _ := lifecycleFixture(t)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	directory := guestHarnessRoot + "/01890f5c-7b00-7000-8000-000000000001/auth/provider"
	if err := service.mkdirGuest(context.Background(), runtime.ResourceSnapshot{}, directory); err != nil {
		t.Fatal(err)
	}
	if len(fakeRuntime.execSpecs) != 1 {
		t.Fatalf("guest directory operations = %#v", fakeRuntime.calls)
	}
	spec := fakeRuntime.execSpecs[0]
	want := []string{DefaultGuestHelperPath, "ensure-dir", "--path", directory}
	if !reflect.DeepEqual(spec.Argv, want) || spec.User != "1000:1000" || len(spec.Env) != 0 {
		t.Fatalf("safe directory spec = %#v, want argv %#v", spec, want)
	}
	joined := strings.Join(fakeRuntime.calls, "\n")
	if strings.Contains(joined, "/usr/bin/find") || strings.Contains(joined, "/bin/mkdir") || strings.Contains(joined, "-exec") {
		t.Fatalf("legacy mkdir/find validation remains: %s", joined)
	}

	unsafeCode := 1
	fakeRuntime.execExit = runtime.Exit{Code: &unsafeCode}
	if err := service.mkdirGuest(context.Background(), runtime.ResourceSnapshot{}, directory); err == nil {
		t.Fatal("unsafe directory helper failure was accepted")
	}
	if _, err := guestDirectoryChain(guestHarnessRoot + "/run/../../outside"); err == nil {
		t.Fatal("guest directory traversal was accepted")
	}
}

func TestCredentialStagingUsesBoundedStdinWithoutRepositoryOrSecretDisclosure(t *testing.T) {
	lifecycle, _, _, fakeRuntime, _ := lifecycleFixture(t)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth-repository"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	source := filepath.Join(sourceRoot, "auth.json")
	secret := []byte(`{"token":"must-not-leak"}`)
	if err := os.WriteFile(source, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o600); err != nil {
		t.Fatal(err)
	}
	runRoot := guestHarnessRoot + "/01890f5c-7b00-7000-8000-000000000001"
	layout := harness.AuthLayout{CredentialArtifacts: []string{"auth.json"}, MaxArtifactBytes: 1 << 20}
	if err := service.copyAuthToGuest(context.Background(), runtime.ResourceSnapshot{}, sourceRoot, runRoot+"/auth", layout); err != nil {
		t.Fatal(err)
	}
	if len(fakeRuntime.execInputs) != 1 || !bytes.Equal(fakeRuntime.execInputs[0], secret) {
		t.Fatalf("credential stdin staging = %#v", fakeRuntime.execInputs)
	}
	joined := strings.Join(fakeRuntime.calls, "\n")
	if strings.Contains(joined, "copy:") || strings.Contains(joined, sourceRoot) || strings.Contains(joined, string(secret)) {
		t.Fatalf("credential leaked to copy/repository path/argv: %s", joined)
	}
	stage := fakeRuntime.execSpecs[len(fakeRuntime.execSpecs)-1]
	wantPath := runRoot + "/auth/auth.json"
	if !reflect.DeepEqual(stage.Argv, []string{DefaultGuestHelperPath, "stage-file", "--max-bytes", "1048576", "--path", wantPath}) ||
		len(stage.Env) != 0 || stage.User != "1000:1000" {
		t.Fatalf("credential staging spec = %#v", stage)
	}
}

func TestReadOnlyConfigStagingUsesExactRootHelperAuthority(t *testing.T) {
	lifecycle, _, _, fakeRuntime, _ := lifecycleFixture(t)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	destinationRoot := "/tmp/dsx-readonly/01890f5c-7b00-7000-8000-000000000001"
	destination := destinationRoot + "/settings.json"
	config := []byte(`{"permissions":{"deny":["*"]}}`)
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "settings.json"), config, 0o400); err != nil {
		t.Fatal(err)
	}
	staged, err := service.copyReadOnlyConfigToGuest(context.Background(), runtime.ResourceSnapshot{}, sourceRoot, destinationRoot, harness.AuthLayout{ReadOnlyConfig: []string{"settings.json"}, MaxArtifactBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(staged, []string{destination}) {
		t.Fatalf("staged reviewed config paths = %#v", staged)
	}
	if len(fakeRuntime.execInputs) != 1 || !bytes.Equal(fakeRuntime.execInputs[0], config) {
		t.Fatalf("read-only config stdin = %#v", fakeRuntime.execInputs)
	}
	spec := fakeRuntime.execSpecs[0]
	want := []string{
		DefaultGuestHelperPath, "stage-file", "--read-only",
		"--child-uid", "1000", "--child-gid", "1000", "--max-bytes", "1048576", "--path", destination,
	}
	if !reflect.DeepEqual(spec.Argv, want) || spec.User != "0:0" || spec.WorkingDir != "/workspace" || len(spec.Env) != 0 {
		t.Fatalf("read-only staging authority = %#v, want argv %#v", spec, want)
	}
	if strings.Contains(strings.Join(spec.Argv, "\x00"), string(config)) ||
		strings.Contains(strings.Join(spec.Argv, "\x00"), sourceRoot) ||
		strings.Contains(strings.Join(spec.Env, "\x00"), string(config)) {
		t.Fatal("read-only configuration or repository path leaked into argv/env")
	}
	if err := service.removeReadOnlyGuestRoot(context.Background(), runtime.ResourceSnapshot{}, destinationRoot); err != nil {
		t.Fatal(err)
	}
	cleanup := fakeRuntime.execSpecs[1]
	cleanupWant := []string{DefaultGuestHelperPath, "remove-read-only", "--path", destinationRoot}
	if !reflect.DeepEqual(cleanup.Argv, cleanupWant) || cleanup.User != "0:0" || cleanup.WorkingDir != "/workspace" || len(cleanup.Env) != 0 {
		t.Fatalf("read-only cleanup authority = %#v, want argv %#v", cleanup, cleanupWant)
	}
}
func TestPartialReadOnlyConfigStagingCleansRootOwnedTree(t *testing.T) {
	lifecycle, _, _, fakeRuntime, _ := lifecycleFixture(t)
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHarnessService(lifecycle, repository, fakeHarnessAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "first.json"), []byte("first"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "invalid.json"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := "/tmp/dsx-readonly/01890f5c-7b00-7000-8000-000000000001"
	_, err = service.copyReadOnlyConfigToGuest(context.Background(), runtime.ResourceSnapshot{}, sourceRoot, root, harness.AuthLayout{ReadOnlyConfig: []string{"first.json", "invalid.json"}, MaxArtifactBytes: 1 << 20})
	if err == nil {
		t.Fatal("unsafe second reviewed config was accepted")
	}
	if len(fakeRuntime.execSpecs) != 2 {
		t.Fatalf("partial staging operations = %#v", fakeRuntime.execSpecs)
	}
	cleanup := fakeRuntime.execSpecs[1]
	if !reflect.DeepEqual(cleanup.Argv, []string{DefaultGuestHelperPath, "remove-read-only", "--path", root}) || cleanup.User != "0:0" {
		t.Fatalf("partial staging cleanup = %#v", cleanup)
	}
}

func writeHarnessAuthConfig(t *testing.T, root, persistence string) {
	t.Helper()
	writeLifecycleConfig(t, root, `,"agents":{"default":"codex","allowed":["codex"]},"authProfiles":{"default":{"harness":"codex","persistence":"`+persistence+`"}}`)
}

func TestGuestExportStreamsToExclusiveBoundedFileAndPreservesOptionalAbsence(t *testing.T) {
	lifecycle, _, _, fakeRuntime, _ := lifecycleFixture(t)
	service := &HarnessService{lifecycle: lifecycle}
	destination := filepath.Join(t.TempDir(), "auth.json")
	present, err := service.exportGuestFile(
		context.Background(),
		runtime.ResourceSnapshot{},
		"/tmp/dsx-run/01890f5c-7b00-7000-8000-000000000001/auth/auth.json",
		destination,
		"auth",
		1<<20,
	)
	if err != nil || !present {
		t.Fatalf("export = present %t, error %v", present, err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil || string(contents) != `{"token":"refreshed"}` {
		t.Fatalf("exported credential = %q, %v", contents, err)
	}
	info, err := os.Lstat(destination)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("exported credential metadata = %v, %v", info, err)
	}
	command := strings.Join(fakeRuntime.execSpecs[len(fakeRuntime.execSpecs)-1].Argv, " ")
	if !strings.Contains(command, " export-file --kind auth --max-bytes 1048576 ") {
		t.Fatalf("export command = %q", command)
	}

	missingCode := 4
	fakeRuntime.execExit = runtime.Exit{Code: &missingCode}
	missingDestination := filepath.Join(t.TempDir(), "missing.json")
	present, err = service.exportGuestFile(
		context.Background(),
		runtime.ResourceSnapshot{},
		"/tmp/dsx-run/01890f5c-7b00-7000-8000-000000000001/auth/missing.json",
		missingDestination,
		"auth",
		1<<20,
	)
	if err != nil || present {
		t.Fatalf("optional export = present %t, error %v", present, err)
	}
	if _, err := os.Lstat(missingDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing export left destination: %v", err)
	}
}
