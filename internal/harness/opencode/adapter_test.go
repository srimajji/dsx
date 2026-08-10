package opencode

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

var _ harness.Adapter = New()

func testRoots() harness.RunRoots {
	return harness.RunRoots{
		Workspace:      "/workspace",
		Home:           "/run/dsx/home",
		Auth:           "/run/dsx/auth",
		Config:         "/run/dsx/config",
		ReadOnlyConfig: "/run/dsx/readonly-config",
		Data:           "/run/dsx/data",
		Cache:          "/run/dsx/cache",
		Temporary:      "/run/dsx/tmp",
	}
}

func TestVersionIsExactPinnedLinuxArtifact(t *testing.T) {
	adapter := New()
	if adapter.Name() != harness.OpenCode {
		t.Fatalf("Name() = %q", adapter.Name())
	}
	want := harness.PinnedArtifact{
		Version:    "1.18.15",
		Source:     "https://registry.npmjs.org/opencode-linux-arm64/-/opencode-linux-arm64-1.18.15.tgz",
		Digest:     "sha512-lJ+pPrJOxo3U2HeXis9aN/vrSFf1iXZXC9S0mTSWtm7qnFOJ3SLI7ALf7NKZoRHOOKUs9RfjR1DMLjzBcGAXog==",
		Executable: "opencode",
	}
	if got := adapter.Version(); got != want {
		t.Fatalf("Version() = %#v, want %#v", got, want)
	}
}

func TestValidateVersionRequiresExactCleanBaseline(t *testing.T) {
	adapter := New()
	if err := adapter.ValidateVersion("1.18.15\n", ""); err != nil {
		t.Fatalf("ValidateVersion(): %v", err)
	}
	for _, output := range []struct {
		stdout string
		stderr string
	}{
		{stdout: "1.18.14\n"},
		{stdout: "  1.18.15\r\n"},
		{stdout: "opencode 1.18.15\n"},
		{stdout: "1.18.15\nunrelated"},
		{stdout: "1.18.15\n", stderr: "do-not-render"},
	} {
		err := adapter.ValidateVersion(output.stdout, output.stderr)
		if err == nil {
			t.Fatalf("accepted version output %q / %q", output.stdout, output.stderr)
		}
		if strings.Contains(err.Error(), "do-not-render") || strings.Contains(err.Error(), "unrelated") {
			t.Fatalf("version diagnostic leaked process output: %v", err)
		}
	}
}

func TestInvocationOneShotKeepsPromptAtomicAndIsolatesEnvironment(t *testing.T) {
	roots := testRoots()
	ambient := map[string]string{
		"HOME":                            "/ambient/home",
		"XDG_CONFIG_HOME":                 "/ambient/config",
		"XDG_DATA_HOME":                   "/ambient/data",
		"XDG_CACHE_HOME":                  "/ambient/cache",
		"XDG_STATE_HOME":                  "/ambient/state",
		"XDG_RUNTIME_DIR":                 "/ambient/runtime",
		"TMPDIR":                          "/ambient/tmp",
		"OPENCODE_TEST_HOME":              "/ambient/test-home",
		"OPENCODE_CONFIG":                 "/ambient/opencode.json",
		"OPENCODE_CONFIG_DIR":             "/ambient/opencode",
		"OPENCODE_CONFIG_CONTENT":         `{"mcp":{"ambient":{}}}`,
		"OPENCODE_DISABLE_PROJECT_CONFIG": "0",
		"KEEP":                            "value",
	}
	prompt := "fix spaces; $(touch /tmp/nope)\n--not-an-option"

	spec, err := New().Invocation(harness.InvocationRequest{
		Roots:       roots,
		Prompt:      prompt,
		Environment: ambient,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"opencode", "run", prompt}; !reflect.DeepEqual(spec.Argv, want) {
		t.Fatalf("Argv = %#v, want %#v", spec.Argv, want)
	}
	if spec.Cwd != roots.Workspace || spec.Terminal {
		t.Fatalf("one-shot cwd/terminal = %q/%v", spec.Cwd, spec.Terminal)
	}
	wantEnv := map[string]string{
		"HOME":                            roots.Home,
		"XDG_CONFIG_HOME":                 roots.Config,
		"XDG_DATA_HOME":                   roots.Auth,
		"XDG_CACHE_HOME":                  roots.Cache,
		"XDG_STATE_HOME":                  roots.Data,
		"XDG_RUNTIME_DIR":                 roots.Temporary,
		"TMPDIR":                          roots.Temporary,
		"OPENCODE_DISABLE_AUTOUPDATE":     "1",
		"OPENCODE_DISABLE_PROJECT_CONFIG": "1",
		"OPENCODE_TEST_HOME":              roots.Home,
		"OPENCODE_CONFIG":                 "",
		"OPENCODE_CONFIG_DIR":             roots.Config + "/opencode",
		"OPENCODE_CONFIG_CONTENT":         "",
		"KEEP":                            "value",
	}
	if !reflect.DeepEqual(spec.Env, wantEnv) {
		t.Fatalf("Env = %#v, want %#v", spec.Env, wantEnv)
	}
	if ambient["HOME"] != "/ambient/home" || ambient["OPENCODE_CONFIG_CONTENT"] == "" {
		t.Fatalf("Invocation mutated caller environment: %#v", ambient)
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		t.Fatalf("invalid one-shot ExecSpec: %v", err)
	}
}

func TestInvocationInteractiveIsDirectPTYContract(t *testing.T) {
	roots := testRoots()
	spec, err := New().Invocation(harness.InvocationRequest{Roots: roots, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spec.Argv, []string{"opencode"}) {
		t.Fatalf("Argv = %#v", spec.Argv)
	}
	if !spec.Terminal || spec.Cwd != roots.Workspace {
		t.Fatalf("interactive cwd/terminal = %q/%v", spec.Cwd, spec.Terminal)
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		t.Fatalf("invalid interactive ExecSpec: %v", err)
	}
}

type fakeOutcome struct {
	exitCode int
	signal   string
}

type fakeExecutor struct {
	got     harness.ExecSpec
	outcome fakeOutcome
}

func (f *fakeExecutor) execute(spec harness.ExecSpec) fakeOutcome {
	f.got = spec
	return f.outcome
}

func TestDirectExecSmokeContractsLeaveExitAndSignalToExecutor(t *testing.T) {
	roots := testRoots()
	requests := []harness.InvocationRequest{
		{Roots: roots, Prompt: "one shot"},
		{Roots: roots, Interactive: true},
	}
	outcomes := []fakeOutcome{{exitCode: 23}, {exitCode: 130, signal: "SIGINT"}}
	for index, request := range requests {
		spec, err := New().Invocation(request)
		if err != nil {
			t.Fatal(err)
		}
		fake := fakeExecutor{outcome: outcomes[index]}
		if got := fake.execute(spec); got != outcomes[index] {
			t.Fatalf("outcome = %#v, want %#v", got, outcomes[index])
		}
		if fake.got.Argv[0] != "opencode" || fake.got.Argv[0] == "sh" {
			t.Fatalf("adapter introduced a wrapper: %#v", fake.got.Argv)
		}
	}
}

func TestInvocationRejectsUnsupportedSemanticsAfterRootValidation(t *testing.T) {
	adapter := New()
	roots := testRoots()
	if _, err := adapter.Invocation(harness.InvocationRequest{Roots: roots}); err == nil {
		t.Fatal("accepted one-shot invocation without a prompt")
	}
	if _, err := adapter.Invocation(harness.InvocationRequest{Roots: roots, Prompt: "   "}); err == nil {
		t.Fatal("accepted one-shot invocation with only whitespace")
	}
	if _, err := adapter.Invocation(harness.InvocationRequest{Roots: roots, Interactive: true, Prompt: "ignored"}); err == nil {
		t.Fatal("accepted an interactive prompt that OpenCode would not consume")
	}

	roots.Home = "ambient-relative-home"
	_, err := adapter.Invocation(harness.InvocationRequest{Roots: roots, Interactive: true, Prompt: "ignored"})
	if err == nil || !strings.Contains(err.Error(), "home root") {
		t.Fatalf("invalid roots were not rejected first: %v", err)
	}
}

func TestAuthLayoutUsesOnlyExplicitXDGCredentialArtifact(t *testing.T) {
	layout := New().AuthLayout()
	want := harness.AuthLayout{
		Backend:             harness.StorageFile,
		CredentialArtifacts: []string{"opencode/auth.json"},
		MaxArtifactBytes:    1 << 20,
		Environment:         map[string]string{"XDG_DATA_HOME": "."},
	}
	if !reflect.DeepEqual(layout, want) {
		t.Fatalf("AuthLayout() = %#v, want %#v", layout, want)
	}
	if err := harness.ValidateAuthLayout(layout); err != nil {
		t.Fatal(err)
	}
}

func TestSeedCopiesOnlyAllowlistedCredentialPrivately(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	credential := `{"anthropic":{"type":"oauth","refresh":"refresh","access":"access","expires":1900000000000},"openai":{"type":"api","key":"key"}}`
	if err := os.WriteFile(filepath.Join(source, "opencode", "auth.json"), []byte(credential), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "opencode", "session.json"), []byte("session"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := New().Seed(context.Background(), harness.SeedRequest{
		SourceRoot:      source,
		DestinationRoot: destination,
		Artifacts:       []string{"opencode/auth.json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "opencode", "auth.json"))
	if err != nil || string(got) != credential {
		t.Fatalf("seeded credential = %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(destination, "opencode", "auth.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "opencode", "session.json")); !os.IsNotExist(err) {
		t.Fatalf("non-credential state was seeded: %v", err)
	}
}

func TestSeedRejectsUnsafeOrUnallowlistedArtifacts(t *testing.T) {
	adapter := New()
	for _, artifact := range []string{"../auth.json", "opencode/session.json", "/opencode/auth.json"} {
		err := adapter.Seed(context.Background(), harness.SeedRequest{
			SourceRoot:      t.TempDir(),
			DestinationRoot: t.TempDir(),
			Artifacts:       []string{artifact},
		})
		if err == nil {
			t.Fatalf("accepted seed artifact %q", artifact)
		}
	}

	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(source, "target.json")
	if err := os.WriteFile(target, []byte("credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(source, "opencode", "auth.json")); err != nil {
		t.Fatal(err)
	}
	err := adapter.Seed(context.Background(), harness.SeedRequest{
		SourceRoot:      source,
		DestinationRoot: t.TempDir(),
		Artifacts:       []string{"opencode/auth.json"},
	})
	if err == nil {
		t.Fatal("accepted credential symlink")
	}
}

func TestSeedRejectsTruncatedAndMalformedCredentialObjects(t *testing.T) {
	for _, input := range []string{
		`{"anthropic":{"type":"oauth","refresh":"refresh"`,
		`[]`,
		`{"anthropic":{"type":"oauth","refresh":"","access":"access","expires":1}}`,
		`{"openai":{"type":"api","key":"key","unexpected":true}}`,
		`{"provider":{"type":"unknown","key":"key"}}`,
	} {
		source := t.TempDir()
		if err := os.MkdirAll(filepath.Join(source, "opencode"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "opencode", "auth.json"), []byte(input), 0o600); err != nil {
			t.Fatal(err)
		}
		destination := t.TempDir()
		err := New().Seed(context.Background(), harness.SeedRequest{SourceRoot: source, DestinationRoot: destination, Artifacts: []string{"opencode/auth.json"}})
		if err == nil {
			t.Fatalf("accepted invalid OpenCode credential JSON %q", input)
		}
		if _, err := os.Stat(filepath.Join(destination, "opencode", "auth.json")); !os.IsNotExist(err) {
			t.Fatalf("invalid credential reached destination: %v", err)
		}
	}
}

func TestSeedRejectsCredentialLargerThanAdapterLimitBeforeCopy(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	document := `{"openai":{"type":"api","key":"` + strings.Repeat("x", int(maxArtifactBytes)) + `"}}`
	if err := os.WriteFile(filepath.Join(source, "opencode", "auth.json"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New().Seed(context.Background(), harness.SeedRequest{
		SourceRoot: source, DestinationRoot: destination, Artifacts: []string{"opencode/auth.json"},
	}); err == nil {
		t.Fatal("oversized OpenCode credential was accepted")
	}
	if _, err := os.Lstat(filepath.Join(destination, "opencode", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("oversized OpenCode credential reached destination: %v", err)
	}
}

func TestEphemeralMCPUsesFinalContentOverrideAndDisablesProjectDiscovery(t *testing.T) {
	roots := testRoots()
	injection, err := New().EphemeralMCP(harness.MCPRequest{
		Roots: roots,
		Servers: []harness.MCPServer{
			{
				Name:    "local",
				Command: []string{"node", "server.js", "a b"},
				Env:     map[string]string{"TOKEN": "local-secret"},
			},
			{
				Name: "browser",
				URL:  "http://browser:8931/mcp",
				Env:  map[string]string{"Authorization": "Bearer remote-secret"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(injection.Files) != 0 || len(injection.Args) != 0 {
		t.Fatalf("ephemeral injection wrote a file or used argv: %#v", injection)
	}
	wantContent := `{"mcp":{"browser":{"type":"remote","url":"http://browser:8931/mcp","headers":{"Authorization":"Bearer remote-secret"}},"local":{"type":"local","command":["node","server.js","a b"],"environment":{"TOKEN":"local-secret"}}}}`
	if !reflect.DeepEqual(injection.Env, map[string]string{"OPENCODE_CONFIG_CONTENT": wantContent}) {
		t.Fatalf("injection = %#v", injection)
	}

	invocation, err := New().Invocation(harness.InvocationRequest{
		Roots:  roots,
		Prompt: "use the browser",
		Environment: map[string]string{
			"OPENCODE_CONFIG":                 "/global/opencode.json",
			"OPENCODE_CONFIG_CONTENT":         `{"mcp":{"browser":{"type":"remote","url":"http://wrong"}}}`,
			"OPENCODE_DISABLE_PROJECT_CONFIG": "0",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range injection.Env {
		invocation.Env[key] = value
	}
	if invocation.Env["OPENCODE_CONFIG"] != "" || invocation.Env["OPENCODE_CONFIG_CONTENT"] != wantContent {
		t.Fatalf("run-private final override lost precedence: %#v", invocation.Env)
	}
	if got := invocation.Env["OPENCODE_DISABLE_PROJECT_CONFIG"]; got != "1" {
		t.Fatalf("project config disable = %q", got)
	}
}

func TestEphemeralMCPRejectsUnsupportedOrUnsafeServersWithoutSecretDiagnostics(t *testing.T) {
	adapter := New()
	roots := testRoots()
	cases := []harness.MCPServer{
		{Name: "neither"},
		{Name: "both", Command: []string{"server"}, URL: "https://browser.invalid/mcp"},
		{Name: "empty-command", Command: []string{" "}},
		{Name: "bad-url", URL: "ftp://browser.invalid/mcp"},
		{Name: "secret-url", URL: "https://user:do-not-render@browser.invalid/mcp"},
		{Name: "bad-env", Command: []string{"server"}, Env: map[string]string{"TOKEN": "do-not-render\x00"}},
		{Name: " padded-name", Command: []string{"server"}},
		{Name: "fragment-url", URL: "https://browser.invalid/mcp#fragment"},
		{Name: "bad-header", URL: "https://browser.invalid/mcp", Env: map[string]string{"Authorization": "Bearer secret\r\ndo-not-render"}},
	}
	for _, server := range cases {
		_, err := adapter.EphemeralMCP(harness.MCPRequest{Roots: roots, Servers: []harness.MCPServer{server}})
		if err == nil {
			t.Fatalf("accepted invalid server %q", server.Name)
		}
		if strings.Contains(err.Error(), "do-not-render") {
			t.Fatalf("diagnostic exposed config content: %v", err)
		}
	}

	_, err := adapter.EphemeralMCP(harness.MCPRequest{
		Roots: roots,
		Servers: []harness.MCPServer{
			{Name: "duplicate", Command: []string{"one"}},
			{Name: "duplicate", Command: []string{"two"}},
		},
	})
	if err == nil {
		t.Fatal("accepted duplicate MCP names")
	}

	invalidRoots := roots
	invalidRoots.Config = "relative"
	_, err = adapter.EphemeralMCP(harness.MCPRequest{Roots: invalidRoots, Servers: []harness.MCPServer{{Name: "neither"}}})
	if err == nil || !strings.Contains(err.Error(), "config root") {
		t.Fatalf("invalid roots were not rejected first: %v", err)
	}
}

func TestEphemeralMCPAlwaysRequiresEffectiveRegistryVerification(t *testing.T) {
	roots := testRoots()
	adapter := New()
	injection, err := adapter.EphemeralMCP(harness.MCPRequest{Roots: roots})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(injection.Env, map[string]string{"OPENCODE_CONFIG_CONTENT": `{"mcp":{}}`}) {
		t.Fatalf("empty injection = %#v", injection)
	}

	verifier, ok := adapter.(harness.MCPVerifier)
	if !ok {
		t.Fatal("OpenCode adapter does not require effective MCP verification")
	}
	spec, err := verifier.MCPVerification(harness.MCPRequest{Roots: roots}, injection)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spec.Argv, []string{"opencode", "debug", "config"}) ||
		spec.Env["OPENCODE_CONFIG_CONTENT"] != `{"mcp":{}}` ||
		spec.Env["OPENCODE_DISABLE_PROJECT_CONFIG"] != "1" {
		t.Fatalf("verification spec = %#v", spec)
	}
}

func TestEffectiveMCPRefusesHostileSameAndDifferentlyNamedRegistries(t *testing.T) {
	adapter := New().(harness.MCPVerifier)
	request := harness.MCPRequest{
		Roots: testRoots(),
		Servers: []harness.MCPServer{{
			Name: "browser",
			URL:  "https://browser.invalid/mcp",
		}},
	}
	for _, effective := range []string{
		`{"mcp":{"browser":{"type":"remote","url":"https://wrong.invalid/mcp"}}}`,
		`{"mcp":{"browser":{"type":"remote","url":"https://browser.invalid/mcp"},"hostile":{"type":"local","command":["steal"]}}}`,
		`{"mcp":{"browser":{"type":"remote","url":"https://browser.invalid/mcp","headers":{"Hostile":"retained"}}}}`,
	} {
		if err := adapter.ValidateEffectiveMCP(request, effective, ""); err == nil {
			t.Fatalf("accepted hostile effective registry %s", effective)
		}
	}
	if err := adapter.ValidateEffectiveMCP(request, `{"model":"safe","mcp":{"browser":{"url":"https://browser.invalid/mcp","type":"remote"}}}`, ""); err != nil {
		t.Fatalf("rejected exact requested registry: %v", err)
	}
	if err := adapter.ValidateEffectiveMCP(harness.MCPRequest{Roots: testRoots()}, `{"mcp":{"ambient":{"type":"local","command":["steal"]}}}`, ""); err == nil {
		t.Fatal("empty request accepted inherited MCP server")
	}
	if err := adapter.ValidateEffectiveMCP(request, `{"mcp":{}}`, "provider secret"); err == nil || strings.Contains(err.Error(), "provider secret") {
		t.Fatalf("stderr refusal = %v", err)
	}
}

func TestLoginUsesProviderAuthPTYAndRejectsCallbackSemantics(t *testing.T) {
	roots := testRoots()
	flow, err := New().Login(harness.LoginRequest{Roots: roots})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(flow.Exec.Argv, []string{"opencode", "auth", "login"}) {
		t.Fatalf("login argv = %#v", flow.Exec.Argv)
	}
	if !flow.Exec.Terminal || flow.Exec.Cwd != roots.Workspace || flow.OpenBrowser || flow.CallbackTimeout != 0 {
		t.Fatalf("login flow = %#v", flow)
	}
	if flow.Exec.Env["HOME"] != roots.Home || flow.Exec.Env["XDG_DATA_HOME"] != roots.Auth {
		t.Fatalf("login is not isolated: %#v", flow.Exec.Env)
	}
	if err := harness.ValidateExecSpec(flow.Exec); err != nil {
		t.Fatalf("invalid login ExecSpec: %v", err)
	}

	secretCallback := "https://callback.invalid/?token=do-not-render"
	_, err = New().Login(harness.LoginRequest{Roots: roots, CallbackURL: secretCallback})
	if err == nil {
		t.Fatal("accepted unsupported callback URL")
	}
	if strings.Contains(err.Error(), "do-not-render") {
		t.Fatalf("callback diagnostic exposed a secret: %v", err)
	}

	invalidRoots := roots
	invalidRoots.Auth = "relative"
	_, err = New().Login(harness.LoginRequest{Roots: invalidRoots, CallbackURL: secretCallback})
	if err == nil || !strings.Contains(err.Error(), "auth root") {
		t.Fatalf("invalid roots were not rejected first: %v", err)
	}
}

func TestPreflightValidatesRootsAndContext(t *testing.T) {
	adapter := New()
	diagnostics, err := adapter.Preflight(context.Background(), testRoots())
	if err != nil || diagnostics != nil {
		t.Fatalf("Preflight() = %#v, %v", diagnostics, err)
	}

	invalid := testRoots()
	invalid.Cache = invalid.Data
	if _, err := adapter.Preflight(context.Background(), invalid); err == nil {
		t.Fatal("preflight accepted aliased roots")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Preflight(ctx, testRoots()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Preflight() error = %v", err)
	}
}

func TestRedactionCoversEphemeralConfigAndCredentialPath(t *testing.T) {
	want := harness.RedactionRules{
		EnvironmentKeys: []string{"OPENCODE_CONFIG_CONTENT"},
		PathPrefixes:    []string{"opencode/auth.json"},
	}
	if got := New().RedactionRules(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RedactionRules() = %#v, want %#v", got, want)
	}
}
