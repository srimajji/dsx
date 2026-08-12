package app

import (
	"context"
	"io"

	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

type Services interface {
	Queries
	Commands
}

type Queries interface {
	Inspect(context.Context, InspectRequest) (InspectResult, error)
	Doctor(context.Context, DoctorRequest) (DoctorResult, error)
	BareState(context.Context, BareStateRequest) (BareState, error)
	PreviewSetup(context.Context, SetupPreviewRequest) (SetupPreview, error)
}

type Commands interface {
	Initialize(context.Context, InitializeRequest) (InitializeResult, error)
}

type InspectRequest struct {
	Root         string
	CLIOverrides CLIOverrides
}

type CLIOverrides struct {
	CPUs   *int
	Memory string
}

type InspectResult struct {
	Facts       ProjectFacts        `json:"facts"`
	Diagnostics []config.Diagnostic `json:"diagnostics"`
	Plan        plan.ExecutionPlan  `json:"plan"`
}

type ProjectFacts struct {
	CanonicalRoot string         `json:"canonical_root"`
	Branch        string         `json:"branch,omitempty"`
	Revision      string         `json:"revision,omitempty"`
	Clean         bool           `json:"clean"`
	ConfigPath    string         `json:"config_path,omitempty"`
	ConfigExists  bool           `json:"config_exists"`
	GitRoots      []DetectedPath `json:"git_roots"`
	Lockfiles     []DetectedPath `json:"lockfiles"`
	Dockerfiles   []DetectedPath `json:"dockerfiles"`
	DevenvFiles   []DetectedPath `json:"devenv_files"`
}

type DetectedPath struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type DoctorRequest struct {
	RequireBuilder bool
}

type DoctorResult struct {
	Capabilities runtime.Capabilities `json:"capabilities"`
	Diagnostics  []config.Diagnostic  `json:"diagnostics"`
}

type BareScreen string

const (
	BareSetup     BareScreen = "setup"
	BareDashboard BareScreen = "dashboard"
)

type BareStateRequest struct {
	Root string
}

type BareState struct {
	Screen          BareScreen           `json:"screen"`
	ConfigExists    bool                 `json:"config_exists"`
	OwnedResources  int                  `json:"owned_resources"`
	Facts           ProjectFacts         `json:"facts"`
	ContainerSystem runtime.SystemStatus `json:"container_system"`
	ConfiguredPorts []config.PortConfig  `json:"configured_ports,omitempty"`
}

type SetupPreviewRequest struct {
	Root           string
	Config         config.ConfigDocument
	RenderedConfig []byte
}

type SetupImageOption struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Available   bool               `json:"available"`
	Image       config.ImageConfig `json:"image"`
}

type SetupPreview struct {
	Facts                  ProjectFacts          `json:"facts"`
	SelectedCapabilities   []string              `json:"selected_capabilities"`
	Diagnostics            []config.Diagnostic   `json:"diagnostics"`
	Config                 config.ConfigDocument `json:"config"`
	RenderedConfig         []byte                `json:"rendered_config"`
	ConfigContentDigest    string                `json:"config_content_digest"`
	ImportedContentDigests []state.ContentDigest `json:"imported_content_digests"`
	Plan                   plan.ExecutionPlan    `json:"plan"`
	Hash                   string                `json:"hash"`
	ProjectState           string                `json:"project_state"`
	ImageOptions           []SetupImageOption    `json:"image_options"`
	SelectedImageOption    string                `json:"selected_image_option"`
}

type InitializeRequest struct {
	Root                           string
	ExpectedHash                   string
	ExpectedConfigDigest           string
	ExpectedProjectState           string
	ExpectedImportedContentDigests []state.ContentDigest
	Confirmed                      bool
	ReplacesConfigDigest           string
	Config                         config.ConfigDocument
	RenderedConfig                 []byte
}

type InitializeResult struct {
	ConfigPath string `json:"config_path"`
	Hash       string `json:"hash"`
	Created    bool   `json:"created"`
}

type InteractiveChild struct {
	Argv   []string
	Env    []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}
