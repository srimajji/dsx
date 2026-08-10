package harness

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
)

type Name string

const (
	OMP      Name = "omp"
	Codex    Name = "codex"
	Claude   Name = "claude"
	OpenCode Name = "opencode"
)

func ParseName(value string) (Name, error) {
	name := Name(value)
	switch name {
	case OMP, Codex, Claude, OpenCode:
		return name, nil
	default:
		return "", fmt.Errorf("unsupported harness %q", value)
	}
}

type PinnedArtifact struct {
	Version    string `json:"version"`
	Source     string `json:"source"`
	Digest     string `json:"digest"`
	Executable string `json:"executable"`
}

type RunRoots struct {
	Workspace      string `json:"workspace"`
	Home           string `json:"home"`
	Auth           string `json:"auth"`
	Config         string `json:"config"`
	ReadOnlyConfig string `json:"read_only_config"`
	Data           string `json:"data"`
	Cache          string `json:"cache"`
	Temporary      string `json:"temporary"`
}

type Diagnostic struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

type InvocationRequest struct {
	Roots          RunRoots
	Prompt         string
	Interactive    bool
	Environment    map[string]string
	ReadOnlyConfig []string
}

type ExecSpec struct {
	Argv     []string          `json:"argv"`
	Env      map[string]string `json:"env,omitempty"`
	Cwd      string            `json:"cwd"`
	Terminal bool              `json:"terminal"`
}

type StorageBackend string

const (
	StorageFile   StorageBackend = "file"
	StorageSQLite StorageBackend = "sqlite"
)

const MaxAuthArtifactBytes = int64(16 << 20)

type AuthLayout struct {
	Backend             StorageBackend `json:"backend"`
	CredentialArtifacts []string       `json:"credential_artifacts"`
	ReadOnlyConfig      []string       `json:"read_only_config,omitempty"`
	// MaxArtifactBytes is the per-file bound for seed, fingerprint, guest transfer, and promotion.
	MaxArtifactBytes int64 `json:"max_artifact_bytes"`
	// Environment maps variable names to clean paths relative to the per-run Auth root; "." means the root itself.
	Environment map[string]string `json:"environment"`
}

type SeedRequest struct {
	SourceRoot       string
	DestinationRoot  string
	Artifacts        []string
	MaxArtifactBytes int64
}

type MCPServer struct {
	Name    string            `json:"name"`
	Command []string          `json:"command,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type MCPRequest struct {
	Roots   RunRoots
	Servers []MCPServer
}

type GeneratedFile struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Data []byte `json:"data"`
}

type ConfigInjection struct {
	Files []GeneratedFile   `json:"files,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
	// Args are inserted immediately after the executable and before the adapter invocation arguments.
	Args []string `json:"args,omitempty"`
}

// MCPVerifier is implemented by adapters whose pinned CLI merges inherited
// MCP configuration and therefore requires a fail-closed effective-registry
// check before the harness process starts.
type MCPVerifier interface {
	MCPVerification(MCPRequest, ConfigInjection) (ExecSpec, error)
	ValidateEffectiveMCP(MCPRequest, string, string) error
}

type LoginRequest struct {
	Roots          RunRoots
	CallbackURL    string
	ReadOnlyConfig []string
}

type LoginFlow struct {
	Exec            ExecSpec
	OpenBrowser     bool
	CallbackTimeout int
}

type RedactionRules struct {
	EnvironmentKeys []string `json:"environment_keys,omitempty"`
	PathPrefixes    []string `json:"path_prefixes,omitempty"`
}

type Adapter interface {
	Name() Name
	Version() PinnedArtifact
	ValidateVersion(stdout, stderr string) error
	Preflight(context.Context, RunRoots) ([]Diagnostic, error)
	Invocation(InvocationRequest) (ExecSpec, error)
	AuthLayout() AuthLayout
	Seed(context.Context, SeedRequest) error
	EphemeralMCP(MCPRequest) (ConfigInjection, error)
	Login(LoginRequest) (LoginFlow, error)
	RedactionRules() RedactionRules
}

func ValidateRoots(roots RunRoots) error {
	values := []struct {
		name  string
		value string
	}{
		{"workspace", roots.Workspace}, {"home", roots.Home}, {"auth", roots.Auth},
		{"config", roots.Config}, {"read-only config", roots.ReadOnlyConfig}, {"data", roots.Data}, {"cache", roots.Cache}, {"temporary", roots.Temporary},
	}
	seen := make(map[string]string, len(values))
	for _, item := range values {
		if !cleanGuestAbsolute(item.value) {
			return fmt.Errorf("%s root must be a clean absolute guest path", item.name)
		}
		if previous, ok := seen[item.value]; ok {
			return fmt.Errorf("%s and %s roots must be distinct", previous, item.name)
		}
		seen[item.value] = item.name
	}
	return nil
}

func ValidateExecSpec(spec ExecSpec) error {
	if len(spec.Argv) == 0 || strings.TrimSpace(spec.Argv[0]) == "" {
		return fmt.Errorf("executable argv is required")
	}
	for _, argument := range spec.Argv {
		if strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("argv contains NUL")
		}
	}
	if !cleanGuestAbsolute(spec.Cwd) {
		return fmt.Errorf("working directory must be a clean absolute guest path")
	}
	for key, value := range spec.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("invalid environment entry %q", key)
		}
	}
	return nil
}

func ValidateAuthLayout(layout AuthLayout) error {
	if layout.Backend != StorageFile && layout.Backend != StorageSQLite {
		return fmt.Errorf("unsupported auth storage backend %q", layout.Backend)
	}
	if len(layout.CredentialArtifacts) == 0 {
		return fmt.Errorf("credential artifact allowlist is required")
	}
	if layout.MaxArtifactBytes <= 0 || layout.MaxArtifactBytes > MaxAuthArtifactBytes {
		return fmt.Errorf("auth artifact size limit must be between 1 and %d bytes", MaxAuthArtifactBytes)
	}
	seen := make(map[string]struct{}, len(layout.CredentialArtifacts)+len(layout.ReadOnlyConfig))
	for _, artifact := range append(append([]string(nil), layout.CredentialArtifacts...), layout.ReadOnlyConfig...) {
		if !cleanRelative(artifact) {
			return fmt.Errorf("artifact %q must be a clean relative path", artifact)
		}
		if _, exists := seen[artifact]; exists {
			return fmt.Errorf("duplicate auth artifact %q", artifact)
		}
		seen[artifact] = struct{}{}
	}
	return nil
}

func SortedEnvironment(environment map[string]string) []string {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+environment[key])
	}
	return result
}

func cleanGuestAbsolute(value string) bool {
	return strings.HasPrefix(value, "/") && value != "/" && path.Clean(value) == value && !strings.Contains(value, "\x00")
}

func cleanRelative(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && path.Clean(value) == value && value != "." && value != ".." && !strings.HasPrefix(value, "../") && !strings.Contains(value, "\x00")
}
