package plan

import (
	"context"

	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/model"
)

type Resolver interface {
	Resolve(context.Context, ResolveInput) (ExecutionPlan, []config.Diagnostic, error)
}

type ResolveInput struct {
	Config    config.ValidatedConfig
	Project   ProjectIdentity
	Sandbox   SandboxIdentity
	Mode      model.WorkspaceMode
	Ownership OwnershipPlan
	CLI       CLIOverrides
	Imported  []ImportedValue
	Defaults  DefaultValues
	Authority AuthorityInputs
}

type CLIOverrides struct {
	Agent   string
	Browser *bool
	CPUs    *int
	Memory  string
}

type ImportedValue struct {
	Pointer string
	Value   any
	Source  config.SourceRef
}

type DefaultValues struct {
	ImageRef            string
	Agent               string
	Internet            bool
	CPUs                int
	MemoryBytes         int64
	MaxConcurrentClones int
}

type AuthorityInputs struct {
	BuildContext          *ContentDigest
	StandardImageDigest   string
	ImportedContent       []ContentDigest
	BrowserImageReference string
	BrowserImageDigest    string
	HostMounts            []HostMountAuthority
	LeappDirectory        *HostMountAuthority
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
