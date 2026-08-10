package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/harness/catalog"
	"github.com/srimajji/dsx/internal/harness/omp"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

type loginAdapter struct {
	name             harness.Name
	artifact         harness.PinnedArtifact
	flow             func(harness.LoginRequest) harness.LoginFlow
	mcp              func(harness.MCPRequest) (harness.ConfigInjection, error)
	seedDestinations *[]string
	observe          func(string)
}

func (adapter loginAdapter) Name() harness.Name { return adapter.name }
func (adapter loginAdapter) Version() harness.PinnedArtifact {
	return adapter.artifact
}
func (adapter loginAdapter) ValidateVersion(stdout, stderr string) error {
	if adapter.observe != nil {
		adapter.observe("version-validated")
	}
	if !strings.Contains(stdout+stderr, "shell-ok") {
		return errors.New("unexpected version")
	}
	return nil
}
func (adapter loginAdapter) Preflight(_ context.Context, roots harness.RunRoots) ([]harness.Diagnostic, error) {
	return nil, harness.ValidateRoots(roots)
}
func (adapter loginAdapter) Invocation(request harness.InvocationRequest) (harness.ExecSpec, error) {
	return harness.ExecSpec{Argv: []string{"/usr/local/bin/fake", "run"}, Cwd: request.Roots.Workspace}, nil
}
func (loginAdapter) AuthLayout() harness.AuthLayout {
	return harness.AuthLayout{Backend: harness.StorageFile, CredentialArtifacts: []string{"auth.json"}, MaxArtifactBytes: 1 << 20, Environment: map[string]string{"FAKE_AUTH": "."}}
}
func (adapter loginAdapter) Seed(ctx context.Context, request harness.SeedRequest) error {
	if adapter.observe != nil {
		adapter.observe("auth-seed")
	}
	if adapter.seedDestinations != nil {
		*adapter.seedDestinations = append(*adapter.seedDestinations, request.SourceRoot, request.DestinationRoot)
	}
	return harness.SeedArtifacts(ctx, request)
}
func (adapter loginAdapter) EphemeralMCP(request harness.MCPRequest) (harness.ConfigInjection, error) {
	if adapter.mcp != nil {
		return adapter.mcp(request)
	}
	return harness.ConfigInjection{}, nil
}
func (adapter loginAdapter) Login(request harness.LoginRequest) (harness.LoginFlow, error) {
	if adapter.observe != nil {
		adapter.observe("login-flow")
	}
	if adapter.flow != nil {
		return adapter.flow(request), nil
	}
	return harness.LoginFlow{Exec: harness.ExecSpec{
		Argv: []string{"/usr/local/bin/fake", string(adapter.name), "login"},
		Env:  map[string]string{"FAKE_AUTH": request.Roots.Auth, "FLOW": string(adapter.name)},
		Cwd:  request.Roots.Workspace, Terminal: true,
	}}, nil
}
func (loginAdapter) RedactionRules() harness.RedactionRules { return harness.RedactionRules{} }

func loginFixture(t *testing.T, name harness.Name, persistence string, flow func(harness.LoginRequest) harness.LoginFlow) (*HarnessService, *auth.Repository, *lifecycleRuntime, string, string) {
	t.Helper()
	service, repository, fakeRuntime, root, hash, _ := loginFixtureWithAuthRoot(t, name, persistence, flow)
	return service, repository, fakeRuntime, root, hash
}

func loginFixtureWithAuthRoot(t *testing.T, name harness.Name, persistence string, flow func(harness.LoginRequest) harness.LoginFlow) (*HarnessService, *auth.Repository, *lifecycleRuntime, string, string, string) {
	t.Helper()
	lifecycle, root, _, fakeRuntime, _ := lifecycleFixture(t)
	writeLifecycleConfig(t, root, `,"agents":{"default":"`+string(name)+`","allowed":["`+string(name)+`"]},"authProfiles":{"work":{"harness":"`+string(name)+`","persistence":"`+persistence+`"}}`)
	hash := inspectLifecycleHash(t, lifecycle.inspection, root)
	authRoot := filepath.Join(t.TempDir(), "auth")
	repository, err := auth.NewRepository(authRoot)
	if err != nil {
		t.Fatal(err)
	}
	var artifact harness.PinnedArtifact
	for _, supported := range catalog.All() {
		if supported.Name() == name {
			artifact = supported.Version()
			break
		}
	}
	if artifact.Version == "" {
		t.Fatalf("fixture has no pinned artifact for %q", name)
	}
	service, err := NewHarnessService(lifecycle, repository, loginAdapter{name: name, artifact: artifact, flow: flow})
	if err != nil {
		t.Fatal(err)
	}
	service.agentImageReference = fixtureAgentImageReference
	return service, repository, fakeRuntime, root, hash, authRoot
}

func interactiveLoginRequest(root, hash string, name harness.Name, runner InteractiveChildRunner) HarnessLoginRequest {
	return HarnessLoginRequest{
		Root: root, ApproveConfig: hash, Agent: string(name), Profile: "work", Interactive: true,
		Stdin: bytes.NewReader(nil), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, RunInteractive: runner,
	}
}

func TestHarnessLoginRejectsUntrustedBuildAttestationBeforeAuthOrProviderMutation(t *testing.T) {
	tests := []struct {
		name        string
		attestation []byte
		exitCode    int
	}{
		{name: "spoofed attestation", attestation: []byte(`{"schemaVersion":1,"platform":"linux/arm64","harnesses":[]}`)},
		{name: "missing attestation", exitCode: 1},
		{name: "oversized attestation", attestation: make([]byte, harness.MaxBuildAttestationBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, _, fakeRuntime, root, hash, authRoot := loginFixtureWithAuthRoot(t, harness.Codex, "global", nil)
			events := make([]string, 0, 2)
			adapter := service.adapters[harness.Codex].(loginAdapter)
			adapter.observe = func(event string) { events = append(events, event) }
			service.adapters[harness.Codex] = adapter
			fakeRuntime.execOutput = func(spec runtime.ExecSpec) ([]byte, []byte) {
				if isHarnessAttestationRead(spec) {
					events = append(events, "attestation-read")
					if test.exitCode != 0 {
						return nil, []byte("unavailable")
					}
					return test.attestation, nil
				}
				events = append(events, "unexpected-guest-or-provider-exec")
				return []byte("shell-ok\n"), nil
			}
			if test.exitCode != 0 {
				code := test.exitCode
				fakeRuntime.execExit.Code = &code
			}
			request := interactiveLoginRequest(root, hash, harness.Codex, func(context.Context, InteractiveChild) (runtime.Exit, error) {
				events = append(events, "provider-login")
				code := 0
				return runtime.Exit{Code: &code}, nil
			})
			if _, err := service.Login(context.Background(), request); model.ErrorCodeOf(err) != model.CodeUnavailable {
				t.Fatalf("Login() error = %v (code %q), want unavailable attestation", err, model.ErrorCodeOf(err))
			}
			if got := strings.Join(events, ","); got != "attestation-read" {
				t.Fatalf("attestation failure ordering = %q, want only attestation read", got)
			}
			assertNoHarnessOrAuthMutation(t, fakeRuntime, authRoot)
		})
	}
}

func TestHarnessLoginAttestsBeforeAuthGuestVersionAndProviderExecution(t *testing.T) {
	service, _, fakeRuntime, root, hash := loginFixture(t, harness.Codex, "global", nil)
	events := make([]string, 0, 8)
	providerStarted := false
	adapter := service.adapters[harness.Codex].(loginAdapter)
	adapter.observe = func(event string) {
		if !providerStarted {
			events = append(events, event)
		}
	}
	service.adapters[harness.Codex] = adapter
	guestMutationObserved := false
	fakeRuntime.execOutput = func(spec runtime.ExecSpec) ([]byte, []byte) {
		switch {
		case isHarnessAttestationRead(spec):
			events = append(events, "attestation-read")
			attestation, err := os.ReadFile("../../images/agent/harnesses.lock.json")
			if err != nil {
				t.Fatal(err)
			}
			return attestation, nil
		case hasLoginArgvSuffix(spec.Argv, adapter.Version().Executable, "--version"):
			events = append(events, "version-exec")
		case len(spec.Argv) >= 2 && spec.Argv[1] == "ensure-dir" && !guestMutationObserved:
			guestMutationObserved = true
			events = append(events, "guest-auth-mutation")
		case strings.Contains(strings.Join(spec.Argv, " "), " export-file --kind auth "):
			return []byte(`{"token":"refreshed"}`), nil
		}
		return []byte("shell-ok\n"), nil
	}
	request := interactiveLoginRequest(root, hash, harness.Codex, func(context.Context, InteractiveChild) (runtime.Exit, error) {
		providerStarted = true
		events = append(events, "provider-login")
		code := 0
		return runtime.Exit{Code: &code}, nil
	})
	if _, err := service.Login(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"attestation-read",
		"auth-seed",
		"auth-seed",
		"guest-auth-mutation",
		"login-flow",
		"version-exec",
		"version-validated",
		"provider-login",
	}
	if got, expected := strings.Join(events, ","), strings.Join(want, ","); got != expected {
		t.Fatalf("Login() ordering = %q, want %q", got, expected)
	}
}

func TestHarnessLoginOMPInstallsEmptyMCPIsolationBeforeCredentialsAndProvider(t *testing.T) {
	pinnedOMP := omp.New()
	service, repository, fakeRuntime, root, hash := loginFixture(t, harness.OMP, "global", func(request harness.LoginRequest) harness.LoginFlow {
		flow, err := pinnedOMP.Login(request)
		if err != nil {
			t.Fatal(err)
		}
		return flow
	})
	maliciousMarker := "must-not-run-project-mcp"
	projectConfigRoot := filepath.Join(root, ".omp")
	if err := os.MkdirAll(projectConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectConfigRoot, "config.yml"), []byte("mcp:\n  servers:\n    hostile:\n      command: "+maliciousMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seed := t.TempDir()
	if err := os.WriteFile(filepath.Join(seed, "auth.json"), []byte(`{"token":"seed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := service.adapters[harness.OMP].(loginAdapter)
	if _, err := repository.Import(context.Background(), auth.Profile{Harness: harness.OMP, Name: "work"}, seed, adapter); err != nil {
		t.Fatal(err)
	}
	adapter.mcp = pinnedOMP.EphemeralMCP
	service.adapters[harness.OMP] = adapter

	events := make([]string, 0, 6)
	fakeRuntime.execOutput = func(spec runtime.ExecSpec) ([]byte, []byte) {
		joined := strings.Join(spec.Argv, " ")
		switch {
		case isHarnessAttestationRead(spec):
			events = append(events, "attestation-read")
			attestation, err := os.ReadFile("../../images/agent/harnesses.lock.json")
			if err != nil {
				t.Fatal(err)
			}
			return attestation, nil
		case strings.Contains(joined, " stage-file ") && strings.Contains(joined, "/auth/mcp.json"):
			events = append(events, "mcp-registry")
		case strings.Contains(joined, " stage-file ") && strings.Contains(joined, "/tmp/omp-ephemeral-mcp.yml"):
			events = append(events, "mcp-overlay")
		case strings.Contains(joined, " stage-file ") && strings.Contains(joined, "/auth/auth.json"):
			events = append(events, "auth-copy")
		case hasLoginArgvSuffix(spec.Argv, "omp", "--version"):
			events = append(events, "version-exec")
		}
		return []byte("shell-ok\n"), nil
	}
	request := interactiveLoginRequest(root, hash, harness.OMP, func(_ context.Context, child InteractiveChild) (runtime.Exit, error) {
		events = append(events, "provider-login")
		if len(child.Argv) < 3 {
			t.Fatalf("isolated OMP login argv = %#v", child.Argv)
		}
		loginArgv := child.Argv[len(child.Argv)-3:]
		if loginArgv[0] != "omp" || loginArgv[1] != "--config" ||
			!strings.HasSuffix(loginArgv[2], "/tmp/omp-ephemeral-mcp.yml") {
			t.Fatalf("isolated OMP login argv = %#v", child.Argv)
		}
		if strings.Contains(strings.Join(child.Argv, " "), maliciousMarker) {
			t.Fatalf("project MCP command reached OMP login argv: %#v", child.Argv)
		}
		code := 17
		return runtime.Exit{Code: &code}, nil
	})
	result, err := service.Login(context.Background(), request)
	if err != nil || result.Exit.Code == nil || *result.Exit.Code != 17 {
		t.Fatalf("Login() = %#v, %v", result, err)
	}
	want := "attestation-read,mcp-registry,mcp-overlay,auth-copy,version-exec,provider-login"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("OMP login isolation ordering = %q, want %q", got, want)
	}
	var sawEmptyRegistry, sawProjectDisable bool
	for _, input := range fakeRuntime.execInputs {
		switch string(input) {
		case "{\n  \"mcpServers\": {}\n}\n":
			sawEmptyRegistry = true
		case "mcp:\n  enableProjectConfig: false\n":
			sawProjectDisable = true
		}
	}
	if !sawEmptyRegistry || !sawProjectDisable {
		t.Fatalf("OMP login staged empty registry=%t project-disable=%t", sawEmptyRegistry, sawProjectDisable)
	}
}

func TestHarnessLoginConsumesAllAdapterFlowsAndPromotesGlobalSeed(t *testing.T) {
	for _, name := range []harness.Name{harness.OMP, harness.Codex, harness.Claude, harness.OpenCode} {
		t.Run(string(name), func(t *testing.T) {
			openBrowser := name == harness.Claude
			service, repository, fakeRuntime, root, hash := loginFixture(t, name, "global", func(request harness.LoginRequest) harness.LoginFlow {
				flow := harness.LoginFlow{Exec: harness.ExecSpec{
					Argv: []string{"/usr/local/bin/fake", string(name), "login", "one argument"},
					Env:  map[string]string{"FAKE_AUTH": request.Roots.Auth, "FLOW": string(name)},
					Cwd:  request.Roots.Workspace, Terminal: true,
				}, OpenBrowser: openBrowser}
				if openBrowser {
					flow.CallbackTimeout = 30
				}
				return flow
			})
			seedDestinations := make([]string, 0)
			observedAdapter := service.adapters[name].(loginAdapter)
			observedAdapter.seedDestinations = &seedDestinations
			service.adapters[name] = observedAdapter
			called, opened := false, 0
			request := interactiveLoginRequest(root, hash, name, func(_ context.Context, child InteractiveChild) (runtime.Exit, error) {
				called = true
				joined := strings.Join(child.Argv, "\x00")
				if !strings.Contains(joined, "/usr/local/bin/fake\x00"+string(name)+"\x00login\x00one argument") {
					t.Fatalf("login child argv = %#v", child.Argv)
				}
				if openBrowser {
					if _, err := io.WriteString(child.Stdout, "Open "+observedClaudeProviderURL(strings.Repeat("s", 43), strings.Repeat("c", 43))+" now\n"); err != nil {
						t.Fatal(err)
					}
				}
				code := 0
				return runtime.Exit{Code: &code}, nil
			})
			if openBrowser {
				request.OpenBrowser = func(_ context.Context, providerURL string) error {
					if !called {
						t.Fatal("browser opened before child startup")
					}
					if providerURL != testClaudeProviderURL(strings.Repeat("s", 43), strings.Repeat("c", 43)) {
						t.Fatalf("provider URL = %q", providerURL)
					}
					opened++
					return nil
				}
			}
			result, err := service.Login(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if !called || opened != boolCount(openBrowser) || result.Exit.Code == nil || *result.Exit.Code != 0 || result.AuthPromotion.Digest == "" {
				t.Fatalf("Login() = %#v, called=%t opened=%d", result, called, opened)
			}
			if len(fakeRuntime.preparedSpecs) != 1 || !containsLoginEnvironment(fakeRuntime.preparedSpecs[0].Env, "FLOW="+string(name)) {
				t.Fatalf("login flow environment was not consumed: %#v", fakeRuntime.preparedSpecs)
			}
			projectID, err := model.NewProjectID(root)
			if err != nil {
				t.Fatal(err)
			}
			scopedSuffix := filepath.Join("sandboxes", string(projectID), "main", string(name), "work")
			foundScopedRun := false
			for _, destination := range seedDestinations {
				if strings.Contains(destination, string(filepath.Separator)+"runs"+string(filepath.Separator)) && strings.HasSuffix(destination, scopedSuffix) {
					foundScopedRun = true
					break
				}
			}
			if !foundScopedRun {
				t.Fatalf("login global authentication copy was not sandbox-addressable: %#v", seedDestinations)
			}
			copy, err := repository.Prepare(context.Background(), auth.Profile{Harness: name, Name: "work"}, model.RunID("00000000-0000-7000-8000-000000000099"), loginAdapter{name: name})
			if err != nil {
				t.Fatal(err)
			}
			credential, err := os.ReadFile(filepath.Join(copy.Root, "auth.json"))
			if err != nil || !strings.Contains(string(credential), "refreshed") {
				t.Fatalf("promoted credential = %q, %v", credential, err)
			}
		})
	}
}

func TestHarnessLoginRejectsNoTTYStaleApprovalAndUnknownProfileBeforeRuntimeMutation(t *testing.T) {
	service, _, fakeRuntime, root, hash := loginFixture(t, harness.Codex, "global", nil)
	code := 0
	runner := func(context.Context, InteractiveChild) (runtime.Exit, error) { return runtime.Exit{Code: &code}, nil }
	requests := []HarnessLoginRequest{
		{Root: root, ApproveConfig: hash, Agent: "codex", Profile: "work"},
		interactiveLoginRequest(root, strings.Repeat("f", 64), harness.Codex, runner),
		interactiveLoginRequest(root, hash, harness.Codex, runner),
	}
	requests[2].Profile = "unknown"
	for _, request := range requests {
		if _, err := service.Login(context.Background(), request); err == nil {
			t.Fatalf("Login(%#v) unexpectedly succeeded", request)
		}
		if len(fakeRuntime.calls) != 0 || len(fakeRuntime.resources) != 0 {
			t.Fatalf("rejected login mutated runtime: calls=%#v resources=%#v", fakeRuntime.calls, fakeRuntime.resources)
		}
	}
}

func TestHarnessLoginSandboxPolicyPersistsOnlyScopedSeed(t *testing.T) {
	service, repository, _, root, hash := loginFixture(t, harness.OMP, "sandbox", nil)
	code := 0
	request := interactiveLoginRequest(root, hash, harness.OMP, func(context.Context, InteractiveChild) (runtime.Exit, error) {
		return runtime.Exit{Code: &code}, nil
	})
	result, err := service.Login(context.Background(), request)
	if err != nil || result.Exit.Code == nil || *result.Exit.Code != 0 || result.AuthPromotion.Digest == "" || result.AuthPromotion.Conflict {
		t.Fatalf("Login() = %#v, %v", result, err)
	}
	if _, err := repository.Prepare(context.Background(), auth.Profile{Harness: harness.OMP, Name: "work"}, model.RunID("00000000-0000-7000-8000-000000000096"), loginAdapter{name: harness.OMP}); err == nil {
		t.Fatal("sandbox login created a global authentication seed")
	}
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	profile := auth.SandboxProfile(auth.Profile{Harness: harness.OMP, Name: "work"}, projectID, "main")
	copy, err := repository.PrepareSandbox(context.Background(), profile, model.RunID("00000000-0000-7000-8000-000000000096"), loginAdapter{name: harness.OMP})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := os.ReadFile(filepath.Join(copy.Root, "auth.json"))
	if err != nil || !strings.Contains(string(credential), "refreshed") {
		t.Fatalf("later sandbox invocation credential = %q, %v", credential, err)
	}
}
func TestHarnessLoginAllocatesSessionIDSeparateFromWorkspaceOwnership(t *testing.T) {
	service, _, fakeRuntime, root, hash := loginFixture(t, harness.Codex, "global", nil)
	ids := []model.RunID{
		"01890f5c-7b00-7000-8000-000000000041",
		"01890f5c-7b00-7000-8000-000000000042",
	}
	index := 0
	service.lifecycle.newRunID = func(time.Time) (model.RunID, error) {
		value := ids[index]
		index++
		return value, nil
	}
	code := 0
	request := interactiveLoginRequest(root, hash, harness.Codex, func(context.Context, InteractiveChild) (runtime.Exit, error) {
		return runtime.Exit{Code: &code}, nil
	})
	if _, err := service.Login(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if index != 2 {
		t.Fatalf("run ID allocations = %d, want login session plus workspace ownership", index)
	}
	sessionRoot := "/tmp/dsx-run/" + string(ids[0])
	for _, spec := range fakeRuntime.execSpecs {
		if len(spec.Argv) >= 4 && spec.Argv[1] == "ensure-dir" && strings.HasPrefix(spec.Argv[3], sessionRoot+"/") {
			return
		}
	}
	t.Fatalf("login guest roots did not use fresh session ID: %#v", fakeRuntime.execSpecs)
}

func TestHarnessLoginFailureLeavesGlobalSeedAndAlwaysRemovesRunCopy(t *testing.T) {
	service, repository, fakeRuntime, root, hash := loginFixture(t, harness.Codex, "global", nil)
	adapter := loginAdapter{name: harness.Codex}
	seed := t.TempDir()
	if err := os.WriteFile(filepath.Join(seed, "auth.json"), []byte(`{"token":"seed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Import(context.Background(), auth.Profile{Harness: harness.Codex, Name: "work"}, seed, adapter); err != nil {
		t.Fatal(err)
	}
	code := 17
	request := interactiveLoginRequest(root, hash, harness.Codex, func(context.Context, InteractiveChild) (runtime.Exit, error) {
		return runtime.Exit{Code: &code}, nil
	})
	result, err := service.Login(context.Background(), request)
	if err != nil || result.Exit.Code == nil || *result.Exit.Code != code || result.AuthPromotion.Digest != "" {
		t.Fatalf("Login() = %#v, %v", result, err)
	}
	copy, err := repository.Prepare(context.Background(), auth.Profile{Harness: harness.Codex, Name: "work"}, model.RunID("00000000-0000-7000-8000-000000000099"), adapter)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := os.ReadFile(filepath.Join(copy.Root, "auth.json"))
	if err != nil || string(credential) != `{"token":"seed"}` {
		t.Fatalf("failed login changed seed = %q, %v", credential, err)
	}
	calls := strings.Join(fakeRuntime.calls, "\n")
	if !strings.Contains(calls, "/bin/rm -rf -- /tmp/dsx-run/") {
		t.Fatalf("guest login roots were not cleaned: %s", calls)
	}
}

func TestHarnessLoginOpenerFailureAndTimeoutCleanCopiesWithoutPromotion(t *testing.T) {
	flow := func(request harness.LoginRequest) harness.LoginFlow {
		return harness.LoginFlow{Exec: harness.ExecSpec{Argv: []string{"/usr/local/bin/fake", "login"}, Cwd: request.Roots.Workspace, Terminal: true}, OpenBrowser: true, CallbackTimeout: 1}
	}
	for _, test := range []struct {
		name   string
		opener LoginBrowserOpener
		runner InteractiveChildRunner
	}{
		{name: "opener", opener: func(context.Context, string) error { return errors.New("opener failed") }, runner: func(_ context.Context, child InteractiveChild) (runtime.Exit, error) {
			_, _ = io.WriteString(child.Stdout, observedClaudeProviderURL(strings.Repeat("s", 43), strings.Repeat("c", 43))+"\n")
			code := 0
			return runtime.Exit{Code: &code}, nil
		}},
		{name: "timeout", opener: func(context.Context, string) error { return nil }, runner: func(ctx context.Context, child InteractiveChild) (runtime.Exit, error) {
			_, _ = io.WriteString(child.Stdout, "file:///tmp/steal javascript:alert(1) https://unknown.invalid/oauth/authorize?state=abcdefghijklmnop\n")
			<-ctx.Done()
			return runtime.Exit{}, ctx.Err()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {

			service, repository, fakeRuntime, root, hash := loginFixture(t, harness.Claude, "sandbox", flow)
			request := interactiveLoginRequest(root, hash, harness.Claude, test.runner)
			request.OpenBrowser = test.opener
			if _, err := service.Login(context.Background(), request); err == nil {
				t.Fatal("Login() unexpectedly succeeded")
			}
			projectID, idErr := model.NewProjectID(root)
			if idErr != nil {
				t.Fatal(idErr)
			}
			profile := auth.SandboxProfile(auth.Profile{Harness: harness.Claude, Name: "work"}, projectID, "main")
			copy, err := repository.PrepareSandbox(context.Background(), profile, model.RunID("01890f5c-7b00-7000-8000-000000000001"), loginAdapter{name: harness.Claude})
			if err != nil {
				t.Fatalf("login run copy was not cleaned: %v", err)
			}
			if err := repository.RemoveRun(context.Background(), copy); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(fakeRuntime.calls, "\n"), "/bin/rm -rf -- /tmp/dsx-run/") {
				t.Fatalf("guest cleanup absent: %#v", fakeRuntime.calls)
			}
		})
	}
}
func observedClaudeProviderURL(state, challenge string) string {
	return "https://claude.com/cai/oauth/authorize?" +
		"code=true" +
		"&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e" +
		"&response_type=code" +
		"&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback" +
		"&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference+user%3Asessions%3Aclaude_code+user%3Amcp_servers+user%3Afile_upload" +
		"&code_challenge=" + challenge +
		"&code_challenge_method=S256" +
		"&state=" + state
}

func testClaudeProviderURL(state, challenge string) string {
	query := url.Values{
		"code":                  {"true"},
		"client_id":             {claudeOAuthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {claudeOAuthRedirectURI},
		"scope":                 {claudeOAuthScope},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return "https://claude.com/cai/oauth/authorize?" + query.Encode()
}

func TestClaudeProviderURLPolicyAndCaptureCleanup(t *testing.T) {
	state := strings.Repeat("s", 43)
	challenge := strings.Repeat("c", 43)
	observed := observedClaudeProviderURL(state, challenge)
	canonical := testClaudeProviderURL(state, challenge)

	canonicalized, err := canonicalClaudeProviderURL(observed)
	if err != nil {
		t.Fatalf("real Claude Code 2.1.226 provider URL rejected: %v", err)
	}
	if canonicalized != canonical {
		t.Fatalf("canonical provider URL = %q, want %q", canonicalized, canonical)
	}

	for _, candidate := range []string{
		"file:///tmp/steal",
		"javascript:alert(1)",
		strings.Replace(observed, "https://claude.com", "https://claude.ai", 1),
		strings.Replace(observed, "https://claude.com", "http://claude.com", 1),
		strings.Replace(observed, "https://claude.com", "https://CLAUDE.com", 1),
		strings.Replace(observed, "https://claude.com", "https://claude.com:443", 1),
		strings.Replace(observed, "https://claude.com", "https://user@claude.com", 1),
		strings.Replace(observed, "/cai/oauth/authorize", "/oauth/authorize", 1),
		strings.Replace(observed, "/cai/oauth/authorize", "/cai/oauth/%61uthorize", 1),
		observed + "#fragment",
		strings.Replace(observed, claudeOAuthClientID, "malicious-client", 1),
		strings.Replace(observed, url.QueryEscape(claudeOAuthRedirectURI), url.QueryEscape("https://attacker.invalid/callback"), 1),
		strings.Replace(observed, url.QueryEscape(claudeOAuthScope), url.QueryEscape("user:profile"), 1),
		strings.Replace(observed, "code_challenge_method=S256", "code_challenge_method=plain", 1),
		strings.Replace(observed, "response_type=code", "response_type=token", 1),
		strings.Replace(observed, "code=true", "code=false", 1),
		strings.Replace(observed, "state="+state, "state=short", 1),
		strings.Replace(observed, "code_challenge="+challenge, "code_challenge="+challenge+"=", 1),
		observed + "&unknown=value",
		observed + "&state=" + state,
		strings.Replace(observed, "redirect_uri=https%3A", "redirect_uri=https%3a", 1),
	} {
		if err := validateClaudeProviderURL(candidate); err == nil {
			t.Fatalf("accepted provider URL %q", candidate)
		}
	}

	var stdout, stderr, recording bytes.Buffer
	opened := 0
	capture := newLoginProviderCapture(context.Background(), func(_ context.Context, got string) error {
		opened++
		if got != canonical {
			t.Fatalf("opened %q, want trusted canonical URL %q", got, canonical)
		}
		return nil
	}, func() {})
	stdoutWriter := capture.Writer(io.MultiWriter(&stdout, &recording))
	stderrWriter := capture.Writer(io.MultiWriter(&stderr, &recording))
	payload := "\x1b]52;c;owned\a reject https://unknown.invalid/oauth/authorize?client_id=" + claudeOAuthClientID +
		"&redirect_uri=" + url.QueryEscape(claudeOAuthRedirectURI) + "\nOpen " + observed + " now\n"
	for offset := 0; offset < len(payload); {
		end := offset + 7
		if end > len(payload) {
			end = len(payload)
		}
		writer := stdoutWriter
		if (offset/7)%2 != 0 {
			writer = stderrWriter
		}
		if _, err := writer.Write([]byte(payload[offset:end])); err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	if opened != 1 {
		t.Fatalf("browser opens = %d, want 1", opened)
	}
	for name, rendered := range map[string]string{
		"stdout": stdout.String(), "stderr": stderr.String(), "recording": recording.String(),
	} {
		if strings.Contains(rendered, "\x1b") {
			t.Fatalf("%s was not sanitized: %q", name, rendered)
		}
		for _, secret := range []string{state, challenge, claudeOAuthClientID, claudeOAuthRedirectURI, claudeOAuthScope, url.QueryEscape(claudeOAuthRedirectURI), url.QueryEscape(claudeOAuthScope)} {
			if strings.Contains(rendered, secret) {
				t.Fatalf("%s disclosed OAuth query value %q in %q", name, secret, rendered)
			}
		}
	}
	if strings.Count(recording.String(), claudeOAuthQueryRedaction) != 2 {
		t.Fatalf("recording did not redact both candidate authorization URLs: %q", recording.String())
	}
	if capture.output != nil {
		t.Fatalf("capture retained raw provider bytes after opening: %q", capture.output)
	}
	capture.Close()
	if capture.ctx != nil || capture.opener != nil || capture.cancel != nil || capture.output != nil {
		t.Fatalf("capture retained state after close: %#v", capture)
	}
}

func TestClaudeProviderCaptureManySmallWritesExhaustsAndCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := 0
	capture := newLoginProviderCapture(ctx, func(context.Context, string) error {
		opened++
		return nil
	}, cancel)
	writer := capture.Writer(io.Discard)
	for written := 0; written <= maxLoginProviderOutput; written += 7 {
		if _, err := writer.Write([]byte("1234567")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("oversized provider discovery did not cancel the login")
	}
	if err := capture.Result(ctx); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("oversized discovery result = %v", err)
	}
	if opened != 0 || capture.output != nil {
		t.Fatalf("oversized discovery opened=%d retained=%q", opened, capture.output)
	}
	capture.Close()
}

func TestHarnessLoginSignalIsReturnedWithoutPromotion(t *testing.T) {
	service, _, _, root, hash := loginFixture(t, harness.OMP, "global", nil)
	request := interactiveLoginRequest(root, hash, harness.OMP, func(context.Context, InteractiveChild) (runtime.Exit, error) {
		return runtime.Exit{Signal: "SIGTERM"}, nil
	})
	result, err := service.Login(context.Background(), request)
	if err != nil || result.Exit.Signal != "SIGTERM" || result.AuthPromotion.Digest != "" {
		t.Fatalf("Login() = %#v, %v", result, err)
	}
}

func containsLoginEnvironment(environment []string, expected string) bool {
	for _, entry := range environment {
		if entry == expected {
			return true
		}
	}
	return false
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func hasLoginArgvSuffix(arguments []string, suffix ...string) bool {
	if len(arguments) < len(suffix) {
		return false
	}
	for index := range suffix {
		if arguments[len(arguments)-len(suffix)+index] != suffix[index] {
			return false
		}
	}
	return true
}
