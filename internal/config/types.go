package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

type ConfigDocument struct {
	Schema        string                       `json:"$schema,omitempty"`
	SchemaVersion int                          `json:"schemaVersion"`
	Workspace     WorkspaceConfig              `json:"workspace"`
	Image         ImageConfig                  `json:"image"`
	Setup         []CommandSpec                `json:"setup,omitempty"`
	Processes     map[string]ProcessSpec       `json:"processes,omitempty"`
	Volumes       map[string]VolumeSpec        `json:"volumes,omitempty"`
	Mounts        []MountSpec                  `json:"mounts,omitempty"`
	Agents        AgentConfig                  `json:"agents,omitempty"`
	AuthProfiles  map[string]AuthProfileConfig `json:"authProfiles,omitempty"`
	Browser       BrowserConfig                `json:"browser,omitempty"`
	AWS           AWSConfig                    `json:"aws,omitempty"`
	Network       NetworkConfig                `json:"network,omitempty"`
	Ports         []PortConfig                 `json:"ports,omitempty"`
	Resources     ResourceLimits               `json:"resources,omitempty"`
}

type WorkspaceConfig struct {
	Root    string             `json:"root"`
	Members []RepositoryMember `json:"members,omitempty"`
}

type RepositoryMember struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type ImageConfig struct {
	Ref      string      `json:"ref,omitempty"`
	Build    *ImageBuild `json:"build,omitempty"`
	Standard bool        `json:"standard,omitempty"`
}

type ImageBuild struct {
	Context string            `json:"context"`
	File    string            `json:"file"`
	Target  string            `json:"target,omitempty"`
	Args    map[string]string `json:"args,omitempty"`
}

type CommandSpec struct {
	Argv      []string            `json:"argv,omitempty"`
	Shell     string              `json:"shell,omitempty"`
	ShellPath string              `json:"shellPath,omitempty"`
	Cwd       string              `json:"cwd,omitempty"`
	Env       map[string]EnvValue `json:"env,omitempty"`
}

type EnvValue struct {
	Value     *string `json:"value,omitempty"`
	HostEnv   string  `json:"hostEnv,omitempty"`
	SecretRef string  `json:"secretRef,omitempty"`
}

type ProcessSpec struct {
	CommandSpec
	DependsOn []string     `json:"dependsOn,omitempty"`
	Required  *bool        `json:"required,omitempty"`
	Terminal  bool         `json:"terminal,omitempty"`
	Health    *HealthCheck `json:"health,omitempty"`
}

type HealthCheck struct {
	HTTP     *HTTPHealth  `json:"http,omitempty"`
	TCP      *TCPHealth   `json:"tcp,omitempty"`
	Command  *CommandSpec `json:"command,omitempty"`
	Interval string       `json:"interval,omitempty"`
	Timeout  string       `json:"timeout,omitempty"`
	Retries  int          `json:"retries,omitempty"`
}

type HTTPHealth struct {
	URL string `json:"url"`
}

type TCPHealth struct {
	Address string `json:"address"`
}

type VolumeSpec struct {
	Target     string `json:"target"`
	Scope      string `json:"scope"`
	Persistent bool   `json:"persistent,omitempty"`
}

type MountSpec struct {
	Source   MountSource `json:"source"`
	Target   string      `json:"target"`
	ReadOnly bool        `json:"readOnly,omitempty"`
}

type MountSource struct {
	Type   string `json:"type"`
	Path   string `json:"path,omitempty"`
	Volume string `json:"volume,omitempty"`
}

type AgentConfig struct {
	Default string   `json:"default,omitempty"`
	Allowed []string `json:"allowed,omitempty"`
}

type AuthProfileConfig struct {
	Harness     string `json:"harness"`
	Persistence string `json:"persistence"`
}

type BrowserConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

type AWSConfig struct {
	Mode      string `json:"mode,omitempty"`
	Directory string `json:"directory,omitempty"`
	Profile   string `json:"profile,omitempty"`
}

type NetworkConfig struct {
	Internet   *bool       `json:"internet,omitempty"`
	HostGrants []HostGrant `json:"hostGrants,omitempty"`
}

type HostGrant struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Port        uint16 `json:"port"`
}

type PortConfig struct {
	Name     string   `json:"name"`
	Guest    uint16   `json:"guest"`
	Host     HostPort `json:"host"`
	Bind     string   `json:"bind,omitempty"`
	Protocol string   `json:"protocol,omitempty"`
}

type HostPort struct {
	Dynamic bool
	Port    uint16
}

func (port *HostPort) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte(`"dynamic"`)) {
		port.Dynamic = true
		port.Port = 0
		return nil
	}
	var value uint16
	if err := json.Unmarshal(data, &value); err != nil || value == 0 {
		return fmt.Errorf("host port must be \"dynamic\" or an integer from 1 to 65535")
	}
	port.Dynamic = false
	port.Port = value
	return nil
}

func (port HostPort) MarshalJSON() ([]byte, error) {
	if port.Dynamic {
		return []byte(`"dynamic"`), nil
	}
	if port.Port == 0 {
		return nil, fmt.Errorf("host port is unset")
	}
	return []byte(strconv.FormatUint(uint64(port.Port), 10)), nil
}

type ResourceLimits struct {
	CPUs                int    `json:"cpus,omitempty"`
	Memory              string `json:"memory,omitempty"`
	MaxConcurrentClones int    `json:"maxConcurrentClones,omitempty"`
}

type SourceRef struct {
	Kind     string `json:"kind"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Priority int    `json:"priority"`
}

type Provenance map[string]SourceRef

type Diagnostic struct {
	Severity string           `json:"severity"`
	Code     string           `json:"code"`
	Message  string           `json:"message"`
	Path     string           `json:"path,omitempty"`
	Line     int              `json:"line,omitempty"`
	Column   int              `json:"column,omitempty"`
	Related  []SourceLocation `json:"related,omitempty"`
}

type SourceLocation struct {
	Path   string `json:"path,omitempty"`
	Offset int    `json:"offset,omitempty"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
}

type ValidatedConfig struct {
	Document        ConfigDocument
	ContentDigest   string
	SourcePath      string
	Provenance      Provenance
	SourceLocations map[string]SourceLocation
}
