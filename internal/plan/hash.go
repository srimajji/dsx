package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

const executableHashDomain = ContractVersion + "\n"

// ExecutablePlanV2 is the canonical, secret-free projection authorized by an
// executable configuration approval. Workspace identity and source revisions
// are supplied separately at creation time and therefore never affect this hash.
type ExecutablePlanV2 struct {
	Agents       AgentPlan              `json:"agents"`
	Image        executableImage        `json:"image"`
	Repositories []executableRepository `json:"repositories"`
	Setup        []executableCommand    `json:"setup"`
	Processes    []executableProcess    `json:"processes"`
	Mounts       []ResolvedMount        `json:"mounts"`
	Volumes      []ResolvedVolume       `json:"volumes"`
	Auth         AuthPlan               `json:"auth"`
	Ports        []executablePort       `json:"ports"`
	Browser      *BrowserPlan           `json:"browser"`
	Bridges      []BridgeGrant          `json:"bridges"`
	Limits       ResourceLimits         `json:"limits"`
}

type executableImage struct {
	Reference   string     `json:"reference"`
	Context     string     `json:"context"`
	File        string     `json:"file"`
	Target      string     `json:"target"`
	BuildArgs   []KeyValue `json:"build_args"`
	InputDigest string     `json:"input_digest"`
}

type executableRepository struct {
	Name          string `json:"name"`
	HostPath      string `json:"host_path"`
	GuestPath     string `json:"guest_path"`
	TrackedDigest string `json:"tracked_digest"`
}

type executableCommand struct {
	Argv      []string        `json:"argv"`
	Shell     string          `json:"shell"`
	ShellPath string          `json:"shell_path"`
	Cwd       string          `json:"cwd"`
	Env       []executableEnv `json:"env"`
}

type executableEnv struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Reference string `json:"reference"`
	Secret    bool   `json:"secret"`
}

type executableProcess struct {
	Name      string            `json:"name"`
	Command   executableCommand `json:"command"`
	DependsOn []string          `json:"depends_on"`
	Required  bool              `json:"required"`
	Terminal  bool              `json:"terminal"`
	Health    *executableHealth `json:"health"`
}

type executableHealth struct {
	Kind       string             `json:"kind"`
	Target     string             `json:"target"`
	Command    *executableCommand `json:"command"`
	IntervalMS int64              `json:"interval_ms"`
	TimeoutMS  int64              `json:"timeout_ms"`
	Retries    int                `json:"retries"`
}

type executablePort struct {
	Name                     string  `json:"name"`
	GuestPort                uint16  `json:"guest_port"`
	Protocol                 string  `json:"protocol"`
	HostIP                   string  `json:"host_ip"`
	HostPort                 *uint16 `json:"host_port"`
	ExplicitNonLoopbackGrant bool    `json:"explicit_non_loopback_grant"`
}

// ExecutableProjection returns a normalized copy of the authority-bearing
// fields in plan. It deliberately omits identities, ownership-derived names,
// provenance, display-only data, run IDs, and secret values.
func ExecutableProjection(plan ExecutionPlan) ExecutablePlanV2 {
	projection := ExecutablePlanV2{
		Agents:    AgentPlan{Allowed: append([]string(nil), plan.Agents.Allowed...), Default: plan.Agents.Default},
		Image:     projectImage(plan.Image),
		Setup:     projectCommands(plan.Setup),
		Processes: projectProcesses(plan.Processes),
		Mounts:    append([]ResolvedMount(nil), plan.Mounts...),
		Volumes:   append([]ResolvedVolume(nil), plan.Volumes...),
		Auth:      AuthPlan{Imports: append([]string(nil), plan.Auth.Imports...)},
		Ports:     projectPorts(plan.Ports),
		Bridges:   append([]BridgeGrant(nil), plan.Bridges...),
		Limits:    plan.Limits,
		Browser:   cloneBrowser(plan.Browser),
	}
	projection.Repositories = make([]executableRepository, len(plan.Repositories))
	for index, repository := range plan.Repositories {
		projection.Repositories[index] = executableRepository(repository)
	}

	sort.Slice(projection.Repositories, func(i, j int) bool {
		left, right := projection.Repositories[i], projection.Repositories[j]
		return lessStrings(
			[]string{left.GuestPath, left.HostPath, left.Name, left.TrackedDigest},
			[]string{right.GuestPath, right.HostPath, right.Name, right.TrackedDigest},
		)
	})
	sort.Strings(projection.Agents.Allowed)
	sort.Slice(projection.Mounts, func(i, j int) bool {
		left, right := projection.Mounts[i], projection.Mounts[j]
		return lessStrings(
			[]string{left.Target, left.SourceType, left.Source, boolString(left.ReadOnly)},
			[]string{right.Target, right.SourceType, right.Source, boolString(right.ReadOnly)},
		)
	})
	sort.Slice(projection.Volumes, func(i, j int) bool {
		left, right := projection.Volumes[i], projection.Volumes[j]
		return lessStrings(
			[]string{left.Target, left.Name, left.Scope, boolString(left.Persistent)},
			[]string{right.Target, right.Name, right.Scope, boolString(right.Persistent)},
		)
	})
	sort.Strings(projection.Auth.Imports)
	sort.Slice(projection.Ports, func(i, j int) bool {
		left, right := projection.Ports[i], projection.Ports[j]
		return lessStrings(portSortKey(left), portSortKey(right))
	})
	sort.Slice(projection.Bridges, func(i, j int) bool {
		left, right := projection.Bridges[i], projection.Bridges[j]
		return lessStrings(
			[]string{left.Kind, left.Name, left.Destination, left.SourceIdentity, uintString(left.Port), boolString(left.ReadOnly)},
			[]string{right.Kind, right.Name, right.Destination, right.SourceIdentity, uintString(right.Port), boolString(right.ReadOnly)},
		)
	})
	return projection
}

// HashExecutionPlan returns the lowercase SHA-256 authorization hash for plan.
func HashExecutionPlan(plan ExecutionPlan) (string, error) {
	canonical, err := json.Marshal(ExecutableProjection(plan))
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(executableHashDomain))
	_, _ = digest.Write(canonical)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// SetExecutableHash computes and stores plan's authorization hash.
func SetExecutableHash(plan *ExecutionPlan) error {
	hash, err := HashExecutionPlan(*plan)
	if err != nil {
		return err
	}
	plan.ExecutableHash = hash
	return nil
}

func projectImage(image ResolvedImage) executableImage {
	result := executableImage{
		Reference: image.Reference, Context: image.Context, File: image.File,
		Target: image.Target, InputDigest: image.InputDigest,
		BuildArgs: append([]KeyValue(nil), image.BuildArgs...),
	}
	sort.Slice(result.BuildArgs, func(i, j int) bool {
		return lessStrings(
			[]string{result.BuildArgs[i].Key, result.BuildArgs[i].Value},
			[]string{result.BuildArgs[j].Key, result.BuildArgs[j].Value},
		)
	})
	return result
}

func projectCommands(commands []ResolvedCommand) []executableCommand {
	result := make([]executableCommand, len(commands))
	for index := range commands {
		result[index] = projectCommand(commands[index])
	}
	return result
}

func projectCommand(command ResolvedCommand) executableCommand {
	result := executableCommand{
		Argv: append([]string(nil), command.Argv...), Shell: command.Shell,
		ShellPath: command.ShellPath, Cwd: command.Cwd,
		Env: make([]executableEnv, len(command.Env)),
	}
	for index, grant := range command.Env {
		value := grant.Value
		if grant.Secret {
			value = ""
		}
		result.Env[index] = executableEnv{
			Name: grant.Name, Value: value, Reference: grant.Reference, Secret: grant.Secret,
		}
	}
	sort.Slice(result.Env, func(i, j int) bool {
		left, right := result.Env[i], result.Env[j]
		return lessStrings(
			[]string{left.Name, left.Reference, left.Value, boolString(left.Secret)},
			[]string{right.Name, right.Reference, right.Value, boolString(right.Secret)},
		)
	})
	return result
}

func projectProcesses(processes []ResolvedProcess) []executableProcess {
	result := make([]executableProcess, len(processes))
	for index, process := range processes {
		projected := executableProcess{
			Name: process.Name, Command: projectCommand(process.Command),
			DependsOn: append([]string(nil), process.DependsOn...), Required: process.Required, Terminal: process.Terminal,
		}
		sort.Strings(projected.DependsOn)
		if process.Health != nil {
			projected.Health = &executableHealth{
				Kind: process.Health.Kind, Target: process.Health.Target,
				IntervalMS: process.Health.IntervalMS, TimeoutMS: process.Health.TimeoutMS,
				Retries: process.Health.Retries,
			}
			if process.Health.Command != nil {
				command := projectCommand(*process.Health.Command)
				projected.Health.Command = &command
			}
		}
		result[index] = projected
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func projectPorts(ports []PortRequest) []executablePort {
	result := make([]executablePort, len(ports))
	for index, port := range ports {
		result[index] = executablePort{
			Name: port.Name, GuestPort: port.GuestPort, Protocol: port.Protocol,
			HostIP: port.HostIP.String(), HostPort: cloneUint16(port.HostPort),
			ExplicitNonLoopbackGrant: port.ExplicitNonLoopbackGrant,
		}
	}
	return result
}

func cloneBrowser(browser *BrowserPlan) *BrowserPlan {
	if browser == nil {
		return nil
	}
	copy := *browser
	return &copy
}

func cloneUint16(value *uint16) *uint16 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func portSortKey(port executablePort) []string {
	hostPort := ""
	if port.HostPort != nil {
		hostPort = uintString(*port.HostPort)
	}
	return []string{port.Name, uintString(port.GuestPort), port.Protocol, port.HostIP, hostPort, boolString(port.ExplicitNonLoopbackGrant)}
}

func lessStrings(left, right []string) bool {
	for index := range left {
		if left[index] != right[index] {
			return left[index] < right[index]
		}
	}
	return false
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func uintString(value uint16) string {
	const digits = "0123456789abcdef"
	buffer := [4]byte{}
	for index := len(buffer) - 1; index >= 0; index-- {
		buffer[index] = digits[value&0xf]
		value >>= 4
	}
	return string(buffer[:])
}
