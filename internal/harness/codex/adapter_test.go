package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/harness"
)

func TestNewSatisfiesFrozenAdapter(t *testing.T) {
	var _ harness.Adapter = New()
	if got := New().Name(); got != harness.Codex {
		t.Fatalf("Name() = %q, want %q", got, harness.Codex)
	}
}

func TestPinnedVersionAndUpstreamArtifact(t *testing.T) {
	got := New().Version()
	want := harness.PinnedArtifact{
		Version:    "rust-v0.147.0",
		Source:     "https://github.com/openai/codex/releases/download/rust-v0.147.0/codex-package-aarch64-unknown-linux-musl.tar.gz",
		Digest:     "sha256:89cbf79bd5ae6f9c58da47e8079f311c84219350c9c43c070d42f3e9b2a81401",
		Executable: "codex",
	}
	if got != want {
		t.Fatalf("Version() = %#v, want %#v", got, want)
	}
}

func TestValidateVersionRequiresExactPinnedOutputWithoutEchoingOutput(t *testing.T) {
	adapter := New()
	if err := adapter.ValidateVersion("codex-cli 0.147.0\n", ""); err != nil {
		t.Fatalf("ValidateVersion() error = %v", err)
	}
	for _, test := range []struct {
		stdout string
		stderr string
	}{
		{stdout: "codex-cli 0.146.0\n"},
		{stdout: "codex-cli 0.147.0"},
		{stdout: "codex-cli 0.147.0\nunrelated\n"},
		{stdout: "codex-cli 0.147.0\n", stderr: "secret stderr"},
	} {
		err := adapter.ValidateVersion(test.stdout, test.stderr)
		if err == nil {
			t.Fatalf("ValidateVersion(%q, %q) accepted", test.stdout, test.stderr)
		}
		if strings.Contains(err.Error(), test.stdout) || (test.stderr != "" && strings.Contains(err.Error(), test.stderr)) {
			t.Fatalf("ValidateVersion() leaked command output: %v", err)
		}
	}
}

func TestInvocationExactOneShotSpecAndIsolatedEnvironment(t *testing.T) {
	roots := testRoots()
	prompt := "fix it; echo $HOME\nthen explain"
	spec, err := New().Invocation(harness.InvocationRequest{
		Roots:       roots,
		Prompt:      prompt,
		Environment: map[string]string{"HOME": "/ambient/home", "CODEX_HOME": "/ambient/codex", "KEEP": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"codex", "exec", prompt}; !reflect.DeepEqual(spec.Argv, want) {
		t.Fatalf("Argv = %#v, want %#v", spec.Argv, want)
	}
	if spec.Cwd != roots.Workspace || spec.Terminal {
		t.Fatalf("one-shot cwd/terminal = %q/%v", spec.Cwd, spec.Terminal)
	}
	wantEnv := map[string]string{
		"HOME":              roots.Home,
		"CODEX_HOME":        roots.Auth,
		"CODEX_SQLITE_HOME": roots.Data,
		"XDG_CONFIG_HOME":   roots.Config,
		"XDG_DATA_HOME":     roots.Data,
		"XDG_CACHE_HOME":    roots.Cache,
		"TMPDIR":            roots.Temporary,
		"KEEP":              "yes",
	}
	if !reflect.DeepEqual(spec.Env, wantEnv) {
		t.Fatalf("Env = %#v, want %#v", spec.Env, wantEnv)
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		t.Fatalf("ValidateExecSpec() = %v", err)
	}
	if len(spec.Argv) != 3 || spec.Argv[2] != prompt {
		t.Fatalf("prompt was split across argv: %#v", spec.Argv)
	}
}

func TestInvocationExactInteractiveSpec(t *testing.T) {
	roots := testRoots()
	spec, err := New().Invocation(harness.InvocationRequest{Roots: roots, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"codex"}; !reflect.DeepEqual(spec.Argv, want) {
		t.Fatalf("Argv = %#v, want %#v", spec.Argv, want)
	}
	if !spec.Terminal || spec.Cwd != roots.Workspace {
		t.Fatalf("interactive cwd/terminal = %q/%v", spec.Cwd, spec.Terminal)
	}

	prompt := "start here\nand keep it one argument"
	withPrompt, err := New().Invocation(harness.InvocationRequest{Roots: roots, Interactive: true, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"codex", prompt}; !reflect.DeepEqual(withPrompt.Argv, want) {
		t.Fatalf("interactive prompt argv = %#v, want %#v", withPrompt.Argv, want)
	}
}

func TestInvocationRejectsEmptyOneShotPrompt(t *testing.T) {
	if _, err := New().Invocation(harness.InvocationRequest{Roots: testRoots(), Prompt: " \n\t"}); err == nil {
		t.Fatal("empty one-shot prompt was accepted")
	}
}

func TestEveryRunRequestValidatesAllRoots(t *testing.T) {
	bad := testRoots()
	bad.Cache = bad.Data
	adapter := New()
	if _, err := adapter.Preflight(context.Background(), bad); err == nil {
		t.Fatal("Preflight accepted duplicate roots")
	}
	if _, err := adapter.Invocation(harness.InvocationRequest{Roots: bad, Prompt: "work"}); err == nil {
		t.Fatal("Invocation accepted duplicate roots")
	}
	if _, err := adapter.EphemeralMCP(harness.MCPRequest{Roots: bad, Servers: []harness.MCPServer{{Name: "browser", URL: "http://browser:8931/mcp"}}}); err == nil {
		t.Fatal("EphemeralMCP accepted duplicate roots")
	}
	if _, err := adapter.Login(harness.LoginRequest{Roots: bad}); err == nil {
		t.Fatal("Login accepted duplicate roots")
	}
}

func TestPreflightHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New().Preflight(ctx, testRoots())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Preflight error = %v, want context.Canceled", err)
	}
}

func TestAuthLayoutSeparatesCredentialFromConfigAndSessionState(t *testing.T) {
	layout := New().AuthLayout()
	if err := harness.ValidateAuthLayout(layout); err != nil {
		t.Fatalf("ValidateAuthLayout() = %v", err)
	}
	if layout.Backend != harness.StorageFile {
		t.Fatalf("Backend = %q", layout.Backend)
	}
	if layout.MaxArtifactBytes != 8<<20 {
		t.Fatalf("MaxArtifactBytes = %d, want 8 MiB", layout.MaxArtifactBytes)
	}
	if want := []string{"auth.json"}; !reflect.DeepEqual(layout.CredentialArtifacts, want) {
		t.Fatalf("CredentialArtifacts = %#v, want %#v", layout.CredentialArtifacts, want)
	}
	if len(layout.ReadOnlyConfig) != 0 {
		t.Fatalf("config unexpectedly classified as credential-root reusable data: %#v", layout.ReadOnlyConfig)
	}
	if want := map[string]string{"CODEX_HOME": "."}; !reflect.DeepEqual(layout.Environment, want) {
		t.Fatalf("Environment = %#v, want %#v", layout.Environment, want)
	}
}

func TestSeedCopiesOnlyPrivateValidAuthAndSurvivesRecreation(t *testing.T) {
	source := t.TempDir()
	firstRun := t.TempDir()
	secondRun := t.TempDir()
	auth := []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"test-value"}`)
	writePrivate(t, filepath.Join(source, "auth.json"), auth)
	writePrivate(t, filepath.Join(source, "config.toml"), []byte("model = 'other'"))
	if err := os.MkdirAll(filepath.Join(source, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivate(t, filepath.Join(source, "sessions", "run.jsonl"), []byte("session"))

	request := harness.SeedRequest{SourceRoot: source, DestinationRoot: firstRun, Artifacts: []string{"auth.json"}}
	if err := New().Seed(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	firstAuth, err := os.ReadFile(filepath.Join(firstRun, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstAuth, auth) {
		t.Fatalf("seeded auth = %q, want %q", firstAuth, auth)
	}
	info, err := os.Stat(filepath.Join(firstRun, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("seeded mode = %04o, want 0600", info.Mode().Perm())
	}
	for _, unwanted := range []string{"config.toml", filepath.Join("sessions", "run.jsonl")} {
		if _, err := os.Stat(filepath.Join(firstRun, unwanted)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("non-credential %q was seeded, stat error = %v", unwanted, err)
		}
	}

	if err := New().Seed(context.Background(), harness.SeedRequest{SourceRoot: firstRun, DestinationRoot: secondRun, Artifacts: []string{"auth.json"}}); err != nil {
		t.Fatal(err)
	}
	recreated, err := os.ReadFile(filepath.Join(secondRun, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recreated, auth) {
		t.Fatalf("recreated auth = %q, want %q", recreated, auth)
	}
}

func TestSeedRejectsSymlinkCorruptAndNonCredentialArtifacts(t *testing.T) {
	adapter := New()
	t.Run("symlink", func(t *testing.T) {
		source := t.TempDir()
		target := filepath.Join(t.TempDir(), "auth.json")
		writePrivate(t, target, []byte(`{"OPENAI_API_KEY":"value"}`))
		if err := os.Symlink(target, filepath.Join(source, "auth.json")); err != nil {
			t.Fatal(err)
		}
		err := adapter.Seed(context.Background(), harness.SeedRequest{SourceRoot: source, DestinationRoot: t.TempDir(), Artifacts: []string{"auth.json"}})
		if err == nil {
			t.Fatal("symlinked credential was accepted")
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		source := t.TempDir()
		secret := "must-not-appear"
		writePrivate(t, filepath.Join(source, "auth.json"), []byte(`{"OPENAI_API_KEY":"`+secret))
		err := adapter.Seed(context.Background(), harness.SeedRequest{SourceRoot: source, DestinationRoot: t.TempDir(), Artifacts: []string{"auth.json"}})
		if err == nil {
			t.Fatal("corrupt credential was accepted")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("seed error leaked credential content: %v", err)
		}
	})
	t.Run("allowlist", func(t *testing.T) {
		err := adapter.Seed(context.Background(), harness.SeedRequest{SourceRoot: t.TempDir(), DestinationRoot: t.TempDir(), Artifacts: []string{"config.toml"}})
		if err == nil {
			t.Fatal("non-credential artifact was accepted")
		}
	})
	t.Run("size-limit", func(t *testing.T) {
		source, destination := t.TempDir(), t.TempDir()
		document := `{"OPENAI_API_KEY":"` + strings.Repeat("x", int(maxArtifactBytes)) + `"}`
		writePrivate(t, filepath.Join(source, "auth.json"), []byte(document))
		err := adapter.Seed(context.Background(), harness.SeedRequest{SourceRoot: source, DestinationRoot: destination, Artifacts: []string{"auth.json"}})
		if err == nil {
			t.Fatal("oversized Codex credential was accepted")
		}
		if _, statErr := os.Lstat(filepath.Join(destination, "auth.json")); !os.IsNotExist(statErr) {
			t.Fatalf("oversized Codex credential reached destination: %v", statErr)
		}
	})
}

func TestLoginUsesHeadlessDeviceFlowAndRejectsUnsupportedCallback(t *testing.T) {
	roots := testRoots()
	flow, err := New().Login(harness.LoginRequest{Roots: roots})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"codex", "login", "--device-auth"}; !reflect.DeepEqual(flow.Exec.Argv, want) {
		t.Fatalf("login argv = %#v, want %#v", flow.Exec.Argv, want)
	}
	if !flow.Exec.Terminal || flow.OpenBrowser || flow.CallbackTimeout != 0 {
		t.Fatalf("login terminal/browser/timeout = %v/%v/%d", flow.Exec.Terminal, flow.OpenBrowser, flow.CallbackTimeout)
	}
	if flow.Exec.Env["CODEX_HOME"] != roots.Auth || flow.Exec.Env["HOME"] != roots.Home {
		t.Fatalf("login environment is not isolated: %#v", flow.Exec.Env)
	}
	if err := harness.ValidateExecSpec(flow.Exec); err != nil {
		t.Fatalf("ValidateExecSpec(login) = %v", err)
	}
	if _, err := New().Login(harness.LoginRequest{Roots: roots, CallbackURL: "http://127.0.0.1:54321/auth/callback"}); err == nil {
		t.Fatal("unsupported custom callback was accepted")
	}
}

func TestEphemeralMCPUsesSecretFreeRepeatableOverrides(t *testing.T) {
	request := harness.MCPRequest{
		Roots: testRoots(),
		Servers: []harness.MCPServer{
			{Name: "playwright", URL: "http://browser.internal:8931/mcp"},
			{Name: "local-tool", Command: []string{"node", "server.js", "--stdio"}},
		},
	}
	injection, err := New().EphemeralMCP(request)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"-c", `mcp_servers={}`,
		"-c", `mcp_servers.local-tool={command = "node", args = ["server.js", "--stdio"], required = true }`,
		"-c", `mcp_servers.playwright={url = "http://browser.internal:8931/mcp", required = true }`,
	}
	if !reflect.DeepEqual(injection.Args, wantArgs) {
		t.Fatalf("Args = %#v, want %#v", injection.Args, wantArgs)
	}
	if len(injection.Files) != 0 || len(injection.Env) != 0 {
		t.Fatalf("Codex override unexpectedly wrote files or environment: %#v", injection)
	}

	base, err := New().Invocation(harness.InvocationRequest{Roots: request.Roots, Prompt: "use browser"})
	if err != nil {
		t.Fatal(err)
	}
	combined := append([]string{base.Argv[0]}, injection.Args...)
	combined = append(combined, base.Argv[1:]...)
	if combined[len(combined)-1] != "use browser" || combined[len(combined)-2] != "exec" {
		t.Fatalf("MCP args did not precede invocation args: %#v", combined)
	}
	if strings.Count(strings.Join(injection.Args, " "), "mcp_servers.playwright=") != 1 {
		t.Fatalf("ephemeral same-name override is not singular/highest-precedence: %#v", injection.Args)
	}
}

func TestEphemeralMCPEmptyRequestClearsWholeRegistry(t *testing.T) {
	injection, err := New().EphemeralMCP(harness.MCPRequest{Roots: testRoots()})
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"-c", "mcp_servers={}"}
	if !reflect.DeepEqual(injection.Args, wantArgs) {
		t.Fatalf("Args = %#v, want %#v", injection.Args, wantArgs)
	}
	if len(injection.Files) != 0 || len(injection.Env) != 0 {
		t.Fatalf("empty Codex registry reset unexpectedly wrote files or environment: %#v", injection)
	}
}

func TestEphemeralMCPFailsClosedForSecretsAndUnsupportedTransports(t *testing.T) {
	roots := testRoots()
	secret := "must-not-enter-argv"
	_, err := New().EphemeralMCP(harness.MCPRequest{Roots: roots, Servers: []harness.MCPServer{{Name: secret, URL: "http://browser:8931/mcp", Env: map[string]string{"Authorization": secret}}}})
	if err == nil {
		t.Fatal("MCP environment was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("MCP error leaked secret: %v", err)
	}

	for _, server := range []harness.MCPServer{
		{Name: "none"},
		{Name: "both", URL: "http://browser:8931/mcp", Command: []string{"server"}},
		{Name: "query", URL: "http://browser:8931/mcp?token=value"},
		{Name: "bad.name", URL: "http://browser:8931/mcp"},
	} {
		if _, err := New().EphemeralMCP(harness.MCPRequest{Roots: roots, Servers: []harness.MCPServer{server}}); err == nil {
			t.Fatalf("unsupported MCP server %#v was accepted", server)
		}
	}
}

func TestRedactionCoversCodexCredentialSources(t *testing.T) {
	rules := New().RedactionRules()
	wantKeys := []string{"CODEX_ACCESS_TOKEN", "CODEX_API_KEY", "OPENAI_API_KEY"}
	if !reflect.DeepEqual(rules.EnvironmentKeys, wantKeys) {
		t.Fatalf("EnvironmentKeys = %#v, want %#v", rules.EnvironmentKeys, wantKeys)
	}
	if want := []string{"auth.json"}; !reflect.DeepEqual(rules.PathPrefixes, want) {
		t.Fatalf("PathPrefixes = %#v, want %#v", rules.PathPrefixes, want)
	}
}

func TestSmokeContractsOneShotExitAndInteractivePTYResize(t *testing.T) {
	roots := testRoots()
	oneShot, err := New().Invocation(harness.InvocationRequest{Roots: roots, Prompt: "fail predictably"})
	if err != nil {
		t.Fatal(err)
	}
	fake := fakeExecution{exitCode: 23}
	if exit := fake.run(oneShot); exit != 23 {
		t.Fatalf("one-shot exit = %d, want 23", exit)
	}
	if err := fake.resize(100, 30); err == nil {
		t.Fatal("one-shot non-PTY accepted resize")
	}

	interactive, err := New().Invocation(harness.InvocationRequest{Roots: roots, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	fake.run(interactive)
	if err := fake.resize(132, 43); err != nil {
		t.Fatalf("interactive resize = %v", err)
	}
	if fake.width != 132 || fake.height != 43 {
		t.Fatalf("recorded resize = %dx%d", fake.width, fake.height)
	}
}

type fakeExecution struct {
	terminal bool
	exitCode int
	width    int
	height   int
}

func (fake *fakeExecution) run(spec harness.ExecSpec) int {
	fake.terminal = spec.Terminal
	return fake.exitCode
}

func (fake *fakeExecution) resize(width, height int) error {
	if !fake.terminal {
		return errors.New("process has no terminal")
	}
	fake.width = width
	fake.height = height
	return nil
}

func testRoots() harness.RunRoots {
	return harness.RunRoots{
		Workspace:      "/workspace/project",
		Home:           "/run/dsx/home",
		Auth:           "/run/dsx/auth/codex/default/run-1",
		Config:         "/run/dsx/config/codex/run-1",
		ReadOnlyConfig: "/run/dsx/readonly-config/codex/run-1",
		Data:           "/run/dsx/data/codex/run-1",
		Cache:          "/run/dsx/cache/codex/run-1",
		Temporary:      "/run/dsx/tmp/codex/run-1",
	}
}

func writePrivate(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
