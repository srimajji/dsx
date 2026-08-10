package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var configNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*$`)
var memoryPattern = regexp.MustCompile(`^([1-9][0-9]*)(MiB|GiB)$`)

const reservedGuestHelperDirectory = "/usr/local/libexec/dsx"

var supportedHarnesses = map[string]struct{}{
	"omp": {}, "codex": {}, "claude": {}, "opencode": {},
}

func validateSemantics(document ConfigDocument, sourcePath string, locations map[string]SourceLocation) []Diagnostic {
	validator := semanticValidator{sourcePath: sourcePath, locations: locations}
	validator.projectPath("/workspace/root", document.Workspace.Root)
	if document.Imports.Devcontainer != nil {
		validator.projectPath("/imports/devcontainer/path", document.Imports.Devcontainer.Path)
	}
	validator.validateMembers(document.Workspace)
	validator.validateImage(document.Image)
	for index, command := range document.Setup {
		validator.validateCommand(fmt.Sprintf("/setup/%d", index), command)
	}
	validator.validateProcesses(document.Processes)
	validator.validateVolumes(document.Volumes)
	validator.validateMounts(document.Mounts, document.Volumes)
	validator.validateAgents(document.Agents, document.AuthProfiles)
	validator.validateAWS(document.AWS)
	validator.validateNetwork(document.Network, document.Browser)
	validator.validatePorts(document.Ports)
	validator.validateResources(document.Resources)
	sort.SliceStable(validator.diagnostics, func(i, j int) bool {
		if validator.diagnostics[i].Path != validator.diagnostics[j].Path {
			return validator.diagnostics[i].Path < validator.diagnostics[j].Path
		}
		return validator.diagnostics[i].Message < validator.diagnostics[j].Message
	})
	return validator.diagnostics
}

type semanticValidator struct {
	sourcePath  string
	locations   map[string]SourceLocation
	diagnostics []Diagnostic
}

func (v *semanticValidator) add(pointer, message string) {
	loc := nearestLocation(pointer, v.locations)
	d := diagnostic("semantic", message, v.sourcePath, loc)
	d.Path = pointer
	v.diagnostics = append(v.diagnostics, d)
}

func (v *semanticValidator) projectPath(pointer, value string) bool {
	if value == "" {
		return false
	}
	if strings.Contains(value, `\`) || path.IsAbs(value) || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") {
		v.add(pointer, "path must be a canonical project-relative path contained by the project")
		return false
	}
	return true
}

func (v *semanticValidator) guestPath(pointer, value string) bool {
	if !path.IsAbs(value) || path.Clean(value) != value || value == "/" {
		v.add(pointer, "path must be a canonical absolute guest path below /")
		return false
	}
	return true
}

func (v *semanticValidator) validateMembers(workspace WorkspaceConfig) {
	names := make(map[string]int)
	paths := make(map[string]int)
	for index, member := range workspace.Members {
		pointer := fmt.Sprintf("/workspace/members/%d", index)
		v.validName(pointer+"/name", member.Name)
		if first, exists := names[member.Name]; exists {
			v.add(pointer+"/name", fmt.Sprintf("member name duplicates /workspace/members/%d/name", first))
		} else {
			names[member.Name] = index
		}
		if !v.projectPath(pointer+"/path", member.Path) {
			continue
		}
		if workspace.Root != "." && member.Path != workspace.Root && !strings.HasPrefix(member.Path, workspace.Root+"/") {
			v.add(pointer+"/path", "member path must be contained by workspace.root")
		}
		if member.Path == "." {
			v.add(pointer+"/path", "member path must be below the workspace root")
		}
		if first, exists := paths[member.Path]; exists {
			v.add(pointer+"/path", fmt.Sprintf("member path duplicates /workspace/members/%d/path", first))
		} else {
			paths[member.Path] = index
		}
	}
	for i := 0; i < len(workspace.Members); i++ {
		for j := i + 1; j < len(workspace.Members); j++ {
			a, b := workspace.Members[i].Path, workspace.Members[j].Path
			if a != "" && b != "" && a != b && pathsOverlap(a, b) {
				v.add(fmt.Sprintf("/workspace/members/%d/path", j), fmt.Sprintf("member path overlaps /workspace/members/%d/path", i))
			}
		}
	}
}

func pathsOverlap(a, b string) bool {
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func (v *semanticValidator) validateImage(image ImageConfig) {
	if (image.Ref == "") == (image.Build == nil) {
		v.add("/image", "image must declare exactly one of ref or build")
	}
	if image.Ref != "" {
		at := strings.LastIndex(image.Ref, "@sha256:")
		if at < 1 || len(image.Ref[at+8:]) != 64 || !isHex(image.Ref[at+8:]) {
			v.add("/image/ref", "image ref must be pinned by a full sha256 digest")
		}
	}
	if image.Build != nil {
		v.projectPath("/image/build/context", image.Build.Context)
		v.projectPath("/image/build/file", image.Build.File)
	}
}

func isHex(value string) bool {
	for _, c := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func (v *semanticValidator) validateCommand(pointer string, command CommandSpec) {
	argv := len(command.Argv) != 0
	shell := command.Shell != "" || command.ShellPath != ""
	if argv == shell {
		v.add(pointer, "command must declare exactly one of argv or shell with shellPath")
	}
	if shell && (command.Shell == "" || command.ShellPath == "") {
		v.add(pointer, "shell commands require both shell and shellPath")
	}
	if command.Cwd != "" {
		v.guestPath(pointer+"/cwd", command.Cwd)
	}
	for _, name := range sortedKeys(command.Env) {
		env := command.Env[name]
		count := 0
		if env.Value != nil {
			count++
		}
		if env.HostEnv != "" {
			count++
		}
		if env.SecretRef != "" {
			count++
		}
		if count != 1 {
			v.add(pointer+"/env/"+escapePointer(name), "environment value must declare exactly one of value, hostEnv, or secretRef")
		}
	}
}

func (v *semanticValidator) validateProcesses(processes map[string]ProcessSpec) {
	names := sortedKeys(processes)
	for _, name := range names {
		process := processes[name]
		pointer := "/processes/" + escapePointer(name)
		v.validName(pointer, name)
		v.validateCommand(pointer, process.CommandSpec)
		for index, dependency := range process.DependsOn {
			if _, exists := processes[dependency]; !exists {
				v.add(fmt.Sprintf("%s/dependsOn/%d", pointer, index), fmt.Sprintf("unknown process dependency %q", dependency))
			}
			if dependency == name {
				v.add(fmt.Sprintf("%s/dependsOn/%d", pointer, index), "process cannot depend on itself")
			}
		}
		if process.Health != nil {
			v.validateHealth(pointer+"/health", *process.Health)
		}
	}
	state := make(map[string]uint8, len(processes))
	var visit func(string, []string)
	visit = func(name string, stack []string) {
		if state[name] == 2 {
			return
		}
		if state[name] == 1 {
			start := 0
			for start < len(stack) && stack[start] != name {
				start++
			}
			cycle := append(append([]string(nil), stack[start:]...), name)
			v.add("/processes/"+escapePointer(name)+"/dependsOn", "process dependency cycle: "+strings.Join(cycle, " -> "))
			return
		}
		state[name] = 1
		for _, dependency := range processes[name].DependsOn {
			if _, exists := processes[dependency]; exists {
				visit(dependency, append(stack, name))
			}
		}
		state[name] = 2
	}
	for _, name := range names {
		visit(name, nil)
	}
}

func (v *semanticValidator) validateHealth(pointer string, health HealthCheck) {
	kinds := 0
	if health.HTTP != nil {
		kinds++
		parsed, err := url.Parse(health.HTTP.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
			v.add(pointer+"/http/url", "health URL must be an absolute http or https URL without user information")
		}
	}
	if health.TCP != nil {
		kinds++
		host, portValue, err := net.SplitHostPort(health.TCP.Address)
		if err != nil || host == "" || !validPortString(portValue) {
			v.add(pointer+"/tcp/address", "TCP health address must be host:port with a port from 1 to 65535")
		}
	}
	if health.Command != nil {
		kinds++
		v.validateCommand(pointer+"/command", *health.Command)
	}
	if kinds != 1 {
		v.add(pointer, "health check must declare exactly one of http, tcp, or command")
	}
	v.duration(pointer+"/interval", health.Interval, 100*time.Millisecond, 5*time.Minute)
	v.duration(pointer+"/timeout", health.Timeout, 100*time.Millisecond, time.Minute)
	if health.Retries < 0 || health.Retries > 100 {
		v.add(pointer+"/retries", "health retries must be from 0 to 100")
	}
}

func validPortString(value string) bool {
	portValue, err := strconv.Atoi(value)
	return err == nil && portValue >= 1 && portValue <= 65535
}

func (v *semanticValidator) duration(pointer, value string, minimum, maximum time.Duration) {
	if value == "" {
		return
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < minimum || duration > maximum {
		v.add(pointer, fmt.Sprintf("duration must be between %s and %s", minimum, maximum))
	}
}

func (v *semanticValidator) validateVolumes(volumes map[string]VolumeSpec) {
	for _, name := range sortedKeys(volumes) {
		volume := volumes[name]
		pointer := "/volumes/" + escapePointer(name)
		v.validName(pointer, name)
		v.guestPath(pointer+"/target", volume.Target)
		if guestPathsOverlap(volume.Target, reservedGuestHelperDirectory) {
			v.add(pointer+"/target", "volume target overlaps the reserved DSX guest helper directory")
		}
		if volume.Scope != "sandbox" && volume.Scope != "project" {
			v.add(pointer+"/scope", "volume scope must be sandbox or project")
		}
	}
}

func (v *semanticValidator) validateMounts(mounts []MountSpec, volumes map[string]VolumeSpec) {
	targets := make(map[string]int)
	for index, mount := range mounts {
		pointer := fmt.Sprintf("/mounts/%d", index)
		v.guestPath(pointer+"/target", mount.Target)
		if guestPathsOverlap(mount.Target, reservedGuestHelperDirectory) {
			v.add(pointer+"/target", "mount target overlaps the reserved DSX guest helper directory")
		}
		if first, exists := targets[mount.Target]; exists {
			v.add(pointer+"/target", fmt.Sprintf("mount target duplicates /mounts/%d/target", first))
		} else {
			targets[mount.Target] = index
		}
		switch mount.Source.Type {
		case "workspace":
			v.projectPath(pointer+"/source/path", mount.Source.Path)
			if mount.Source.Volume != "" {
				v.add(pointer+"/source/volume", "workspace mount cannot name a volume")
			}
		case "volume":
			if _, exists := volumes[mount.Source.Volume]; !exists {
				v.add(pointer+"/source/volume", fmt.Sprintf("unknown volume %q", mount.Source.Volume))
			}
			if mount.Source.Path != "" {
				v.add(pointer+"/source/path", "volume mount cannot declare a path")
			}
		case "host":
			v.validateHostMount(pointer, mount)
		default:
			v.add(pointer+"/source/type", "mount source type must be workspace, volume, or host")
		}
	}
}

func guestPathsOverlap(left, right string) bool {
	left = path.Clean(left)
	right = path.Clean(right)
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func (v *semanticValidator) validateHostMount(pointer string, mount MountSpec) {
	if err := ValidateHostMountPath(mount.Source.Path); err != nil {
		v.add(pointer+"/source/path", err.Error())
		return
	}
	if !mount.ReadOnly {
		v.add(pointer+"/readOnly", "host mounts must be read-only; writable project source uses a workspace mount")
	}
}

// ValidateHostMountPath applies the platform-independent lexical policy used
// both before filesystem access and again after app-layer canonicalization.
func ValidateHostMountPath(source string) error {
	if !path.IsAbs(source) || path.Clean(source) != source {
		return fmt.Errorf("host mount source must be a canonical absolute path")
	}
	lower := strings.ToLower(source)
	denied := false
	for _, forbidden := range []string{
		"/users",
		"/home",
		"/root",
		"/run",
		"/var/run",
		"/private/var/run",
		"/tmp",
		"/private/tmp",
		"/usr/local/libexec/dsx",
	} {
		if hostPathsOverlap(lower, forbidden) {
			denied = true
			break
		}
	}
	components := strings.Split(strings.Trim(lower, "/"), "/")
	for _, component := range components {
		switch component {
		case ".ssh", ".gnupg", "keychains", "chrome", "chromium", "firefox":
			denied = true
		}
		if strings.HasSuffix(component, ".sock") || strings.Contains(component, "ssh_auth_sock") || strings.Contains(component, "ssh-agent") || strings.Contains(component, "gpg-agent") || strings.Contains(component, "keychain") || strings.Contains(component, "tailscale") {
			denied = true
		}
	}
	if strings.Contains(lower, "/library/keychains") || strings.Contains(lower, "/.docker/run/") || strings.Contains(lower, "/private/tmp/com.apple.launchd") {
		denied = true
	}
	if denied {
		return fmt.Errorf("host mount is denied: home, runtime, SSH/GPG, Keychain, Tailscale, and browser-profile paths cannot be mounted")
	}
	return nil
}

func hostPathsOverlap(left, right string) bool {
	left = path.Clean(left)
	right = path.Clean(right)
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func (v *semanticValidator) validateAgents(agents AgentConfig, profiles map[string]AuthProfileConfig) {
	allowed := make(map[string]struct{}, len(agents.Allowed))
	for index, harness := range agents.Allowed {
		if _, ok := supportedHarnesses[harness]; !ok {
			v.add(fmt.Sprintf("/agents/allowed/%d", index), fmt.Sprintf("unsupported harness %q", harness))
		}
		if _, exists := allowed[harness]; exists {
			v.add(fmt.Sprintf("/agents/allowed/%d", index), fmt.Sprintf("duplicate allowed harness %q", harness))
		}
		allowed[harness] = struct{}{}
	}
	if agents.Default != "" {
		if _, ok := supportedHarnesses[agents.Default]; !ok {
			v.add("/agents/default", fmt.Sprintf("unsupported harness %q", agents.Default))
		}
		if len(allowed) != 0 {
			if _, ok := allowed[agents.Default]; !ok {
				v.add("/agents/default", "default harness must be included in agents.allowed")
			}
		}
	}
	for _, name := range sortedKeys(profiles) {
		profile := profiles[name]
		pointer := "/authProfiles/" + escapePointer(name)
		v.validName(pointer, name)
		if _, ok := supportedHarnesses[profile.Harness]; !ok {
			v.add(pointer+"/harness", fmt.Sprintf("unsupported harness %q", profile.Harness))
		}
		if profile.Persistence != "sandbox" && profile.Persistence != "global" {
			v.add(pointer+"/persistence", "auth profile persistence must be sandbox or global")
		}
	}
}

func (v *semanticValidator) validateAWS(aws AWSConfig) {
	switch aws.Mode {
	case "", "none":
		if aws.Directory != "" || aws.Profile != "" {
			v.add("/aws", "AWS directory and profile require mode leapp")
		}
	case "leapp":
		if aws.Directory == "" {
			v.add("/aws", "leapp mode requires directory")
		}
		if aws.Directory != "" && (!path.IsAbs(aws.Directory) || path.Clean(aws.Directory) != aws.Directory) {
			v.add("/aws/directory", "AWS directory must be a canonical absolute path")
		}
	default:
		v.add("/aws/mode", "AWS mode must be none or leapp")
	}
}

func (v *semanticValidator) validateNetwork(network NetworkConfig, browser BrowserConfig) {
	if browser.Enabled && network.Internet != nil && !*network.Internet {
		v.add("/browser/enabled", "browser requires network.internet to be enabled")
	}
	names := make(map[string]int)
	for index, grant := range network.HostGrants {
		pointer := fmt.Sprintf("/network/hostGrants/%d", index)
		v.validName(pointer+"/name", grant.Name)
		if first, exists := names[grant.Name]; exists {
			v.add(pointer+"/name", fmt.Sprintf("host grant name duplicates /network/hostGrants/%d/name", first))
		} else {
			names[grant.Name] = index
		}
		if !validHostDestination(grant.Destination) {
			v.add(pointer+"/destination", "host grant destination must be a hostname or IP address, not a URL or path")
		}
		if grant.Port == 0 {
			v.add(pointer+"/port", "host grant port must be from 1 to 65535")
		}
	}
}

func validHostDestination(value string) bool {
	if value == "" || strings.ContainsAny(value, "/:@ \\\t\r\n") {
		return net.ParseIP(value) != nil
	}
	if len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

func (v *semanticValidator) validatePorts(ports []PortConfig) {
	names := make(map[string]int)
	guest := make(map[string]int)
	host := make(map[string]int)
	for index, portConfig := range ports {
		pointer := fmt.Sprintf("/ports/%d", index)
		v.validName(pointer+"/name", portConfig.Name)
		if first, exists := names[portConfig.Name]; exists {
			v.add(pointer+"/name", fmt.Sprintf("port name duplicates /ports/%d/name", first))
		} else {
			names[portConfig.Name] = index
		}
		protocol := portConfig.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		guestKey := fmt.Sprintf("%s/%d", protocol, portConfig.Guest)
		if first, exists := guest[guestKey]; exists {
			v.add(pointer+"/guest", fmt.Sprintf("guest port duplicates /ports/%d", first))
		} else {
			guest[guestKey] = index
		}
		if !portConfig.Host.Dynamic {
			hostKey := fmt.Sprintf("%s/%s/%d", protocol, portConfig.Bind, portConfig.Host.Port)
			if first, exists := host[hostKey]; exists {
				v.add(pointer+"/host", fmt.Sprintf("host port duplicates /ports/%d", first))
			} else {
				host[hostKey] = index
			}
		}
		if portConfig.Guest == 0 || (!portConfig.Host.Dynamic && portConfig.Host.Port == 0) {
			v.add(pointer, "guest and fixed host ports must be from 1 to 65535")
		}
		if portConfig.Bind != "" {
			address, err := netip.ParseAddr(portConfig.Bind)
			if err != nil || address.Zone() != "" || address.Is4In6() || address.IsMulticast() || address.IsLinkLocalUnicast() {
				v.add(pointer+"/bind", "host port bind must be a supported IP address")
			}
		}
		if protocol != "tcp" {
			v.add(pointer+"/protocol", "config v1 supports only tcp ports")
		}
	}
}

func (v *semanticValidator) validateResources(resources ResourceLimits) {
	if resources.CPUs < 0 || resources.CPUs > 64 {
		v.add("/resources/cpus", "cpus must be from 1 to 64 when specified")
	}
	if resources.Memory != "" {
		matches := memoryPattern.FindStringSubmatch(resources.Memory)
		if matches == nil {
			v.add("/resources/memory", "memory must use a positive MiB or GiB value")
		} else {
			amount, _ := strconv.ParseUint(matches[1], 10, 64)
			mib := amount
			if matches[2] == "GiB" {
				if amount > ^uint64(0)/1024 {
					mib = ^uint64(0)
				} else {
					mib = amount * 1024
				}
			}
			if mib < 128 || mib > 1048576 {
				v.add("/resources/memory", "memory must be between 128MiB and 1024GiB")
			}
		}
	}
	if resources.MaxConcurrentClones < 0 || resources.MaxConcurrentClones > 32 {
		v.add("/resources/maxConcurrentClones", "maxConcurrentClones must be from 1 to 32 when specified")
	}
}

func (v *semanticValidator) validName(pointer, value string) {
	if !configNamePattern.MatchString(value) || len(value) > 63 {
		v.add(pointer, "name must use lowercase letters, digits, hyphens, or underscores and start with a letter")
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
