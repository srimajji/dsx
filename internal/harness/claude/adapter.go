package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/srimajji/dsx/internal/harness"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	version                  = "2.1.226"
	executable               = "claude"
	artifactSource           = "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-arm64/-/claude-code-linux-arm64-2.1.226.tgz"
	artifactDigest           = "sha512-/USq3R28PunkjZZfyweEgUAlR7npgrruZmb47j3oRRxlFWk5Jurj+THiwhl/AKxUgkxaxb35cr6DCAeXNlQ/jg=="
	credentialArtifact       = ".credentials.json"
	readOnlySettingsArtifact = "settings.json"
	mcpConfigFilename        = "claude-mcp.json"
	loginCallbackTimeout     = 300
	maxArtifactBytes         = int64(1 << 20)
)

var reservedMCPServerNames = map[string]struct{}{
	"workspace":        {},
	"claude-in-chrome": {},
	"computer-use":     {},
	"Claude Preview":   {},
	"Claude Browser":   {},
}

type adapter struct{}

var _ harness.Adapter = (*adapter)(nil)

// New returns the adapter for the pinned Linux ARM64 Claude Code baseline.
func New() harness.Adapter {
	return &adapter{}
}

func (*adapter) Name() harness.Name {
	return harness.Claude
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
	if stderr != "" || (stdout != version+" (Claude Code)" && stdout != version+" (Claude Code)\n") {
		return fmt.Errorf("claude executable version does not match pinned %s baseline", version)
	}
	return nil
}

func (a *adapter) Preflight(ctx context.Context, roots harness.RunRoots) ([]harness.Diagnostic, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := harness.ValidateRoots(roots); err != nil {
		return nil, fmt.Errorf("claude preflight: %w", err)
	}
	if err := harness.ValidateAuthLayout(a.AuthLayout()); err != nil {
		return nil, fmt.Errorf("claude preflight auth layout: %w", err)
	}
	return []harness.Diagnostic{{
		Severity: "warning",
		Code:     "claude.auth.macos-keychain-nonportable",
		Message:  "macOS Keychain credentials are not portable; DSX accepts only the Linux CLAUDE_CONFIG_DIR/.credentials.json file",
	}}, nil
}

func (*adapter) Invocation(request harness.InvocationRequest) (harness.ExecSpec, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.ExecSpec{}, fmt.Errorf("claude invocation: %w", err)
	}

	argv := []string{executable}
	if len(request.ReadOnlyConfig) != 0 {
		settingsPath := path.Join(request.Roots.ReadOnlyConfig, readOnlySettingsArtifact)
		if len(request.ReadOnlyConfig) != 1 || request.ReadOnlyConfig[0] != settingsPath {
			return harness.ExecSpec{}, errors.New("claude invocation received unallowlisted reviewed configuration")
		}
		argv = append(argv, "--settings", settingsPath)
	}
	if !request.Interactive {
		argv = append(argv, "-p", request.Prompt)
	}
	spec := harness.ExecSpec{
		Argv:     argv,
		Env:      isolatedEnvironment(request.Roots, request.Environment),
		Cwd:      request.Roots.Workspace,
		Terminal: request.Interactive,
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return harness.ExecSpec{}, fmt.Errorf("claude invocation: %w", err)
	}
	return spec, nil
}

func (*adapter) AuthLayout() harness.AuthLayout {
	return harness.AuthLayout{
		Backend:             harness.StorageFile,
		CredentialArtifacts: []string{credentialArtifact},
		ReadOnlyConfig:      []string{readOnlySettingsArtifact},
		MaxArtifactBytes:    maxArtifactBytes,
		Environment:         map[string]string{"CLAUDE_CONFIG_DIR": "."},
	}
}

func (*adapter) Seed(ctx context.Context, request harness.SeedRequest) error {
	request.MaxArtifactBytes = maxArtifactBytes
	for index := range request.Artifacts {
		if request.Artifacts[index] != credentialArtifact {
			return fmt.Errorf("claude seed artifact %d is not the portable credential file", index)
		}
	}
	credentialPath := filepath.Join(request.SourceRoot, credentialArtifact)
	if err := validateCredentialFile(credentialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("validate Claude credentials: %w", err)
	}
	if err := harness.SeedArtifacts(ctx, request); err != nil {
		return fmt.Errorf("claude seed: %w", err)
	}
	return nil
}

type credentialDocument struct {
	ClaudeAIOAuth oauthCredential `json:"claudeAiOauth"`
}

type oauthCredential struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"`
	Scopes           []string `json:"scopes"`
	SubscriptionType string   `json:"subscriptionType,omitempty"`
	RateLimitTier    string   `json:"rateLimitTier,omitempty"`
}

func validateCredentialFile(filePath string) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxArtifactBytes {
		return errors.New("credential JSON must be a non-empty regular file no larger than 1 MiB")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) || opened.Size() == 0 || opened.Size() > maxArtifactBytes {
		return errors.New("credential JSON changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxArtifactBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxArtifactBytes {
		return errors.New("credential JSON must be no larger than 1 MiB")
	}
	var document credentialDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode bounded credential object: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("credential JSON contains trailing data")
	}
	credential := document.ClaudeAIOAuth
	if credential.AccessToken == "" || credential.RefreshToken == "" || credential.ExpiresAt <= 0 {
		return errors.New("Claude OAuth credential requires accessToken, refreshToken, and positive expiresAt")
	}
	if len(credential.AccessToken) > 256<<10 || len(credential.RefreshToken) > 256<<10 {
		return errors.New("Claude OAuth token exceeds the supported bound")
	}
	if len(credential.Scopes) > 128 {
		return errors.New("Claude OAuth scope list exceeds the supported bound")
	}
	for _, scope := range credential.Scopes {
		if scope == "" || len(scope) > 256 {
			return errors.New("Claude OAuth scope is empty or exceeds the supported bound")
		}
	}
	if len(credential.SubscriptionType) > 256 || len(credential.RateLimitTier) > 256 {
		return errors.New("Claude OAuth metadata exceeds the supported bound")
	}
	return nil
}

func (*adapter) EphemeralMCP(request harness.MCPRequest) (harness.ConfigInjection, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.ConfigInjection{}, fmt.Errorf("claude MCP injection: %w", err)
	}

	servers := make(map[string]claudeMCPServer, len(request.Servers))
	for index, server := range request.Servers {
		if err := validateMCPServer(server); err != nil {
			return harness.ConfigInjection{}, fmt.Errorf("claude MCP server %d: %w", index, err)
		}
		if _, duplicate := servers[server.Name]; duplicate {
			return harness.ConfigInjection{}, fmt.Errorf("claude MCP server %d duplicates a server name", index)
		}
		if len(server.Command) != 0 {
			servers[server.Name] = claudeMCPServer{
				Type:    "stdio",
				Command: server.Command[0],
				Args:    append([]string(nil), server.Command[1:]...),
				Env:     cloneMap(server.Env),
			}
			continue
		}
		servers[server.Name] = claudeMCPServer{
			Type:    "http",
			URL:     server.URL,
			Headers: cloneMap(server.Env),
		}
	}

	data, err := json.Marshal(claudeMCPDocument{MCPServers: servers})
	if err != nil {
		return harness.ConfigInjection{}, fmt.Errorf("marshal claude MCP configuration: %w", err)
	}
	data = append(data, '\n')
	configPath := path.Join(request.Roots.Temporary, mcpConfigFilename)
	return harness.ConfigInjection{
		Files: []harness.GeneratedFile{{Path: configPath, Mode: 0o600, Data: data}},
		Args:  []string{"--mcp-config", configPath, "--strict-mcp-config"},
	}, nil
}

func (*adapter) Login(request harness.LoginRequest) (harness.LoginFlow, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.LoginFlow{}, fmt.Errorf("claude login: %w", err)
	}
	if request.CallbackURL != "" {
		return harness.LoginFlow{}, fmt.Errorf("claude %s does not expose a validated caller-supplied OAuth callback URI; guest callback delivery is unavailable", version)
	}

	argv := []string{executable}
	if len(request.ReadOnlyConfig) != 0 {
		settingsPath := path.Join(request.Roots.ReadOnlyConfig, readOnlySettingsArtifact)
		if len(request.ReadOnlyConfig) != 1 || request.ReadOnlyConfig[0] != settingsPath {
			return harness.LoginFlow{}, errors.New("claude login received unallowlisted reviewed configuration")
		}
		argv = append(argv, "--settings", settingsPath)
	}
	argv = append(argv, "auth", "login")
	spec := harness.ExecSpec{
		Argv:     argv,
		Env:      isolatedEnvironment(request.Roots, nil),
		Cwd:      request.Roots.Workspace,
		Terminal: true,
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return harness.LoginFlow{}, fmt.Errorf("claude login: %w", err)
	}
	return harness.LoginFlow{
		Exec:            spec,
		OpenBrowser:     true,
		CallbackTimeout: loginCallbackTimeout,
	}, nil
}

func (*adapter) RedactionRules() harness.RedactionRules {
	return harness.RedactionRules{
		EnvironmentKeys: []string{
			"ANTHROPIC_API_KEY",
			"ANTHROPIC_AUTH_TOKEN",
			"ANTHROPIC_AWS_API_KEY",
			"ANTHROPIC_CUSTOM_HEADERS",
			"ANTHROPIC_FOUNDRY_API_KEY",
			"ANTHROPIC_FOUNDRY_AUTH_TOKEN",
			"AWS_ACCESS_KEY_ID",
			"AWS_BEARER_TOKEN_BEDROCK",
			"AWS_SECRET_ACCESS_KEY",
			"AWS_SESSION_TOKEN",
			"CLAUDE_CODE_CLIENT_KEY",
			"CLAUDE_CODE_CLIENT_KEY_PASSPHRASE",
			"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
			"CLAUDE_CODE_OAUTH_TOKEN",
			"MCP_CLIENT_SECRET",
		},
		PathPrefixes: []string{credentialArtifact, mcpConfigFilename},
	}
}

func isolatedEnvironment(roots harness.RunRoots, supplied map[string]string) map[string]string {
	environment := cloneMap(supplied)
	environment["HOME"] = roots.Home
	environment["CLAUDE_CONFIG_DIR"] = roots.Auth
	environment["XDG_CONFIG_HOME"] = roots.Config
	environment["XDG_DATA_HOME"] = roots.Data
	environment["XDG_CACHE_HOME"] = roots.Cache
	environment["TMPDIR"] = roots.Temporary
	environment["TMP"] = roots.Temporary
	environment["TEMP"] = roots.Temporary
	environment["CLAUDE_CODE_TMPDIR"] = roots.Temporary
	return environment
}

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func validateMCPServer(server harness.MCPServer) error {
	if !validMCPServerName(server.Name) {
		return fmt.Errorf("name must contain only letters, numbers, hyphens, and underscores")
	}
	if _, reserved := reservedMCPServerNames[server.Name]; reserved {
		return fmt.Errorf("name is reserved by Claude Code")
	}
	if (len(server.Command) == 0) == (server.URL == "") {
		return fmt.Errorf("exactly one of command or URL is required")
	}
	if len(server.Command) != 0 {
		for _, argument := range server.Command {
			if argument == "" || strings.TrimSpace(argument) != argument || strings.IndexByte(argument, 0) >= 0 {
				return fmt.Errorf("command contains an empty, padded, or invalid argument")
			}
		}
		for key, value := range server.Env {
			if !validEnvironmentKey(key) || strings.TrimSpace(key) != key || strings.TrimSpace(value) != value || strings.IndexByte(value, 0) >= 0 {
				return fmt.Errorf("command environment contains an invalid entry")
			}
		}
		return nil
	}

	parsed, err := url.Parse(server.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" || strings.TrimSpace(server.URL) != server.URL {
		return fmt.Errorf("URL must be an absolute HTTP(S) URL without user information or a fragment")
	}
	for key, value := range server.Env {
		if !validHTTPHeaderName(key) || strings.TrimSpace(key) != key || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("HTTP headers contain an invalid entry")
		}
	}
	return nil
}

func validMCPServerName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func validEnvironmentKey(key string) bool {
	if key == "" || strings.ContainsAny(key, "=\x00") {
		return false
	}
	return true
}

func validHTTPHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", character) {
			continue
		}
		return false
	}
	return true
}

type claudeMCPDocument struct {
	MCPServers map[string]claudeMCPServer `json:"mcpServers"`
}

type claudeMCPServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}
