package opencode

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
	"path/filepath"
	"reflect"
	"strings"
)

const (
	executable              = "opencode"
	credentialArtifact      = "opencode/auth.json"
	ephemeralConfigEnv      = "OPENCODE_CONFIG_CONTENT"
	projectConfigDisableEnv = "OPENCODE_DISABLE_PROJECT_CONFIG"
	artifactSource          = "https://registry.npmjs.org/opencode-linux-arm64/-/opencode-linux-arm64-1.18.15.tgz"
	artifactIntegrity       = "sha512-lJ+pPrJOxo3U2HeXis9aN/vrSFf1iXZXC9S0mTSWtm7qnFOJ3SLI7ALf7NKZoRHOOKUs9RfjR1DMLjzBcGAXog=="
	maxArtifactBytes        = int64(1 << 20)
)

type adapter struct{}

var _ harness.MCPVerifier = adapter{}

// New returns the adapter for the pinned OpenCode Linux arm64 artifact.
func New() harness.Adapter {
	return adapter{}
}

func (adapter) Name() harness.Name {
	return harness.OpenCode
}

func (adapter) Version() harness.PinnedArtifact {
	return harness.PinnedArtifact{
		Version:    "1.18.15",
		Source:     artifactSource,
		Digest:     artifactIntegrity,
		Executable: executable,
	}
}
func (adapter) ValidateVersion(stdout, stderr string) error {
	if stdout != "1.18.15\n" || stderr != "" {
		return fmt.Errorf("OpenCode executable version output does not match pinned 1.18.15")
	}
	return nil
}

func (a adapter) Preflight(ctx context.Context, roots harness.RunRoots) ([]harness.Diagnostic, error) {
	if err := harness.ValidateRoots(roots); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := harness.ValidateAuthLayout(a.AuthLayout()); err != nil {
		return nil, fmt.Errorf("invalid OpenCode auth layout: %w", err)
	}
	return nil, nil
}

func (adapter) Invocation(request harness.InvocationRequest) (harness.ExecSpec, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.ExecSpec{}, err
	}

	var argv []string
	if request.Interactive {
		if request.Prompt != "" {
			return harness.ExecSpec{}, fmt.Errorf("OpenCode interactive invocation does not accept a prompt")
		}
		argv = []string{executable}
	} else {
		if strings.TrimSpace(request.Prompt) == "" {
			return harness.ExecSpec{}, fmt.Errorf("OpenCode one-shot invocation requires a prompt")
		}
		// The complete prompt is one argv element. OpenCode's `run [message..]`
		// accepts it directly; no shell or re-tokenization is involved.
		argv = []string{executable, "run", request.Prompt}
	}

	spec := harness.ExecSpec{
		Argv:     argv,
		Env:      isolatedEnvironment(request.Roots, request.Environment),
		Cwd:      request.Roots.Workspace,
		Terminal: request.Interactive,
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return harness.ExecSpec{}, err
	}
	return spec, nil
}

func (adapter) AuthLayout() harness.AuthLayout {
	return harness.AuthLayout{
		Backend:             harness.StorageFile,
		CredentialArtifacts: []string{credentialArtifact},
		MaxArtifactBytes:    maxArtifactBytes,
		Environment: map[string]string{
			// OpenCode stores auth.json below $XDG_DATA_HOME/opencode.
			// AuthLayout values are relative to the per-run credential root.
			"XDG_DATA_HOME": ".",
		},
	}
}
func (adapter) Seed(ctx context.Context, request harness.SeedRequest) error {
	request.MaxArtifactBytes = maxArtifactBytes
	for _, artifact := range request.Artifacts {
		if artifact != credentialArtifact {
			return fmt.Errorf("OpenCode seed contains a non-credential artifact")
		}
	}
	credentialPath := filepath.Join(request.SourceRoot, filepath.FromSlash(credentialArtifact))
	if err := validateCredentialFile(credentialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("validate OpenCode credentials: %w", err)
	}
	return harness.SeedArtifacts(ctx, request)
}

type credentialType struct {
	Type string `json:"type"`
}

type oauthCredential struct {
	Type          string `json:"type"`
	Refresh       string `json:"refresh"`
	Access        string `json:"access"`
	Expires       int64  `json:"expires"`
	AccountID     string `json:"accountId,omitempty"`
	EnterpriseURL string `json:"enterpriseUrl,omitempty"`
}

type apiCredential struct {
	Type     string            `json:"type"`
	Key      string            `json:"key"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type wellKnownCredential struct {
	Type  string `json:"type"`
	Key   string `json:"key"`
	Token string `json:"token"`
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
	var document map[string]json.RawMessage
	if err := decodeStrictJSON(data, &document, false); err != nil {
		return fmt.Errorf("decode bounded credential object: %w", err)
	}
	if len(document) > 128 {
		return errors.New("OpenCode credential provider count exceeds the supported bound")
	}
	for provider, raw := range document {
		if provider == "" || strings.TrimSpace(provider) != provider || len(provider) > 512 {
			return errors.New("OpenCode credential provider name is invalid")
		}
		var tagged credentialType
		if err := json.Unmarshal(raw, &tagged); err != nil {
			return fmt.Errorf("decode OpenCode credential %q type: %w", provider, err)
		}
		switch tagged.Type {
		case "oauth":
			var credential oauthCredential
			if err := decodeStrictJSON(raw, &credential, true); err != nil {
				return fmt.Errorf("decode OpenCode OAuth credential %q: %w", provider, err)
			}
			if credential.Refresh == "" || credential.Access == "" || credential.Expires < 0 {
				return fmt.Errorf("OpenCode OAuth credential %q is incomplete", provider)
			}
			if exceedsCredentialStringBound(credential.Refresh, credential.Access, credential.AccountID, credential.EnterpriseURL) {
				return fmt.Errorf("OpenCode OAuth credential %q exceeds the supported bound", provider)
			}
		case "api":
			var credential apiCredential
			if err := decodeStrictJSON(raw, &credential, true); err != nil {
				return fmt.Errorf("decode OpenCode API credential %q: %w", provider, err)
			}
			if credential.Key == "" || exceedsCredentialStringBound(credential.Key) || len(credential.Metadata) > 64 {
				return fmt.Errorf("OpenCode API credential %q is invalid", provider)
			}
			for key, value := range credential.Metadata {
				if key == "" || len(key) > 512 || len(value) > 4096 {
					return fmt.Errorf("OpenCode API credential %q metadata exceeds the supported bound", provider)
				}
			}
		case "wellknown":
			var credential wellKnownCredential
			if err := decodeStrictJSON(raw, &credential, true); err != nil {
				return fmt.Errorf("decode OpenCode well-known credential %q: %w", provider, err)
			}
			if credential.Key == "" || credential.Token == "" || exceedsCredentialStringBound(credential.Key, credential.Token) {
				return fmt.Errorf("OpenCode well-known credential %q is invalid", provider)
			}
		default:
			return fmt.Errorf("OpenCode credential %q has unsupported type %q", provider, tagged.Type)
		}
	}
	return nil
}

func decodeStrictJSON(data []byte, destination any, disallowUnknown bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON contains trailing data")
	}
	return nil
}

func exceedsCredentialStringBound(values ...string) bool {
	for _, value := range values {
		if len(value) > 256<<10 {
			return true
		}
	}
	return false
}

type configDocument struct {
	MCP map[string]mcpConfig `json:"mcp"`
}

type mcpConfig struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command,omitempty"`
	URL         string            `json:"url,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

func (adapter) EphemeralMCP(request harness.MCPRequest) (harness.ConfigInjection, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.ConfigInjection{}, err
	}
	servers := make(map[string]mcpConfig, len(request.Servers))

	for _, server := range request.Servers {
		if !validConfigKey(server.Name) {
			return harness.ConfigInjection{}, fmt.Errorf("OpenCode MCP server name is invalid")
		}
		if _, duplicate := servers[server.Name]; duplicate {
			return harness.ConfigInjection{}, fmt.Errorf("OpenCode MCP server names must be unique")
		}

		hasCommand := len(server.Command) != 0
		hasURL := server.URL != ""
		if hasCommand == hasURL {
			return harness.ConfigInjection{}, fmt.Errorf("OpenCode MCP server requires exactly one of command or URL")
		}
		if hasCommand {
			if err := validateEnvironment(server.Env); err != nil {
				return harness.ConfigInjection{}, fmt.Errorf("OpenCode MCP environment is invalid: %w", err)
			}
		}

		if hasCommand {
			if strings.TrimSpace(server.Command[0]) == "" {
				return harness.ConfigInjection{}, fmt.Errorf("OpenCode MCP command executable is required")
			}
			for _, argument := range server.Command {
				if strings.IndexByte(argument, 0) >= 0 {
					return harness.ConfigInjection{}, fmt.Errorf("OpenCode MCP command contains NUL")
				}
			}
			servers[server.Name] = mcpConfig{
				Type:        "local",
				Command:     append([]string(nil), server.Command...),
				Environment: copyEnvironment(server.Env),
			}
			continue
		}

		parsed, err := url.Parse(server.URL)
		if err != nil ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.Host == "" ||
			parsed.User != nil ||
			parsed.Fragment != "" ||
			strings.TrimSpace(server.URL) != server.URL {
			return harness.ConfigInjection{}, fmt.Errorf("OpenCode remote MCP URL is invalid")
		}
		if err := validateHeaders(server.Env); err != nil {
			return harness.ConfigInjection{}, fmt.Errorf("OpenCode MCP headers are invalid: %w", err)
		}
		servers[server.Name] = mcpConfig{
			Type:    "remote",
			URL:     server.URL,
			Headers: copyEnvironment(server.Env),
		}
	}

	data, err := json.Marshal(configDocument{MCP: servers})
	if err != nil {
		return harness.ConfigInjection{}, fmt.Errorf("encode OpenCode ephemeral MCP configuration: %w", err)
	}

	// In 1.18.15 OPENCODE_CONFIG_CONTENT is merged after global, explicit,
	// project, and config-directory files. An OPENCODE_CONFIG file would be
	// merged earlier and could not guarantee the run-private MCP override.
	return harness.ConfigInjection{
		Env: map[string]string{ephemeralConfigEnv: string(data)},
	}, nil
}
func (a adapter) MCPVerification(request harness.MCPRequest, injection harness.ConfigInjection) (harness.ExecSpec, error) {
	expected, err := a.EphemeralMCP(request)
	if err != nil {
		return harness.ExecSpec{}, err
	}
	if !reflect.DeepEqual(injection, expected) {
		return harness.ExecSpec{}, fmt.Errorf("OpenCode MCP verification received a mismatched injection")
	}
	environment := isolatedEnvironment(request.Roots, nil)
	for key, value := range injection.Env {
		environment[key] = value
	}
	spec := harness.ExecSpec{
		Argv: []string{executable, "debug", "config"},
		Env:  environment,
		Cwd:  request.Roots.Workspace,
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return harness.ExecSpec{}, err
	}
	return spec, nil
}

func (a adapter) ValidateEffectiveMCP(request harness.MCPRequest, stdout, stderr string) error {
	if stderr != "" {
		return fmt.Errorf("OpenCode effective MCP inspection wrote unexpected diagnostic output")
	}
	expected, err := a.EphemeralMCP(request)
	if err != nil {
		return err
	}
	var expectedDocument, effectiveDocument struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(expected.Env[ephemeralConfigEnv]), &expectedDocument); err != nil {
		return fmt.Errorf("decode expected OpenCode MCP registry: %w", err)
	}
	if err := json.Unmarshal([]byte(stdout), &effectiveDocument); err != nil {
		return fmt.Errorf("decode effective OpenCode configuration")
	}
	if expectedDocument.MCP == nil {
		expectedDocument.MCP = map[string]json.RawMessage{}
	}
	if effectiveDocument.MCP == nil {
		effectiveDocument.MCP = map[string]json.RawMessage{}
	}
	if !equalJSONRegistry(expectedDocument.MCP, effectiveDocument.MCP) {
		return fmt.Errorf("OpenCode effective MCP registry differs from the requested run-private registry")
	}
	return nil
}

func equalJSONRegistry(expected, effective map[string]json.RawMessage) bool {
	if len(expected) != len(effective) {
		return false
	}
	for name, expectedRaw := range expected {
		effectiveRaw, found := effective[name]
		if !found {
			return false
		}
		var expectedValue, effectiveValue any
		if json.Unmarshal(expectedRaw, &expectedValue) != nil || json.Unmarshal(effectiveRaw, &effectiveValue) != nil ||
			!reflect.DeepEqual(expectedValue, effectiveValue) {
			return false
		}
	}
	return true
}

func (adapter) Login(request harness.LoginRequest) (harness.LoginFlow, error) {
	if err := harness.ValidateRoots(request.Roots); err != nil {
		return harness.LoginFlow{}, err
	}
	if request.CallbackURL != "" {
		// `opencode auth login [url]` treats url as an auth-provider metadata
		// endpoint, not an OAuth callback. Passing a DSX callback would silently
		// request different semantics, so the adapter rejects it.
		return harness.LoginFlow{}, fmt.Errorf("OpenCode provider login does not accept a callback URL")
	}

	spec := harness.ExecSpec{
		Argv:     []string{executable, "auth", "login"},
		Env:      isolatedEnvironment(request.Roots, nil),
		Cwd:      request.Roots.Workspace,
		Terminal: true,
	}
	if err := harness.ValidateExecSpec(spec); err != nil {
		return harness.LoginFlow{}, err
	}
	return harness.LoginFlow{Exec: spec}, nil
}

func (adapter) RedactionRules() harness.RedactionRules {
	return harness.RedactionRules{
		EnvironmentKeys: []string{ephemeralConfigEnv},
		PathPrefixes:    []string{credentialArtifact},
	}
}

func isolatedEnvironment(roots harness.RunRoots, input map[string]string) map[string]string {
	environment := copyEnvironment(input)
	environment["HOME"] = roots.Home
	environment["XDG_CONFIG_HOME"] = roots.Config
	environment["XDG_DATA_HOME"] = roots.Auth
	environment["XDG_CACHE_HOME"] = roots.Cache
	environment["XDG_STATE_HOME"] = roots.Data
	environment["XDG_RUNTIME_DIR"] = roots.Temporary
	environment["TMPDIR"] = roots.Temporary
	environment["OPENCODE_DISABLE_AUTOUPDATE"] = "1"
	// OpenCode 1.18.15's config loader gates ConfigPaths project files and
	// project .opencode directory discovery on this flag.
	environment[projectConfigDisableEnv] = "1"
	// OpenCode gives this test hook precedence over os.UserHomeDir; pin it so
	// even an inherited value cannot redirect reads to an ambient home.
	environment["OPENCODE_TEST_HOME"] = roots.Home
	// Block ambient OpenCode-specific config. A run-ephemeral injection is
	// deliberately merged over these values by the caller.
	environment["OPENCODE_CONFIG"] = ""
	environment["OPENCODE_CONFIG_DIR"] = roots.Config + "/opencode"
	environment[ephemeralConfigEnv] = ""
	return environment
}

func copyEnvironment(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func validateEnvironment(environment map[string]string) error {
	for key, value := range environment {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("invalid environment entry %q", key)
		}
	}
	return nil
}

func validateHeaders(headers map[string]string) error {
	for key, value := range headers {
		if !validHTTPHeaderName(key) || strings.TrimSpace(key) != key || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("invalid HTTP header")
		}
	}
	return nil
}

func validConfigKey(value string) bool {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return false
	}
	for index := range len(value) {
		if value[index] < 0x20 || value[index] == 0x7f {
			return false
		}
	}
	return true
}

func validHTTPHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if character <= 0x20 || character >= 0x7f || strings.ContainsRune("()<>@,;:\\\"/[]?={}", rune(character)) {
			return false
		}
	}
	return true
}
