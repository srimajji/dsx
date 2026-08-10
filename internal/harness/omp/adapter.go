package omp

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/srimajji/dsx/internal/harness"
)

const (
	version        = "17.2.12"
	executable     = "omp"
	artifactSource = "https://github.com/can1357/oh-my-pi/releases/download/v17.2.12/omp-linux-arm64"
	artifactDigest = "sha256:f176edf8174db252abe1aa6e84df284e1b83b8dd7ef34ac7faf7884a5e172a4c"

	mcpConfigArtifact = "mcp.json"
	maxArtifactBytes  = harness.MaxAuthArtifactBytes
	mcpOverlayName    = "omp-ephemeral-mcp.yml"
)

var (
	credentialArtifacts  = []string{"agent.db", "agent.db-wal"}
	mcpNamePattern       = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)
	protectedEnvironment = map[string]struct{}{
		"HOME": {}, "XDG_CONFIG_HOME": {}, "XDG_DATA_HOME": {}, "XDG_STATE_HOME": {},
		"XDG_CACHE_HOME": {}, "TMPDIR": {}, "PI_CONFIG_DIR": {}, "PI_CODING_AGENT_DIR": {},
		"OMP_PROFILE": {}, "PI_PROFILE": {}, "PI_CONFIG_FILES": {},
	}
)

type adapter struct{}

var _ harness.Adapter = (*adapter)(nil)

// New returns the adapter for the pinned OMP Linux arm64 artifact.
func New() harness.Adapter {
	return &adapter{}
}

func (*adapter) Name() harness.Name {
	return harness.OMP
}

func (*adapter) Version() harness.PinnedArtifact {
	return harness.PinnedArtifact{
		Version:    version,
		Source:     artifactSource,
		Digest:     artifactDigest,
		Executable: executable,
	}
}
func (*adapter) ValidateVersion(stdout, stderr string) error {
	if stdout != "omp/17.2.12\n" || stderr != "" {
		return fmt.Errorf("OMP executable version output does not match pinned 17.2.12")
	}
	return nil
}

func (a *adapter) Preflight(ctx context.Context, roots harness.RunRoots) ([]harness.Diagnostic, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := harness.ValidateRoots(roots); err != nil {
		return nil, fmt.Errorf("OMP preflight roots: %w", err)
	}
	if err := harness.ValidateAuthLayout(a.AuthLayout()); err != nil {
		return nil, fmt.Errorf("OMP auth layout: %w", err)
	}
	return []harness.Diagnostic{{
		Severity: "warning",
		Code:     "omp-auth-closed-snapshot-required",
		Message:  "OMP credential seeding and promotion require agent.db and agent.db-wal from a closed OMP process; live SQLite snapshots are unsupported",
	}}, nil
}

func (*adapter) Invocation(request harness.InvocationRequest) (harness.ExecSpec, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.ExecSpec{}, fmt.Errorf("OMP invocation roots: %w", err)
	}
	if !request.Interactive && strings.TrimSpace(request.Prompt) == "" {
		return harness.ExecSpec{}, fmt.Errorf("OMP one-shot invocation requires a prompt")
	}

	environment, err := invocationEnvironment(request.Roots, request.Environment)
	if err != nil {
		return harness.ExecSpec{}, err
	}
	argv := []string{executable}
	if !request.Interactive {
		argv = append(argv, "-p")
	}
	if request.Prompt != "" {
		argv = append(argv, request.Prompt)
	}
	spec := harness.ExecSpec{
		Argv:     argv,
		Env:      environment,
		Cwd:      request.Roots.Workspace,
		Terminal: request.Interactive,
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return harness.ExecSpec{}, fmt.Errorf("invalid OMP invocation: %w", err)
	}
	return spec, nil
}

func (*adapter) AuthLayout() harness.AuthLayout {
	return harness.AuthLayout{
		Backend:             harness.StorageSQLite,
		CredentialArtifacts: append([]string(nil), credentialArtifacts...),
		MaxArtifactBytes:    maxArtifactBytes,
		Environment: map[string]string{
			"PI_CODING_AGENT_DIR": ".",
		},
	}
}

func (*adapter) Seed(ctx context.Context, request harness.SeedRequest) error {
	request.MaxArtifactBytes = maxArtifactBytes
	if len(request.Artifacts) == 0 {
		return fmt.Errorf("OMP seed requires explicit credential artifacts")
	}
	allowed := make(map[string]struct{}, len(credentialArtifacts))
	for _, artifact := range credentialArtifacts {
		allowed[artifact] = struct{}{}
	}
	seen := make(map[string]struct{}, len(request.Artifacts))
	for _, artifact := range request.Artifacts {
		if _, ok := allowed[artifact]; !ok {
			return fmt.Errorf("OMP seed artifact is not credential allowlisted")
		}
		if _, duplicate := seen[artifact]; duplicate {
			return fmt.Errorf("OMP seed contains a duplicate credential artifact")
		}
		seen[artifact] = struct{}{}
	}
	validationRoot, err := os.MkdirTemp(request.DestinationRoot, ".omp-validation-")
	if err != nil {
		return fmt.Errorf("create OMP validation snapshot: %w", err)
	}
	defer os.RemoveAll(validationRoot)
	if err := os.Chmod(validationRoot, 0o700); err != nil {
		return err
	}
	validationRequest := request
	validationRequest.DestinationRoot = validationRoot
	if err := harness.SeedArtifacts(ctx, validationRequest); err != nil {
		return fmt.Errorf("copy OMP validation snapshot: %w", err)
	}
	if err := validateSQLiteSnapshot(ctx, validationRoot); err != nil {
		return fmt.Errorf("validate OMP credential snapshot: %w", err)
	}
	finalRequest := request
	finalRequest.SourceRoot = validationRoot
	if err := harness.SeedArtifacts(ctx, finalRequest); err != nil {
		return fmt.Errorf("seed OMP credentials: %w", err)
	}
	return nil
}

func (*adapter) EphemeralMCP(request harness.MCPRequest) (harness.ConfigInjection, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.ConfigInjection{}, fmt.Errorf("OMP MCP roots: %w", err)
	}

	servers := make(map[string]any, len(request.Servers))
	for index, server := range request.Servers {
		if err := validateMCPServer(index, server); err != nil {
			return harness.ConfigInjection{}, err
		}
		if _, duplicate := servers[server.Name]; duplicate {
			return harness.ConfigInjection{}, fmt.Errorf("OMP MCP server at index %d duplicates another server name", index)
		}
		if len(server.Command) > 0 {
			entry := map[string]any{
				"type":    "stdio",
				"command": server.Command[0],
			}
			if len(server.Command) > 1 {
				entry["args"] = server.Command[1:]
			}
			if len(server.Env) > 0 {
				entry["env"] = server.Env
			}
			servers[server.Name] = entry
			continue
		}
		entry := map[string]any{
			"type": "http",
			"url":  server.URL,
		}
		// MCPServer has one metadata map. For a remote transport it represents
		// request headers; for stdio it represents the child environment.
		if len(server.Env) > 0 {
			entry["headers"] = server.Env
		}
		servers[server.Name] = entry
	}

	data, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return harness.ConfigInjection{}, fmt.Errorf("encode OMP MCP configuration: %w", err)
	}
	data = append(data, '\n')

	mcpPath := path.Join(request.Roots.Auth, mcpConfigArtifact)
	overlayPath := path.Join(request.Roots.Temporary, mcpOverlayName)
	injection := harness.ConfigInjection{
		Files: []harness.GeneratedFile{
			{Path: mcpPath, Mode: 0o600, Data: data},
			{Path: overlayPath, Mode: 0o600, Data: []byte("mcp:\n  enableProjectConfig: false\n")},
		},
		Args: []string{"--config", overlayPath},
	}
	return injection, nil
}

func (*adapter) Login(request harness.LoginRequest) (harness.LoginFlow, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.LoginFlow{}, fmt.Errorf("OMP login roots: %w", err)
	}
	if request.CallbackURL != "" {
		return harness.LoginFlow{}, fmt.Errorf("OMP %s does not support a caller-supplied OAuth callback URL", version)
	}
	environment, err := invocationEnvironment(request.Roots, nil)
	if err != nil {
		return harness.LoginFlow{}, err
	}
	spec := harness.ExecSpec{
		Argv:     []string{executable},
		Env:      environment,
		Cwd:      request.Roots.Workspace,
		Terminal: true,
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return harness.LoginFlow{}, fmt.Errorf("invalid OMP login invocation: %w", err)
	}
	// OMP exposes provider login only inside its interactive TUI and opens the
	// provider URL itself. It has no standalone login argv in 17.2.12.
	return harness.LoginFlow{Exec: spec, OpenBrowser: false, CallbackTimeout: 0}, nil
}

func (*adapter) RedactionRules() harness.RedactionRules {
	return harness.RedactionRules{
		EnvironmentKeys: []string{
			"AI_GATEWAY_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_FOUNDRY_API_KEY",
			"ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_SEARCH_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
			"AWS_SESSION_TOKEN", "AZURE_OPENAI_API_KEY", "BRAVE_API_KEY", "CEREBRAS_API_KEY",
			"CLAUDE_CODE_CLIENT_CERT", "CLAUDE_CODE_CLIENT_KEY", "COPILOT_GITHUB_TOKEN", "CURSOR_ACCESS_TOKEN",
			"EXA_API_KEY", "FIRECRAWL_API_KEY", "GEMINI_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
			"GROQ_API_KEY", "KILO_API_KEY", "MINIMAX_API_KEY", "MISTRAL_API_KEY", "NODE_EXTRA_CA_CERTS",
			"OPENCODE_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "PERPLEXITY_COOKIES",
			"PERPLEXITY_API_KEY", "TAVILY_API_KEY", "TINYFISH_API_KEY", "UMANS_AI_CODING_PLAN_API_KEY",
			"WAFER_SERVERLESS_API_KEY", "XAI_API_KEY", "ZAI_API_KEY",
		},
		PathPrefixes: []string{
			"agent.db",
			"agent.db-wal",
			"mcp.json",
			".omp/agent/agent.db",
			".omp/profiles/",
		},
	}
}

func invocationEnvironment(roots harness.RunRoots, supplied map[string]string) (map[string]string, error) {
	environment := make(map[string]string, len(supplied)+11)
	for key, value := range supplied {
		if _, protected := protectedEnvironment[key]; protected {
			return nil, fmt.Errorf("OMP invocation environment may not override isolation key %q", key)
		}
		environment[key] = value
	}
	environment["HOME"] = roots.Home
	environment["XDG_CONFIG_HOME"] = roots.Config
	environment["XDG_DATA_HOME"] = roots.Data
	environment["XDG_STATE_HOME"] = roots.Data
	environment["XDG_CACHE_HOME"] = roots.Cache
	environment["TMPDIR"] = roots.Temporary
	environment["PI_CONFIG_DIR"] = ".omp"
	environment["PI_CODING_AGENT_DIR"] = roots.Auth
	// Empty OMP_PROFILE is authoritative over PI_PROFILE and selects OMP's
	// default profile, where PI_CODING_AGENT_DIR is honored.
	environment["OMP_PROFILE"] = ""
	environment["PI_PROFILE"] = ""
	return environment, nil
}

func validateSQLiteSnapshot(ctx context.Context, root string) error {
	databasePath := filepath.Join(root, "agent.db")
	walPath := filepath.Join(root, "agent.db-wal")
	database, err := os.Open(databasePath)
	if errors.Is(err, os.ErrNotExist) {
		if _, walErr := os.Lstat(walPath); walErr == nil {
			return errors.New("SQLite WAL exists without its database")
		} else if !errors.Is(walErr, os.ErrNotExist) {
			return walErr
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer database.Close()
	info, err := database.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 100 {
		return errors.New("SQLite database is not a complete regular database file")
	}
	header := make([]byte, 100)
	if _, err := io.ReadFull(database, header); err != nil || string(header[:16]) != "SQLite format 3\x00" {
		return errors.New("invalid SQLite database header")
	}
	pageSize := int(binary.BigEndian.Uint16(header[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 || info.Size()%int64(pageSize) != 0 {
		return errors.New("invalid SQLite database page geometry")
	}
	if err := validateWALGeometry(walPath, pageSize); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	const validationSQL = "PRAGMA query_only=ON;" +
		"PRAGMA integrity_check(1);" +
		"SELECT CASE WHEN count(*)=2 THEN 'objects-ok' ELSE 'objects-invalid' END FROM sqlite_master WHERE type='table' AND name IN ('auth_schema_version','auth_credentials');" +
		"SELECT CASE WHEN count(*)=8 AND sum(name IN ('id','provider','credential_type','data','disabled_cause','identity_key','created_at','updated_at'))=8 THEN 'columns-ok' ELSE 'columns-invalid' END FROM pragma_table_info('auth_credentials');" +
		"SELECT CASE WHEN count(*)=1 AND min(id)=1 AND min(version)=7 THEN 'version-ok' ELSE 'version-invalid' END FROM auth_schema_version;"
	command := exec.CommandContext(ctx, "/usr/bin/sqlite3", "-batch", "-readonly", "-noheader", databasePath, validationSQL)
	command.Env = []string{"HOME=/nonexistent", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("read-only SQLite validation failed: %w", err)
	}
	if string(output) != "ok\nobjects-ok\ncolumns-ok\nversion-ok\n" {
		return fmt.Errorf("SQLite integrity or pinned OMP auth schema validation failed")
	}
	return nil
}

func validateWALGeometry(filePath string, databasePageSize int) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 32 {
		return errors.New("SQLite WAL is truncated")
	}
	header := make([]byte, 32)
	if _, err := io.ReadFull(file, header); err != nil {
		return errors.New("SQLite WAL header is truncated")
	}
	magic := binary.BigEndian.Uint32(header[:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return errors.New("invalid SQLite WAL header")
	}
	walPageSize := int(binary.BigEndian.Uint32(header[8:12]))
	if walPageSize != databasePageSize || (info.Size()-32)%int64(24+databasePageSize) != 0 {
		return errors.New("SQLite WAL is incoherent with the database")
	}
	return nil
}

func validateMCPServer(index int, server harness.MCPServer) error {
	if !mcpNamePattern.MatchString(server.Name) {
		return fmt.Errorf("OMP MCP server at index %d has an invalid name", index)
	}
	hasCommand := len(server.Command) > 0
	hasURL := server.URL != ""
	if hasCommand == hasURL {
		return fmt.Errorf("OMP MCP server at index %d must set exactly one of command or URL", index)
	}
	if hasCommand {
		for argumentIndex, argument := range server.Command {
			if strings.IndexByte(argument, 0) >= 0 || (argumentIndex == 0 && strings.TrimSpace(argument) == "") {
				return fmt.Errorf("OMP MCP server at index %d has an invalid command", index)
			}
		}
	} else {
		parsed, err := url.Parse(server.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
			return fmt.Errorf("OMP MCP server at index %d has an invalid URL", index)
		}
	}
	for key, value := range server.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("OMP MCP server at index %d has an invalid metadata entry", index)
		}
	}
	return nil
}
