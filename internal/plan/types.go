package plan

import (
	"net/netip"

	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/model"
)

const ContractVersion = "dsx.execution-plan/v1"

type ExecutionPlan struct {
	ContractVersion string              `json:"contract_version"`
	Project         ProjectIdentity     `json:"project"`
	Sandbox         SandboxIdentity     `json:"sandbox"`
	Mode            model.WorkspaceMode `json:"mode"`
	Agent           string              `json:"agent"`
	Image           ResolvedImage       `json:"image"`
	Repositories    []RepositoryPlan    `json:"repositories"`
	Setup           []ResolvedCommand   `json:"setup"`
	Processes       []ResolvedProcess   `json:"processes"`
	Mounts          []ResolvedMount     `json:"mounts"`
	Volumes         []ResolvedVolume    `json:"volumes"`
	Auth            []ResolvedAuthGrant `json:"auth"`
	Ports           []PortRequest       `json:"ports"`
	Browser         *BrowserPlan        `json:"browser,omitempty"`
	Bridges         []BridgeGrant       `json:"bridges"`
	Limits          ResourceLimits      `json:"limits"`
	Ownership       OwnershipPlan       `json:"ownership"`
	Provenance      config.Provenance   `json:"provenance"`
	ExecutableHash  string              `json:"executable_hash"`
}

type ProjectIdentity struct {
	ID            model.ProjectID `json:"id"`
	CanonicalRoot string          `json:"canonical_root"`
}

type SandboxIdentity struct {
	Name  model.SandboxName `json:"name"`
	RunID model.RunID       `json:"run_id"`
}

type ResolvedImage struct {
	Reference   string     `json:"reference,omitempty"`
	Context     string     `json:"context,omitempty"`
	File        string     `json:"file,omitempty"`
	Target      string     `json:"target,omitempty"`
	BuildArgs   []KeyValue `json:"build_args,omitempty"`
	InputDigest string     `json:"input_digest"`
	Standard    bool       `json:"standard,omitempty"`
}

type RepositoryPlan struct {
	Name          string `json:"name"`
	HostPath      string `json:"host_path"`
	GuestPath     string `json:"guest_path"`
	SourceRef     string `json:"source_ref,omitempty"`
	SourceCommit  string `json:"source_commit,omitempty"`
	TrackedDigest string `json:"tracked_digest,omitempty"`
}

type ResolvedCommand struct {
	Argv      []string   `json:"argv,omitempty"`
	Shell     string     `json:"shell,omitempty"`
	ShellPath string     `json:"shell_path,omitempty"`
	Cwd       string     `json:"cwd"`
	Env       []EnvGrant `json:"env,omitempty"`
}

type ResolvedProcess struct {
	Name      string          `json:"name"`
	Command   ResolvedCommand `json:"command"`
	DependsOn []string        `json:"depends_on,omitempty"`
	Required  bool            `json:"required"`
	Terminal  bool            `json:"terminal"`
	Health    *ResolvedHealth `json:"health,omitempty"`
}

type ResolvedHealth struct {
	Kind       string           `json:"kind"`
	Target     string           `json:"target,omitempty"`
	Command    *ResolvedCommand `json:"command,omitempty"`
	IntervalMS int64            `json:"interval_ms"`
	TimeoutMS  int64            `json:"timeout_ms"`
	Retries    int              `json:"retries"`
}

type EnvGrant struct {
	Name      string `json:"name"`
	Value     string `json:"value,omitempty"`
	Reference string `json:"reference,omitempty"`
	Secret    bool   `json:"secret"`
}

type ResolvedMount struct {
	SourceType     string `json:"source_type"`
	Source         string `json:"source"`
	SourceIdentity string `json:"source_identity,omitempty"`
	Target         string `json:"target"`
	ReadOnly       bool   `json:"read_only"`
}

type ResolvedVolume struct {
	Name       string `json:"name"`
	Target     string `json:"target"`
	Scope      string `json:"scope"`
	Persistent bool   `json:"persistent"`
}

type ResolvedAuthGrant struct {
	Harness     string `json:"harness"`
	Profile     string `json:"profile"`
	Persistence string `json:"persistence"`
}

type PortRequest struct {
	Name                     string     `json:"name"`
	GuestPort                uint16     `json:"guest_port"`
	Protocol                 string     `json:"protocol"`
	HostIP                   netip.Addr `json:"host_ip"`
	HostPort                 *uint16    `json:"host_port,omitempty"`
	ExplicitNonLoopbackGrant bool       `json:"explicit_non_loopback_grant"`
}

type BrowserPlan struct {
	Enabled        bool   `json:"enabled"`
	ImageReference string `json:"image_reference,omitempty"`
	ImageDigest    string `json:"image_digest,omitempty"`
}

type BridgeGrant struct {
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	Destination    string `json:"destination,omitempty"`
	SourceIdentity string `json:"source_identity,omitempty"`
	Port           uint16 `json:"port,omitempty"`
	ReadOnly       bool   `json:"read_only"`
}

type ResourceLimits struct {
	CPUs                int   `json:"cpus"`
	MemoryBytes         int64 `json:"memory_bytes"`
	MaxConcurrentClones int   `json:"max_concurrent_clones"`
}

type OwnershipPlan struct {
	Labels       []KeyValue `json:"labels"`
	ResourceName string     `json:"resource_name"`
}

type KeyValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}
