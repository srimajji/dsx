package claude_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/harness/claude"
)

func TestNewSatisfiesPinnedClaudeLinuxArtifactContract(t *testing.T) {
	var adapter harness.Adapter = claude.New()
	if adapter.Name() != harness.Claude {
		t.Fatalf("Name() = %q", adapter.Name())
	}
	want := harness.PinnedArtifact{
		Version:    "2.1.226",
		Source:     "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-arm64/-/claude-code-linux-arm64-2.1.226.tgz",
		Digest:     "sha512-/USq3R28PunkjZZfyweEgUAlR7npgrruZmb47j3oRRxlFWk5Jurj+THiwhl/AKxUgkxaxb35cr6DCAeXNlQ/jg==",
		Executable: "claude",
	}
	if got := adapter.Version(); got != want {
		t.Fatalf("Version() = %#v, want %#v", got, want)
	}
}

func TestValidateVersionAcceptsOnlyPinnedClaudeOutputWithoutLeakingOutput(t *testing.T) {
	adapter := claude.New()
	for _, stdout := range []string{"2.1.226 (Claude Code)", "2.1.226 (Claude Code)\n"} {
		if err := adapter.ValidateVersion(stdout, ""); err != nil {
			t.Fatalf("ValidateVersion(%q) = %v", stdout, err)
		}
	}
	for _, test := range []struct {
		stdout string
		stderr string
	}{
		{stdout: "2.1.225 (Claude Code)\n"},
		{stdout: "2.1.226 (Claude Code)\nextra output\n"},
		{stdout: "2.1.226 (Claude Code)\n", stderr: "secret diagnostic"},
	} {
		err := adapter.ValidateVersion(test.stdout, test.stderr)
		if err == nil {
			t.Fatalf("ValidateVersion(%q, %q) succeeded", test.stdout, test.stderr)
		}
		if strings.Contains(err.Error(), test.stdout) || (test.stderr != "" && strings.Contains(err.Error(), test.stderr)) {
			t.Fatalf("ValidateVersion leaked unrelated process output: %v", err)
		}
	}
}

func TestInvocationReturnsExactInteractiveAndPrintSpecs(t *testing.T) {
	adapter := claude.New()
	roots := testRoots()

	interactive, err := adapter.Invocation(harness.InvocationRequest{
		Roots:       roots,
		Prompt:      "not an interactive argv atom",
		Interactive: true,
		Environment: map[string]string{
			"PATH":              "/opt/dsx/bin:/usr/bin",
			"HOME":              "/Users/host",
			"CLAUDE_CONFIG_DIR": "/Users/host/.claude",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(interactive.Argv, []string{"claude"}) {
		t.Fatalf("interactive argv = %#v", interactive.Argv)
	}
	if !interactive.Terminal {
		t.Fatal("interactive invocation did not request a PTY")
	}
	if err := harness.ValidateExecSpec(interactive); err != nil {
		t.Fatalf("interactive spec does not validate: %v", err)
	}

	prompt := "review this; printf '%s' \"$HOME\" && touch /tmp/pwned"
	oneShot, err := adapter.Invocation(harness.InvocationRequest{
		Roots:       roots,
		Prompt:      prompt,
		Interactive: false,
		Environment: map[string]string{"ANTHROPIC_API_KEY": "secret", "HOME": "/Users/host"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(oneShot.Argv, []string{"claude", "-p", prompt}) {
		t.Fatalf("one-shot argv split or changed the prompt: %#v", oneShot.Argv)
	}
	if oneShot.Terminal {
		t.Fatal("one-shot invocation requested a terminal")
	}
	if oneShot.Cwd != roots.Workspace {
		t.Fatalf("one-shot cwd = %q", oneShot.Cwd)
	}

	settingsPath := path.Join(roots.ReadOnlyConfig, "settings.json")
	withSettings, err := adapter.Invocation(harness.InvocationRequest{
		Roots:          roots,
		Prompt:         "review",
		ReadOnlyConfig: []string{settingsPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude", "--settings", settingsPath, "-p", "review"}; !reflect.DeepEqual(withSettings.Argv, want) {
		t.Fatalf("reviewed settings argv = %#v, want %#v", withSettings.Argv, want)
	}
	if _, err := adapter.Invocation(harness.InvocationRequest{Roots: roots, ReadOnlyConfig: []string{path.Join(roots.ReadOnlyConfig, "other.json")}}); err == nil {
		t.Fatal("accepted unallowlisted reviewed configuration")
	}
	if err := harness.ValidateExecSpec(oneShot); err != nil {
		t.Fatalf("one-shot spec does not validate: %v", err)
	}

	wantIsolation := map[string]string{
		"HOME":               roots.Home,
		"CLAUDE_CONFIG_DIR":  roots.Auth,
		"XDG_CONFIG_HOME":    roots.Config,
		"XDG_DATA_HOME":      roots.Data,
		"XDG_CACHE_HOME":     roots.Cache,
		"TMPDIR":             roots.Temporary,
		"TMP":                roots.Temporary,
		"TEMP":               roots.Temporary,
		"CLAUDE_CODE_TMPDIR": roots.Temporary,
	}
	for key, want := range wantIsolation {
		if got := oneShot.Env[key]; got != want {
			t.Errorf("one-shot env %s = %q, want %q", key, got, want)
		}
	}
	if oneShot.Env["ANTHROPIC_API_KEY"] != "secret" {
		t.Fatal("non-isolation environment was not preserved")
	}
	for _, value := range oneShot.Env {
		if strings.Contains(value, "/Users/host") {
			t.Fatalf("ambient host home leaked into environment: %#v", oneShot.Env)
		}
	}
}

func TestEveryRunRootIsValidatedBeforeUse(t *testing.T) {
	adapter := claude.New()
	roots := testRoots()
	roots.Cache = "relative/cache"

	if _, err := adapter.Preflight(context.Background(), roots); err == nil {
		t.Fatal("Preflight accepted invalid roots")
	}
	if _, err := adapter.Invocation(harness.InvocationRequest{Roots: roots}); err == nil {
		t.Fatal("Invocation accepted invalid roots")
	}
	if _, err := adapter.EphemeralMCP(harness.MCPRequest{Roots: roots}); err == nil {
		t.Fatal("EphemeralMCP accepted invalid roots")
	}
	if _, err := adapter.Login(harness.LoginRequest{Roots: roots}); err == nil {
		t.Fatal("Login accepted invalid roots")
	}
}

func TestMacOSKeychainIsReportedAsNonPortable(t *testing.T) {
	adapter := claude.New()
	diagnostics, err := adapter.Preflight(context.Background(), testRoots())
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 || diagnostics[0].Severity != "warning" || diagnostics[0].Code != "claude.auth.macos-keychain-nonportable" {
		t.Fatalf("Preflight diagnostics = %#v", diagnostics)
	}
	if !strings.Contains(diagnostics[0].Message, "macOS Keychain") || !strings.Contains(diagnostics[0].Message, ".credentials.json") {
		t.Fatalf("Preflight did not explain the portable boundary: %#v", diagnostics[0])
	}

	layout := adapter.AuthLayout()
	if err := harness.ValidateAuthLayout(layout); err != nil {
		t.Fatalf("AuthLayout does not validate: %v", err)
	}
	if layout.Backend != harness.StorageFile || !reflect.DeepEqual(layout.CredentialArtifacts, []string{".credentials.json"}) {
		t.Fatalf("portable credential layout = %#v", layout)
	}
	if layout.MaxArtifactBytes != 1<<20 {
		t.Fatalf("MaxArtifactBytes = %d, want 1 MiB", layout.MaxArtifactBytes)
	}
	if !reflect.DeepEqual(layout.Environment, map[string]string{"CLAUDE_CONFIG_DIR": "."}) {
		t.Fatalf("credential-relative environment = %#v", layout.Environment)
	}
	for _, artifact := range append(append([]string(nil), layout.CredentialArtifacts...), layout.ReadOnlyConfig...) {
		if strings.Contains(strings.ToLower(artifact), "keychain") {
			t.Fatalf("AuthLayout claims a Keychain artifact is portable: %q", artifact)
		}
	}
}

func TestCredentialAllowlistExcludesSessionsAndKeepsReviewedConfigReadOnly(t *testing.T) {
	layout := claude.New().AuthLayout()
	if !reflect.DeepEqual(layout.CredentialArtifacts, []string{".credentials.json"}) {
		t.Fatalf("credential artifacts = %#v", layout.CredentialArtifacts)
	}
	if !reflect.DeepEqual(layout.ReadOnlyConfig, []string{"settings.json"}) {
		t.Fatalf("read-only config = %#v", layout.ReadOnlyConfig)
	}
	for _, forbidden := range []string{".claude.json", "projects", "history.jsonl", "session-env"} {
		if slicesContain(layout.CredentialArtifacts, forbidden) {
			t.Fatalf("session or configuration state %q is credential material", forbidden)
		}
	}
}

func TestSeedCopiesOnlyPortableCredentialPrivately(t *testing.T) {
	adapter := claude.New()
	source := t.TempDir()
	destination := t.TempDir()
	credential := `{"claudeAiOauth":{"accessToken":"access","refreshToken":"refresh","expiresAt":1900000000000,"scopes":["user:inference"]}}`
	if err := os.WriteFile(filepath.Join(source, ".credentials.json"), []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "history.jsonl"), []byte("session"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := adapter.Seed(context.Background(), harness.SeedRequest{
		SourceRoot:      source,
		DestinationRoot: destination,
		Artifacts:       []string{".credentials.json"},
	}); err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(destination, ".credentials.json")
	if got, err := os.ReadFile(credentialPath); err != nil || string(got) != credential {
		t.Fatalf("seeded credential = %q, %v", got, err)
	}
	if info, err := os.Stat(credentialPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("seeded credential mode = %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "history.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session state was seeded: %v", err)
	}

	err := adapter.Seed(context.Background(), harness.SeedRequest{
		SourceRoot:      source,
		DestinationRoot: destination,
		Artifacts:       []string{"history.jsonl"},
	})
	if err == nil {
		t.Fatal("Seed accepted session state")
	}
	if strings.Contains(err.Error(), "history.jsonl") {
		t.Fatalf("Seed rendered a caller-controlled artifact path: %v", err)
	}
}

func TestSeedRejectsTruncatedAndNonCredentialJSON(t *testing.T) {
	adapter := claude.New()
	for _, input := range []string{
		`{"claudeAiOauth":{"accessToken":"access"`,
		`[]`,
		`{"claudeAiOauth":{"accessToken":"access","refreshToken":"","expiresAt":1,"scopes":[]}}`,
		`{"claudeAiOauth":{"accessToken":"access","refreshToken":"refresh","expiresAt":1,"scopes":[]},"unexpected":true}`,
	} {
		source := t.TempDir()
		if err := os.WriteFile(filepath.Join(source, ".credentials.json"), []byte(input), 0o600); err != nil {
			t.Fatal(err)
		}
		destination := t.TempDir()
		if err := adapter.Seed(context.Background(), harness.SeedRequest{SourceRoot: source, DestinationRoot: destination, Artifacts: []string{".credentials.json"}}); err == nil {
			t.Fatalf("accepted invalid credential JSON %q", input)
		}
		if _, err := os.Stat(filepath.Join(destination, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid credential reached destination: %v", err)
		}
	}
}

func TestSeedRejectsCredentialLargerThanAdapterLimitBeforeCopy(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	document := `{"claudeAiOauth":{"accessToken":"` + strings.Repeat("x", 1<<20) + `","refreshToken":"refresh","expiresAt":1,"scopes":[]}}`
	if err := os.WriteFile(filepath.Join(source, ".credentials.json"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := claude.New().Seed(context.Background(), harness.SeedRequest{
		SourceRoot: source, DestinationRoot: destination, Artifacts: []string{".credentials.json"},
	}); err == nil {
		t.Fatal("oversized Claude credential was accepted")
	}
	if _, err := os.Lstat(filepath.Join(destination, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("oversized Claude credential reached destination: %v", err)
	}
}

func TestLoginUsesGuestFileAuthAndFailsClosedForCallbackDelivery(t *testing.T) {
	adapter := claude.New()
	roots := testRoots()
	flow, err := adapter.Login(harness.LoginRequest{Roots: roots})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(flow.Exec.Argv, []string{"claude", "auth", "login"}) {
		t.Fatalf("login argv = %#v", flow.Exec.Argv)
	}
	if !flow.Exec.Terminal || !flow.OpenBrowser {
		t.Fatalf("login interactive/browser flags = terminal %t, browser %t", flow.Exec.Terminal, flow.OpenBrowser)
	}
	if flow.CallbackTimeout != 300 {
		t.Fatalf("login callback timeout = %d", flow.CallbackTimeout)
	}
	if flow.Exec.Env["HOME"] != roots.Home || flow.Exec.Env["CLAUDE_CONFIG_DIR"] != roots.Auth {
		t.Fatalf("login did not isolate guest file auth: %#v", flow.Exec.Env)
	}
	if err := harness.ValidateExecSpec(flow.Exec); err != nil {
		t.Fatalf("login spec does not validate: %v", err)
	}
	settingsPath := path.Join(roots.ReadOnlyConfig, "settings.json")
	withSettings, err := adapter.Login(harness.LoginRequest{Roots: roots, ReadOnlyConfig: []string{settingsPath}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude", "--settings", settingsPath, "auth", "login"}; !reflect.DeepEqual(withSettings.Exec.Argv, want) {
		t.Fatalf("reviewed settings login argv = %#v, want %#v", withSettings.Exec.Argv, want)
	}

	callback := "http://127.0.0.1:43891/oauth/callback"
	if _, err := adapter.Login(harness.LoginRequest{Roots: roots, CallbackURL: callback}); err == nil {
		t.Fatal("Login accepted a callback URI that Claude cannot receive from the guest")
	} else if !strings.Contains(err.Error(), "guest callback delivery is unavailable") {
		t.Fatalf("unsupported callback error = %v", err)
	}

	if _, err := adapter.Login(harness.LoginRequest{Roots: roots, CallbackURL: "file:///Users/host/Library/Keychains/login.keychain-db"}); err == nil {
		t.Fatal("Login accepted a host Keychain path as a callback")
	} else if strings.Contains(err.Error(), "/Users/host") {
		t.Fatalf("Login rendered the rejected host path: %v", err)
	}
}

func TestEphemeralMCPIsStrictRunScopedAndPrivate(t *testing.T) {
	adapter := claude.New()
	roots := testRoots()
	injection, err := adapter.EphemeralMCP(harness.MCPRequest{
		Roots: roots,
		Servers: []harness.MCPServer{{
			Name: "playwright",
			URL:  "http://browser.internal:8931/mcp",
			Env:  map[string]string{"Authorization": "Bearer super-secret"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := roots.Temporary + "/claude-mcp.json"
	if !reflect.DeepEqual(injection.Args, []string{"--mcp-config", wantPath, "--strict-mcp-config"}) {
		t.Fatalf("MCP args = %#v", injection.Args)
	}
	invocation, err := adapter.Invocation(harness.InvocationRequest{Roots: roots, Prompt: "use browser", Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	composedArgv := append([]string{invocation.Argv[0]}, injection.Args...)
	composedArgv = append(composedArgv, invocation.Argv[1:]...)
	if want := []string{"claude", "--mcp-config", wantPath, "--strict-mcp-config", "-p", "use browser"}; !reflect.DeepEqual(composedArgv, want) {
		t.Fatalf("strict MCP precedence argv = %#v, want %#v", composedArgv, want)
	}
	if len(injection.Files) != 1 || injection.Files[0].Path != wantPath || injection.Files[0].Mode != 0o600 {
		t.Fatalf("MCP generated files = %#v", injection.Files)
	}
	if len(injection.Env) != 0 {
		t.Fatalf("MCP injection unexpectedly mutates environment: %#v", injection.Env)
	}
	if strings.HasPrefix(injection.Files[0].Path, roots.Home+"/") || strings.HasPrefix(injection.Files[0].Path, roots.Auth+"/") || strings.HasPrefix(injection.Files[0].Path, roots.Config+"/") {
		t.Fatalf("MCP configuration mutates reusable/global state: %q", injection.Files[0].Path)
	}

	wantJSON := "{\"mcpServers\":{\"playwright\":{\"type\":\"http\",\"url\":\"http://browser.internal:8931/mcp\",\"headers\":{\"Authorization\":\"Bearer super-secret\"}}}}\n"
	if got := string(injection.Files[0].Data); got != wantJSON {
		t.Fatalf("MCP JSON = %q, want %q", got, wantJSON)
	}
	var document struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(injection.Files[0].Data, &document); err != nil || len(document.MCPServers) != 1 {
		t.Fatalf("MCP JSON contract = %#v, %v", document, err)
	}
}

func TestEphemeralMCPPreservesStructuredStdioCommand(t *testing.T) {
	injection, err := claude.New().EphemeralMCP(harness.MCPRequest{
		Roots: testRoots(),
		Servers: []harness.MCPServer{{
			Name:    "local_tools",
			Command: []string{"node", "/opt/tools/server.js", "argument with spaces"},
			Env:     map[string]string{"TOKEN": "secret"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"mcpServers\":{\"local_tools\":{\"type\":\"stdio\",\"command\":\"node\",\"args\":[\"/opt/tools/server.js\",\"argument with spaces\"],\"env\":{\"TOKEN\":\"secret\"}}}}\n"
	if got := string(injection.Files[0].Data); got != want {
		t.Fatalf("stdio MCP JSON = %q, want %q", got, want)
	}
}

func TestOneShotFakeExitAndInteractivePTYSmokeContracts(t *testing.T) {
	adapter := claude.New()
	oneShot, err := adapter.Invocation(harness.InvocationRequest{Roots: testRoots(), Prompt: "finish", Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	oneShotRunner := fakeRunner{exitCode: 23}
	if got := oneShotRunner.run(oneShot); got != 23 || oneShotRunner.sawTerminal {
		t.Fatalf("one-shot fake exit contract = exit %d, terminal %t", got, oneShotRunner.sawTerminal)
	}

	interactive, err := adapter.Invocation(harness.InvocationRequest{Roots: testRoots(), Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	interactiveRunner := fakeRunner{}
	interactiveRunner.run(interactive)
	if !interactiveRunner.sawTerminal || !interactiveRunner.resize(132, 43) {
		t.Fatalf("interactive PTY smoke contract = terminal %t, resize %t", interactiveRunner.sawTerminal, interactiveRunner.resized)
	}
}

func TestRedactionCoversPortableCredentialMCPAndEnvironmentSecrets(t *testing.T) {
	rules := claude.New().RedactionRules()
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if !slicesContain(rules.EnvironmentKeys, key) {
			t.Errorf("redaction omits %s", key)
		}
	}
	for _, path := range []string{".credentials.json", "claude-mcp.json"} {
		if !slicesContain(rules.PathPrefixes, path) {
			t.Errorf("redaction omits %s", path)
		}
	}
}

type fakeRunner struct {
	exitCode    int
	sawTerminal bool
	resized     bool
}

func (runner *fakeRunner) run(spec harness.ExecSpec) int {
	runner.sawTerminal = spec.Terminal
	return runner.exitCode
}

func (runner *fakeRunner) resize(columns, rows uint16) bool {
	if !runner.sawTerminal || columns == 0 || rows == 0 {
		return false
	}
	runner.resized = true
	return true
}

func testRoots() harness.RunRoots {
	return harness.RunRoots{
		Workspace:      "/workspace/project",
		Home:           "/run/dsx/home",
		Auth:           "/run/dsx/auth/claude/default",
		Config:         "/run/dsx/config/claude",
		ReadOnlyConfig: "/run/dsx/readonly-config/claude",
		Data:           "/run/dsx/data/claude",
		Cache:          "/run/dsx/cache/claude",
		Temporary:      "/run/dsx/tmp/claude",
	}
}

func slicesContain(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
