package plan

import (
	"context"

	"github.com/srimajji/dsx/internal/config"
)

type Resolver interface {
	Resolve(context.Context, ResolveInput) (ExecutionPlan, []config.Diagnostic, error)
}

type ResolveInput struct {
	Config    config.ValidatedConfig
	Project   ProjectIdentity
	CLI       CLIOverrides
	Imported  []ImportedValue
	Defaults  DefaultValues
	Authority AuthorityInputs
}

type CLIOverrides struct {
	CPUs   *int
	Memory string
}

type ImportedValue struct {
	Pointer string
	Value   any
	Source  config.SourceRef
}

type DefaultValues struct {
	ImageRef                string
	DefaultAgent            string
	Internet                bool
	CPUs                    int
	MemoryBytes             int64
	MaxConcurrentWorkspaces int
}

type AuthorityInputs struct {
	BuildContext            *ContentDigest
	StandardImageDigest     string
	ImportedContent         []ContentDigest
	BrowserImageReference   string
	BrowserImageDigest      string
	HostMounts              []HostMountAuthority
	HostDefaultAWSDirectory *HostMountAuthority
}

type ContentDigest struct {
	Path   string
	Digest string
}

type HostMountAuthority struct {
	DeclaredPath  string
	CanonicalPath string
	Identity      string
}
