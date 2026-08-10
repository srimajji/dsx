package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/srimajji/dsx/internal/harness"
)

const (
	version          = "rust-v0.147.0"
	versionOutput    = "codex-cli 0.147.0\n"
	artifactURL      = "https://github.com/openai/codex/releases/download/rust-v0.147.0/codex-package-aarch64-unknown-linux-musl.tar.gz"
	artifactHash     = "sha256:89cbf79bd5ae6f9c58da47e8079f311c84219350c9c43c070d42f3e9b2a81401"
	authArtifact     = "auth.json"
	maxArtifactBytes = int64(8 << 20)
)

type adapter struct{}

var _ harness.Adapter = adapter{}

func New() harness.Adapter { return adapter{} }

func (adapter) Name() harness.Name { return harness.Codex }

func (adapter) Version() harness.PinnedArtifact {
	return harness.PinnedArtifact{
		Version:    version,
		Source:     artifactURL,
		Digest:     artifactHash,
		Executable: "codex",
	}
}

func (adapter) ValidateVersion(stdout, stderr string) error {
	if stdout != versionOutput || stderr != "" {
		return fmt.Errorf("codex version output does not match pinned %s", version)
	}
	return nil
}

func (a adapter) Preflight(ctx context.Context, roots harness.RunRoots) ([]harness.Diagnostic, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := harness.ValidateRoots(roots); err != nil {
		return nil, fmt.Errorf("codex preflight: %w", err)
	}
	if err := harness.ValidateAuthLayout(a.AuthLayout()); err != nil {
		return nil, fmt.Errorf("codex preflight auth layout: %w", err)
	}
	return nil, nil
}

func (adapter) Invocation(request harness.InvocationRequest) (harness.ExecSpec, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.ExecSpec{}, fmt.Errorf("codex invocation: %w", err)
	}

	argv := []string{"codex"}
	if request.Interactive {
		if request.Prompt != "" {
			argv = append(argv, request.Prompt)
		}
	} else {
		if strings.TrimSpace(request.Prompt) == "" {
			return harness.ExecSpec{}, fmt.Errorf("codex one-shot invocation requires a prompt")
		}
		argv = append(argv, "exec", request.Prompt)
	}

	spec := harness.ExecSpec{
		Argv:     argv,
		Env:      runEnvironment(request.Roots, request.Environment),
		Cwd:      request.Roots.Workspace,
		Terminal: request.Interactive,
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return harness.ExecSpec{}, fmt.Errorf("codex invocation: %w", err)
	}
	return spec, nil
}

func (adapter) AuthLayout() harness.AuthLayout {
	return harness.AuthLayout{
		Backend:             harness.StorageFile,
		CredentialArtifacts: []string{authArtifact},
		MaxArtifactBytes:    maxArtifactBytes,
		Environment:         map[string]string{"CODEX_HOME": "."},
	}
}

func (adapter) Seed(ctx context.Context, request harness.SeedRequest) error {
	request.MaxArtifactBytes = maxArtifactBytes
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.SourceRoot == "" || request.DestinationRoot == "" {
		return fmt.Errorf("codex seed source and destination roots are required")
	}
	seen := make(map[string]struct{}, len(request.Artifacts))
	for _, artifact := range request.Artifacts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if artifact != authArtifact {
			return fmt.Errorf("codex seed artifact is not credential allowlisted")
		}
		if _, exists := seen[artifact]; exists {
			return fmt.Errorf("codex seed contains a duplicate credential artifact")
		}
		seen[artifact] = struct{}{}
		if err := validateSeedAuth(filepath.Join(request.SourceRoot, authArtifact)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("codex seed auth artifact: %w", err)
		}
	}
	if err := harness.SeedArtifacts(ctx, request); err != nil {
		return fmt.Errorf("codex seed: %w", err)
	}
	return nil
}

func (adapter) EphemeralMCP(request harness.MCPRequest) (harness.ConfigInjection, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.ConfigInjection{}, fmt.Errorf("codex MCP injection: %w", err)
	}

	servers := append([]harness.MCPServer(nil), request.Servers...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	args := make([]string, 0, len(servers)*2+2)
	// The pinned CLI applies repeated overrides in order. Replace the complete
	// registry first so neither same-name nor differently named project MCP
	// servers survive, then install only this run's requested servers.
	args = append(args, "-c", "mcp_servers={}")
	for index, server := range servers {
		if index > 0 && server.Name == servers[index-1].Name {
			return harness.ConfigInjection{}, fmt.Errorf("codex MCP injection contains a duplicate server name")
		}
		if err := validateServerName(server.Name); err != nil {
			return harness.ConfigInjection{}, fmt.Errorf("codex MCP server name: %w", err)
		}
		if len(server.Env) != 0 {
			return harness.ConfigInjection{}, fmt.Errorf("codex MCP server environment is unsupported because the frozen contract cannot mark arbitrary values secret")
		}
		serverConfig, err := renderServer(server)
		if err != nil {
			return harness.ConfigInjection{}, err
		}
		args = append(args, "-c", "mcp_servers."+server.Name+"="+serverConfig)
	}
	return harness.ConfigInjection{Args: args}, nil
}

func (adapter) Login(request harness.LoginRequest) (harness.LoginFlow, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.LoginFlow{}, fmt.Errorf("codex login: %w", err)
	}
	if request.CallbackURL != "" {
		return harness.LoginFlow{}, fmt.Errorf("codex %s does not support a caller-provided login callback URL", version)
	}

	spec := harness.ExecSpec{
		Argv:     []string{"codex", "login", "--device-auth"},
		Env:      runEnvironment(request.Roots, nil),
		Cwd:      request.Roots.Workspace,
		Terminal: true,
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return harness.LoginFlow{}, fmt.Errorf("codex login: %w", err)
	}
	return harness.LoginFlow{Exec: spec, OpenBrowser: false, CallbackTimeout: 0}, nil
}

func (adapter) RedactionRules() harness.RedactionRules {
	return harness.RedactionRules{
		EnvironmentKeys: []string{
			"CODEX_ACCESS_TOKEN",
			"CODEX_API_KEY",
			"OPENAI_API_KEY",
		},
		PathPrefixes: []string{authArtifact},
	}
}

func runEnvironment(roots harness.RunRoots, requested map[string]string) map[string]string {
	environment := make(map[string]string, len(requested)+7)
	for key, value := range requested {
		environment[key] = value
	}
	// Isolation values deliberately replace request and ambient values. CODEX_HOME
	// is per-run writable state; only auth.json is ever snapshotted as credential.
	environment["HOME"] = roots.Home
	environment["CODEX_HOME"] = roots.Auth
	environment["CODEX_SQLITE_HOME"] = roots.Data
	environment["XDG_CONFIG_HOME"] = roots.Config
	environment["XDG_DATA_HOME"] = roots.Data
	environment["XDG_CACHE_HOME"] = roots.Cache
	environment["TMPDIR"] = roots.Temporary
	return environment
}

func validateSeedAuth(path string) error {
	if err := harness.IsRegularPrivateFile(path); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > maxArtifactBytes {
		return fmt.Errorf("credential file exceeds size limit")
	}

	decoder := json.NewDecoder(io.LimitReader(file, maxArtifactBytes+1))
	var document map[string]json.RawMessage
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("credential file is not valid JSON")
	}
	if document == nil {
		return fmt.Errorf("credential file must contain a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("credential file has trailing data")
	}
	return nil
}

func renderServer(server harness.MCPServer) (string, error) {
	hasCommand := len(server.Command) > 0
	hasURL := server.URL != ""
	if hasCommand == hasURL {
		return "", fmt.Errorf("codex MCP server must set exactly one transport")
	}

	var config strings.Builder
	config.WriteByte('{')
	if hasCommand {
		if strings.TrimSpace(server.Command[0]) == "" {
			return "", fmt.Errorf("codex MCP server has an empty command")
		}
		for _, argument := range server.Command {
			if strings.IndexByte(argument, 0) >= 0 {
				return "", fmt.Errorf("codex MCP server command contains NUL")
			}
		}
		config.WriteString("command = ")
		config.WriteString(tomlString(server.Command[0]))
		if len(server.Command) > 1 {
			config.WriteString(", args = ")
			config.WriteString(tomlStrings(server.Command[1:]))
		}
	} else {
		parsed, err := url.Parse(server.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("codex MCP server URL must be an absolute HTTP(S) URL without user info, query, or fragment")
		}
		config.WriteString("url = ")
		config.WriteString(tomlString(server.URL))
	}
	config.WriteString(", required = true }")
	return config.String(), nil
}

func validateServerName(name string) error {
	if name == "" {
		return fmt.Errorf("must be non-empty")
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' || character == '-') {
			return fmt.Errorf("must contain only ASCII letters, digits, underscore, or hyphen")
		}
	}
	return nil
}

func tomlString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func tomlStrings(values []string) string {
	var result strings.Builder
	result.WriteByte('[')
	for index, value := range values {
		if index > 0 {
			result.WriteString(", ")
		}
		result.WriteString(tomlString(value))
	}
	result.WriteByte(']')
	return result.String()
}
