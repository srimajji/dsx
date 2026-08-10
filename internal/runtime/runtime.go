package runtime

import (
	"context"
	"errors"
	"io"
	"net/netip"
)

type Adapter interface {
	Probe(context.Context) (Capabilities, error)
	EnsureImage(context.Context, ImageSpec) (Image, error)
	CreateVolume(context.Context, VolumeSpec) (Resource, error)
	CreateNetwork(context.Context, NetworkSpec) (Resource, error)
	CreateWorkspace(context.Context, WorkspaceSpec) (Resource, error)
	CreateBrowser(context.Context, BrowserSpec) (Resource, error)
	StartWorkspace(context.Context, ResourceSnapshot) error
	PrepareExec(context.Context, ResourceSnapshot, ExecSpec) (ProcessSpec, error)
	Exec(context.Context, ResourceSnapshot, ExecSpec, ExecIO) (Exit, error)
	CopyTo(context.Context, ResourceSnapshot, HostPath, GuestPath) error
	CopyFrom(context.Context, ResourceSnapshot, GuestPath, HostPath) error
	Inspect(context.Context, ResourceID) (ResourceSnapshot, error)
	List(context.Context, ResourceKind) ([]ResourceSnapshot, error)
	Signal(context.Context, ResourceSnapshot, Signal) error
	Stop(context.Context, ResourceSnapshot, StopPolicy) error
	Delete(context.Context, ResourceSnapshot) error
}

type Capabilities struct {
	HostOS                    string `json:"host_os"`
	HostVersion               string `json:"host_version"`
	HostArch                  string `json:"host_arch"`
	CLIVersion                string `json:"cli_version"`
	ServerVersion             string `json:"server_version"`
	CompatibilityID           string `json:"compatibility_id"`
	ServiceHealthy            bool   `json:"service_healthy"`
	BuilderHealthy            bool   `json:"builder_healthy"`
	MachineReadableInspection bool   `json:"machine_readable_inspection"`
	Labels                    bool   `json:"labels"`
	Networks                  bool   `json:"networks"`
	Volumes                   bool   `json:"volumes"`
	Copy                      bool   `json:"copy"`
	FixedPublication          bool   `json:"fixed_publication"`
	DynamicPublication        bool   `json:"dynamic_publication"`
	PTY                       bool   `json:"pty"`
	Resize                    bool   `json:"resize"`
}

var ErrResourceNotFound = errors.New("runtime resource not found")

type ResourceID string

type ResourceKind string

const (
	ResourceWorkspace ResourceKind = "workspace"
	ResourceBrowser   ResourceKind = "browser"
	ResourceNetwork   ResourceKind = "network"
	ResourceVolume    ResourceKind = "volume"
)

type Label struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Resource struct {
	ID   ResourceID   `json:"id"`
	Name string       `json:"name"`
	Kind ResourceKind `json:"kind"`
}

type ResourceSnapshot struct {
	Resource
	State            string                  `json:"state"`
	ImageDigest      string                  `json:"image_digest,omitempty"`
	Labels           []Label                 `json:"labels"`
	Mounts           []Mount                 `json:"mounts"`
	Networks         []string                `json:"networks"`
	Ports            []PortBinding           `json:"ports"`
	NetworkAddresses map[string][]netip.Addr `json:"network_addresses,omitempty"`
}

type ImageSpec struct {
	Reference string
	Context   HostPath
	File      HostPath
	Target    string
	BuildArgs []Label
	Labels    []Label
}

type Image struct {
	Reference string
	Digest    string
	Local     bool
}

type VolumeSpec struct {
	Name   string
	Labels []Label
}

type NetworkSpec struct {
	Name   string
	Labels []Label
}

type WorkspaceSpec struct {
	Name        string
	Image       Image
	Entrypoint  []string
	Env         []string
	WorkingDir  GuestPath
	User        string
	Mounts      []Mount
	Networks    []string
	Ports       []PortRequest
	Labels      []Label
	CPUs        int
	MemoryBytes int64
}

// BrowserSpec deliberately excludes mounts, volumes, host paths, users, and
// port publications. Browser sandboxes are an untrusted, network-only boundary.
type BrowserSpec struct {
	Name        string
	Image       Image
	Entrypoint  []string
	Env         []string
	Networks    []string
	Labels      []Label
	CPUs        int
	MemoryBytes int64
}

// MountAuthority identifies the app-validated capability that permits a mount
// source to cross the runtime boundary. Apple adapters must validate the
// authority/type/target pairing before serializing it.
type MountAuthority string

const (
	MountAuthorityRepository     MountAuthority = "repository"
	MountAuthorityConfiguredHost MountAuthority = "configured-host"
	MountAuthorityLeappMirror    MountAuthority = "leapp-mirror"
	MountAuthorityGuestHelper    MountAuthority = "guest-helper"
	MountAuthorityInternal       MountAuthority = "internal-artifact"
	MountAuthorityVolume         MountAuthority = "volume"
)

type Mount struct {
	Source    string         `json:"source"`
	Target    string         `json:"target"`
	ReadOnly  bool           `json:"read_only"`
	Type      string         `json:"type"`
	Authority MountAuthority `json:"-"`
}

type PortRequest struct {
	HostIP    netip.Addr
	HostPort  *uint16
	GuestPort uint16
	Protocol  string
}

type PortBinding struct {
	HostIP    netip.Addr `json:"host_ip"`
	HostPort  uint16     `json:"host_port"`
	GuestPort uint16     `json:"guest_port"`
	Protocol  string     `json:"protocol"`
}

type ExecSpec struct {
	Argv       []string
	Env        []string
	WorkingDir GuestPath
	User       string
	Terminal   bool
}

type ExecIO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// ProcessSpec is a validated, shell-free host process invocation. Presentation
// adapters use it only when they must own the PTY lifecycle for an interactive
// runtime exec.
type ProcessSpec struct {
	Executable string
	Args       []string
	Env        []string
	Dir        string
}

type Exit struct {
	Code   *int
	Signal string
}

type Signal string

type StopPolicy struct {
	TimeoutSeconds int
	Signal         Signal
}

type HostPath string

type GuestPath string
