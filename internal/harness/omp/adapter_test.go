package omp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/harness"
)

func testRoots() harness.RunRoots {
	return harness.RunRoots{
		Workspace:      "/workspace",
		Home:           "/run/dsx/omp/home",
		Auth:           "/run/dsx/omp/auth",
		Config:         "/run/dsx/omp/config",
		ReadOnlyConfig: "/run/dsx/omp/readonly-config",
		Data:           "/run/dsx/omp/data",
		Cache:          "/run/dsx/omp/cache",
		Temporary:      "/run/dsx/omp/tmp",
	}
}

func TestNewSatisfiesAdapterAndPinsLinuxArm64Artifact(t *testing.T) {
	var got harness.Adapter = New()
	if got.Name() != harness.OMP {
		t.Fatalf("Name() = %q", got.Name())
	}
	want := harness.PinnedArtifact{
		Version:    "17.2.12",
		Source:     "https://github.com/can1357/oh-my-pi/releases/download/v17.2.12/omp-linux-arm64",
		Digest:     "sha256:f176edf8174db252abe1aa6e84df284e1b83b8dd7ef34ac7faf7884a5e172a4c",
		Executable: "omp",
	}
	if artifact := got.Version(); artifact != want {
		t.Fatalf("Version() = %#v, want %#v", artifact, want)
	}
}

func TestValidateVersionAcceptsOnlyExactPinnedOutputWithoutEchoingOutput(t *testing.T) {
	adapter := New()
	if err := adapter.ValidateVersion("omp/17.2.12\n", ""); err != nil {
		t.Fatal(err)
	}
	secret := "omp/17.2.13 secret-output"
	for _, input := range []struct{ stdout, stderr string }{
		{"omp/17.2.13\n", ""},
		{"omp/17.2.12", ""},
		{"omp/17.2.12\n", secret},
	} {
		err := adapter.ValidateVersion(input.stdout, input.stderr)
		if err == nil {
			t.Fatalf("accepted version output %#v", input)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("version error leaked unrelated output: %q", err)
		}
	}
}

func TestInvocationProducesExactOneShotSpecWithoutAmbientHome(t *testing.T) {
	t.Setenv("HOME", "/ambient/home/must-not-leak")
	t.Setenv("PI_CODING_AGENT_DIR", "/ambient/omp/must-not-leak")
	roots := testRoots()
	spec, err := New().Invocation(harness.InvocationRequest{
		Roots:       roots,
		Prompt:      "return exactly ready",
		Environment: map[string]string{"TERM": "xterm-256color"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{"omp", "-p", "return exactly ready"}
	if !reflect.DeepEqual(spec.Argv, wantArgv) {
		t.Fatalf("argv = %#v, want %#v", spec.Argv, wantArgv)
	}
	wantEnv := map[string]string{
		"HOME": roots.Home, "XDG_CONFIG_HOME": roots.Config, "XDG_DATA_HOME": roots.Data,
		"XDG_STATE_HOME": roots.Data, "XDG_CACHE_HOME": roots.Cache, "TMPDIR": roots.Temporary,
		"PI_CONFIG_DIR": ".omp", "PI_CODING_AGENT_DIR": roots.Auth, "OMP_PROFILE": "", "PI_PROFILE": "",
		"TERM": "xterm-256color",
	}
	if !reflect.DeepEqual(spec.Env, wantEnv) {
		t.Fatalf("env = %#v, want %#v", spec.Env, wantEnv)
	}
	if spec.Cwd != roots.Workspace || spec.Terminal {
		t.Fatalf("cwd/terminal = %q/%v", spec.Cwd, spec.Terminal)
	}
	if strings.Contains(strings.Join(harness.SortedEnvironment(spec.Env), "\n"), "/ambient/") {
		t.Fatalf("ambient environment leaked: %#v", spec.Env)
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		t.Fatalf("generated spec is invalid: %v", err)
	}
}

func TestInvocationProducesExactInteractivePTYSpec(t *testing.T) {
	roots := testRoots()
	spec, err := New().Invocation(harness.InvocationRequest{Roots: roots, Prompt: "inspect this workspace", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"omp", "inspect this workspace"}; !reflect.DeepEqual(spec.Argv, want) {
		t.Fatalf("argv = %#v, want %#v", spec.Argv, want)
	}
	if !spec.Terminal || spec.Cwd != roots.Workspace {
		t.Fatalf("interactive spec = %#v", spec)
	}
	empty, err := New().Invocation(harness.InvocationRequest{Roots: roots, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(empty.Argv, []string{"omp"}) || !empty.Terminal {
		t.Fatalf("empty interactive spec = %#v", empty)
	}
}

func TestInvocationRejectsUnsafeRequests(t *testing.T) {
	roots := testRoots()
	if _, err := New().Invocation(harness.InvocationRequest{Roots: roots}); err == nil {
		t.Fatal("accepted an empty one-shot prompt")
	}
	if _, err := New().Invocation(harness.InvocationRequest{Roots: roots, Prompt: "ok", Environment: map[string]string{"HOME": roots.Home}}); err == nil {
		t.Fatal("accepted an isolation environment override")
	}
	roots.Auth = "relative"
	if _, err := New().Invocation(harness.InvocationRequest{Roots: roots, Prompt: "ok"}); err == nil {
		t.Fatal("accepted invalid run roots")
	}
}

type fakeRunner struct{ specs []harness.ExecSpec }

func (f *fakeRunner) run(spec harness.ExecSpec) (string, error) {
	if err := harness.ValidateExecSpec(spec); err != nil {
		return "", err
	}
	f.specs = append(f.specs, spec)
	if spec.Terminal {
		return "interactive PTY attached", nil
	}
	return "one-shot output streamed", nil
}

func TestFakeOneShotAndInteractivePTYSmokeContract(t *testing.T) {
	adapter := New()
	roots := testRoots()
	oneShot, err := adapter.Invocation(harness.InvocationRequest{Roots: roots, Prompt: "smoke"})
	if err != nil {
		t.Fatal(err)
	}
	interactive, err := adapter.Invocation(harness.InvocationRequest{Roots: roots, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	oneShotOutput, err := runner.run(oneShot)
	if err != nil {
		t.Fatal(err)
	}
	interactiveOutput, err := runner.run(interactive)
	if err != nil {
		t.Fatal(err)
	}
	if oneShotOutput != "one-shot output streamed" || interactiveOutput != "interactive PTY attached" {
		t.Fatalf("fake outputs = %q, %q", oneShotOutput, interactiveOutput)
	}
	if len(runner.specs) != 2 || runner.specs[0].Terminal || !runner.specs[1].Terminal {
		t.Fatalf("fake runner terminal contracts = %#v", runner.specs)
	}
}

func TestAuthLayoutIsSQLiteAndAllowsOnlyCredentialDatabaseArtifacts(t *testing.T) {
	layout := New().AuthLayout()
	wantArtifacts := []string{"agent.db", "agent.db-wal"}
	if layout.Backend != harness.StorageSQLite || !reflect.DeepEqual(layout.CredentialArtifacts, wantArtifacts) {
		t.Fatalf("layout = %#v", layout)
	}
	if layout.MaxArtifactBytes != 16<<20 {
		t.Fatalf("MaxArtifactBytes = %d, want 16 MiB", layout.MaxArtifactBytes)
	}
	if !reflect.DeepEqual(layout.Environment, map[string]string{"PI_CODING_AGENT_DIR": "."}) {
		t.Fatalf("auth environment = %#v", layout.Environment)
	}
	if len(layout.ReadOnlyConfig) != 0 {
		t.Fatalf("unexpected reusable writable config = %#v", layout.ReadOnlyConfig)
	}
	if contains(layout.CredentialArtifacts, mcpConfigArtifact) {
		t.Fatal("ephemeral mcp.json would be promoted with credentials")
	}
	if err := harness.ValidateAuthLayout(layout); err != nil {
		t.Fatalf("layout is invalid: %v", err)
	}
}

func TestPreflightDiagnosesClosedProcessSQLiteSnapshotContract(t *testing.T) {
	diagnostics, err := New().Preflight(context.Background(), testRoots())
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	got := diagnostics[0]
	if got.Severity != "warning" || got.Code != "omp-auth-closed-snapshot-required" {
		t.Fatalf("diagnostic = %#v", got)
	}
	if !strings.Contains(got.Message, "closed OMP process") || !strings.Contains(got.Message, "live SQLite snapshots are unsupported") {
		t.Fatalf("diagnostic does not express snapshot boundary: %q", got.Message)
	}
}

func TestSeedCopiesOnlyExplicitValidSQLiteArtifactsPrivately(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	writeSQLiteArtifacts(t, source)
	if err := os.WriteFile(filepath.Join(source, "session.jsonl"), []byte("not credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := harness.SeedRequest{SourceRoot: source, DestinationRoot: destination, Artifacts: New().AuthLayout().CredentialArtifacts}
	if err := New().Seed(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("agent.db mode = %04o", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(destination, "agent.db-wal")); !os.IsNotExist(err) {
		t.Fatalf("absent WAL was invented: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "session.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("non-credential state was seeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "agent.db-shm")); !os.IsNotExist(err) {
		t.Fatalf("transient SQLite shared memory was promoted: %v", err)
	}
	if err := New().Seed(context.Background(), harness.SeedRequest{SourceRoot: source, DestinationRoot: destination, Artifacts: []string{"session.jsonl"}}); err == nil {
		t.Fatal("accepted a non-credential artifact")
	}
	if err := New().Seed(context.Background(), harness.SeedRequest{SourceRoot: source, DestinationRoot: destination}); err == nil {
		t.Fatal("accepted an implicit artifact selection")
	}
}

func TestSeedAcceptsSQLiteArtifactsAboveOneMiBThroughSixteenMiBAndRejectsNextByte(t *testing.T) {
	for _, size := range []int64{2 << 20, 16 << 20} {
		t.Run(fmt.Sprintf("%d-bytes", size), func(t *testing.T) {
			source, destination := t.TempDir(), t.TempDir()
			writeSQLiteArtifactsAtSize(t, source, size)
			if err := New().Seed(context.Background(), harness.SeedRequest{
				SourceRoot: source, DestinationRoot: destination, Artifacts: []string{"agent.db"},
			}); err != nil {
				t.Fatalf("seed %d-byte OMP database: %v", size, err)
			}
			info, err := os.Stat(filepath.Join(destination, "agent.db"))
			if err != nil || info.Size() != size {
				t.Fatalf("staged OMP database size = %v, %v; want %d", info, err, size)
			}
		})
	}

	source, destination := t.TempDir(), t.TempDir()
	writeSQLiteArtifactsAtSize(t, source, 16<<20)
	database := filepath.Join(source, "agent.db")
	if err := os.Truncate(database, (16<<20)+1); err != nil {
		t.Fatal(err)
	}
	if err := New().Seed(context.Background(), harness.SeedRequest{
		SourceRoot: source, DestinationRoot: destination, Artifacts: []string{"agent.db"},
	}); err == nil {
		t.Fatal("accepted OMP database larger than 16 MiB")
	}
	if _, err := os.Lstat(filepath.Join(destination, "agent.db")); !os.IsNotExist(err) {
		t.Fatalf("oversized OMP database reached destination: %v", err)
	}
}

func TestSeedAcceptsBoundedSQLiteWALAboveOneMiBAndRejectsOversizedWAL(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	writeSQLiteArtifacts(t, source)
	wal, stopWriter := startLargeSQLiteWAL(t, filepath.Join(source, "agent.db"))
	defer stopWriter()
	sourceInfo, err := os.Stat(wal)
	if err != nil || sourceInfo.Size() <= 1<<20 || sourceInfo.Size() > 16<<20 {
		t.Fatalf("source WAL = %v, %v; want (1 MiB, 16 MiB]", sourceInfo, err)
	}
	if err := New().Seed(context.Background(), harness.SeedRequest{
		SourceRoot: source, DestinationRoot: destination, Artifacts: []string{"agent.db", "agent.db-wal"},
	}); err != nil {
		t.Fatalf("seed bounded OMP WAL: %v", err)
	}
	info, err := os.Stat(filepath.Join(destination, "agent.db-wal"))
	if err != nil || info.Size() != sourceInfo.Size() {
		t.Fatalf("staged WAL = %v, %v; want %d bytes", info, err, sourceInfo.Size())
	}

	oversizedDestination := t.TempDir()
	if err := os.Truncate(wal, (16<<20)+1); err != nil {
		t.Fatal(err)
	}
	if err := New().Seed(context.Background(), harness.SeedRequest{
		SourceRoot: source, DestinationRoot: oversizedDestination, Artifacts: []string{"agent.db", "agent.db-wal"},
	}); err == nil {
		t.Fatal("accepted OMP WAL larger than 16 MiB")
	}
	if _, err := os.Lstat(filepath.Join(oversizedDestination, "agent.db-wal")); !os.IsNotExist(err) {
		t.Fatalf("oversized OMP WAL reached destination: %v", err)
	}
}

func TestSeedRejectsCorruptAndSymlinkedCredentialArtifacts(t *testing.T) {
	corruptSource := t.TempDir()
	if err := os.WriteFile(filepath.Join(corruptSource, "agent.db"), []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	corruptDestination := t.TempDir()
	if err := New().Seed(context.Background(), harness.SeedRequest{SourceRoot: corruptSource, DestinationRoot: corruptDestination, Artifacts: []string{"agent.db"}}); err == nil {
		t.Fatal("accepted a corrupt SQLite credential database")
	}
	if _, err := os.Stat(filepath.Join(corruptDestination, "agent.db")); !os.IsNotExist(err) {
		t.Fatalf("corrupt database reached destination: %v", err)
	}
	headerOnlySource := t.TempDir()
	if err := os.WriteFile(filepath.Join(headerOnlySource, "agent.db"), []byte("SQLite format 3\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New().Seed(context.Background(), harness.SeedRequest{SourceRoot: headerOnlySource, DestinationRoot: t.TempDir(), Artifacts: []string{"agent.db"}}); err == nil {
		t.Fatal("accepted a header-only SQLite credential database")
	}
	symlinkSource := t.TempDir()
	realDatabase := filepath.Join(symlinkSource, "real.db")
	if err := os.WriteFile(realDatabase, []byte("SQLite format 3\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDatabase, filepath.Join(symlinkSource, "agent.db")); err != nil {
		t.Fatal(err)
	}
	if err := New().Seed(context.Background(), harness.SeedRequest{SourceRoot: symlinkSource, DestinationRoot: t.TempDir(), Artifacts: []string{"agent.db"}}); err == nil {
		t.Fatal("accepted a symlinked SQLite credential database")
	}
}

func TestEphemeralMCPUsesPerRunNativeConfigAndAuthoritativeOverlay(t *testing.T) {
	roots := testRoots()
	injection, err := New().EphemeralMCP(harness.MCPRequest{Roots: roots, Servers: []harness.MCPServer{
		{Name: "browser", URL: "http://browser.internal:8931/mcp", Env: map[string]string{"Authorization": "Bearer run-only"}},
		{Name: "filesystem", Command: []string{"mcp-filesystem", "--root", roots.Workspace}, Env: map[string]string{"RUN_ID": "run-1"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(injection.Files) != 2 {
		t.Fatalf("generated files = %#v", injection.Files)
	}
	mcpFile := injection.Files[0]
	if mcpFile.Path != roots.Auth+"/mcp.json" || mcpFile.Mode != 0o600 {
		t.Fatalf("MCP file location/mode = %q/%04o", mcpFile.Path, mcpFile.Mode)
	}
	var document struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcpFile.Data, &document); err != nil {
		t.Fatal(err)
	}
	browser := document.MCPServers["browser"]
	if browser.Type != "http" || browser.URL != "http://browser.internal:8931/mcp" || browser.Headers["Authorization"] != "Bearer run-only" {
		t.Fatalf("browser MCP = %#v", browser)
	}
	filesystem := document.MCPServers["filesystem"]
	if filesystem.Type != "stdio" || filesystem.Command != "mcp-filesystem" || !reflect.DeepEqual(filesystem.Args, []string{"--root", roots.Workspace}) || filesystem.Env["RUN_ID"] != "run-1" {
		t.Fatalf("filesystem MCP = %#v", filesystem)
	}
	overlay := injection.Files[1]
	if overlay.Path != roots.Temporary+"/"+mcpOverlayName || overlay.Mode != 0o600 {
		t.Fatalf("overlay location/mode = %q/%04o", overlay.Path, overlay.Mode)
	}
	if string(overlay.Data) != "mcp:\n  enableProjectConfig: false\n" {
		t.Fatalf("overlay = %q", overlay.Data)
	}
	if !reflect.DeepEqual(injection.Args, []string{"--config", overlay.Path}) {
		t.Fatalf("overlay args = %#v", injection.Args)
	}
	if len(injection.Env) != 0 {
		t.Fatalf("unexpected MCP environment = %#v", injection.Env)
	}
	if contains(New().AuthLayout().CredentialArtifacts, filepath.Base(mcpFile.Path)) {
		t.Fatal("run-only MCP file is credential-promotable")
	}
}

func TestEphemeralMCPEmptyRequestStillDisablesProjectConfig(t *testing.T) {
	roots := testRoots()
	injection, err := New().EphemeralMCP(harness.MCPRequest{Roots: roots})
	if err != nil {
		t.Fatal(err)
	}
	if len(injection.Files) != 2 {
		t.Fatalf("generated files = %#v", injection.Files)
	}
	if got := string(injection.Files[0].Data); got != "{\n  \"mcpServers\": {}\n}\n" {
		t.Fatalf("empty MCP registry = %q", got)
	}
	overlay := injection.Files[1]
	if got := string(overlay.Data); got != "mcp:\n  enableProjectConfig: false\n" {
		t.Fatalf("project config overlay = %q", got)
	}
	if !reflect.DeepEqual(injection.Args, []string{"--config", overlay.Path}) {
		t.Fatalf("overlay args = %#v", injection.Args)
	}
}

func TestMCPArgsComposeBeforeInvocationArgs(t *testing.T) {
	roots := testRoots()
	invocation, err := New().Invocation(harness.InvocationRequest{Roots: roots, Prompt: "smoke"})
	if err != nil {
		t.Fatal(err)
	}
	injection, err := New().EphemeralMCP(harness.MCPRequest{Roots: roots, Servers: []harness.MCPServer{{Name: "browser", URL: "http://browser.internal/mcp"}}})
	if err != nil {
		t.Fatal(err)
	}
	composed := append([]string{invocation.Argv[0]}, injection.Args...)
	composed = append(composed, invocation.Argv[1:]...)
	want := []string{"omp", "--config", roots.Temporary + "/" + mcpOverlayName, "-p", "smoke"}
	if !reflect.DeepEqual(composed, want) {
		t.Fatalf("composed argv = %#v, want %#v", composed, want)
	}
}

func TestEphemeralMCPRejectsUnsafeServersWithoutRenderingSecrets(t *testing.T) {
	roots := testRoots()
	cases := []harness.MCPServer{
		{Name: "both", Command: []string{"server"}, URL: "https://example.test/mcp"},
		{Name: "bad/name", Command: []string{"server"}},
		{Name: "remote", URL: "https://secret-user:secret-password@example.test/mcp"},
		{Name: "stdio", Command: []string{"server"}, Env: map[string]string{"TOKEN": "secret-value\x00"}},
	}
	for _, server := range cases {
		_, err := New().EphemeralMCP(harness.MCPRequest{Roots: roots, Servers: []harness.MCPServer{server}})
		if err == nil {
			t.Fatalf("accepted unsafe server %#v", server)
		}
		message := err.Error()
		if strings.Contains(message, "secret-user") || strings.Contains(message, "secret-password") || strings.Contains(message, "secret-value") {
			t.Fatalf("error rendered secret input: %q", message)
		}
	}
	roots.Temporary = "tmp"
	if _, err := New().EphemeralMCP(harness.MCPRequest{Roots: roots, Servers: []harness.MCPServer{{Name: "ok", Command: []string{"server"}}}}); err == nil {
		t.Fatal("accepted invalid MCP roots")
	}
}

func TestLoginUsesIsolatedInteractiveOMPAndRejectsExternalCallback(t *testing.T) {
	roots := testRoots()
	flow, err := New().Login(harness.LoginRequest{Roots: roots})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(flow.Exec.Argv, []string{"omp"}) || !flow.Exec.Terminal || flow.Exec.Cwd != roots.Workspace {
		t.Fatalf("login exec = %#v", flow.Exec)
	}
	if flow.Exec.Env["HOME"] != roots.Home || flow.Exec.Env["PI_CODING_AGENT_DIR"] != roots.Auth {
		t.Fatalf("login isolation env = %#v", flow.Exec.Env)
	}
	if flow.OpenBrowser || flow.CallbackTimeout != 0 {
		t.Fatalf("login orchestration = %#v", flow)
	}
	if err := harness.ValidateExecSpec(flow.Exec); err != nil {
		t.Fatalf("login spec is invalid: %v", err)
	}
	callback := "http://127.0.0.1:43123/callback?token=secret-callback-value"
	_, err = New().Login(harness.LoginRequest{Roots: roots, CallbackURL: callback})
	if err == nil {
		t.Fatal("accepted an unsupported caller-supplied callback")
	}
	if strings.Contains(err.Error(), callback) || strings.Contains(err.Error(), "secret-callback-value") {
		t.Fatalf("login error rendered callback secret: %q", err)
	}
}

func TestEveryRootBearingMethodValidatesAllRoots(t *testing.T) {
	roots := testRoots()
	roots.Cache = roots.Data
	adapter := New()
	if _, err := adapter.Preflight(context.Background(), roots); err == nil {
		t.Fatal("Preflight accepted aliased roots")
	}
	if _, err := adapter.Invocation(harness.InvocationRequest{Roots: roots, Prompt: "ok"}); err == nil {
		t.Fatal("Invocation accepted aliased roots")
	}
	if _, err := adapter.EphemeralMCP(harness.MCPRequest{Roots: roots}); err == nil {
		t.Fatal("EphemeralMCP accepted aliased roots")
	}
	if _, err := adapter.Login(harness.LoginRequest{Roots: roots}); err == nil {
		t.Fatal("Login accepted aliased roots")
	}
}

func TestRedactionRulesCoverOMPDatabaseAndCredentialEnvironment(t *testing.T) {
	rules := New().RedactionRules()
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY", "PERPLEXITY_COOKIES"} {
		if !contains(rules.EnvironmentKeys, key) {
			t.Fatalf("missing redaction key %q", key)
		}
	}
	for _, prefix := range []string{"agent.db", "agent.db-wal", "mcp.json", ".omp/agent/agent.db", ".omp/profiles/"} {
		if !contains(rules.PathPrefixes, prefix) {
			t.Fatalf("missing redaction path %q", prefix)
		}
	}
}

const sqliteFixtureSchema = "PRAGMA journal_mode=DELETE;" +
	"CREATE TABLE auth_schema_version (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL);" +
	"INSERT INTO auth_schema_version(id, version) VALUES (1, 7);" +
	"CREATE TABLE auth_credentials (" +
	"id INTEGER PRIMARY KEY AUTOINCREMENT, provider TEXT NOT NULL, credential_type TEXT NOT NULL, data TEXT NOT NULL," +
	"disabled_cause TEXT DEFAULT NULL, identity_key TEXT DEFAULT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);"

func writeSQLiteArtifacts(t *testing.T, root string) {
	t.Helper()
	database := filepath.Join(root, "agent.db")
	command := exec.Command("/usr/bin/sqlite3", "-batch", database, sqliteFixtureSchema)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create SQLite fixture: %v: %s", err, output)
	}
	if err := os.Chmod(database, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent.db-shm"), []byte("transient, never promoted"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSQLiteArtifactsAtSize(t *testing.T, root string, target int64) {
	t.Helper()
	database := filepath.Join(root, "agent.db")
	low, high := int64(1), target
	for low <= high {
		blobBytes := low + (high-low)/2
		_ = os.Remove(database)
		sql := sqliteFixtureSchema +
			"CREATE TABLE fixture_padding(data BLOB);" +
			fmt.Sprintf("INSERT INTO fixture_padding(data) VALUES(zeroblob(%d));VACUUM;", blobBytes)
		command := exec.Command("/usr/bin/sqlite3", "-batch", database, sql)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("create sized SQLite fixture: %v: %s", err, output)
		}
		info, err := os.Stat(database)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case info.Size() == target:
			if err := os.Chmod(database, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "agent.db-shm"), []byte("transient, never promoted"), 0o600); err != nil {
				t.Fatal(err)
			}
			return
		case info.Size() < target:
			low = blobBytes + 1
		default:
			high = blobBytes - 1
		}
	}
	t.Fatalf("could not create a valid SQLite fixture of exactly %d bytes", target)
}

func startLargeSQLiteWAL(t *testing.T, database string) (string, func()) {
	t.Helper()
	_ = os.Remove(database + "-shm")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	command := exec.CommandContext(ctx, "/usr/bin/sqlite3", "-batch", database)
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(stdin, "PRAGMA journal_mode=WAL;\nPRAGMA wal_autocheckpoint=0;\nCREATE TABLE wal_padding(data BLOB);\nINSERT INTO wal_padding VALUES(zeroblob(12*1024*1024));\n.print ready\n"); err != nil {
		cancel()
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			cancel()
			t.Fatalf("prepare large SQLite WAL: %v: %s", err, stderr.String())
		}
		if strings.TrimSpace(line) == "ready" {
			break
		}
	}
	stop := func() {
		cancel()
		_ = stdin.Close()
		_ = command.Wait()
	}
	return database + "-wal", stop
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
