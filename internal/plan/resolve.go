package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/srimajji/dsx/internal/config"
)

type pureResolver struct{}

func NewResolver() Resolver { return pureResolver{} }

func (pureResolver) Resolve(_ context.Context, input ResolveInput) (ExecutionPlan, []config.Diagnostic, error) {
	resolved, err := ResolvePlan(input)
	if err != nil {
		return resolved, nil, err
	}
	for _, grant := range resolved.Bridges {
		if grant.Kind == "aws" {
			return resolved, []config.Diagnostic{{Severity: "warning", Code: "aws_all_profiles_readable", Path: "/aws", Message: "AWS/Leapp access exposes every profile in the approved directory; AWS_PROFILE selects a default but is not credential isolation"}}, nil
		}
	}
	return resolved, nil, nil
}

type selected[T any] struct {
	value  T
	source config.SourceRef
	rank   int
	set    bool
}

func (value *selected[T]) choose(candidate T, source config.SourceRef, rank int) {
	if !value.set || rank > value.rank {
		value.value = candidate
		value.source = source
		value.rank = rank
		value.set = true
	}
}

type importedLayer struct {
	document config.ConfigDocument
	sources  map[string]config.SourceRef
	declared map[string]bool
}

func ResolvePlan(input ResolveInput) (ExecutionPlan, error) {
	imports, err := decodeImports(input.Imported)
	if err != nil {
		return ExecutionPlan{}, err
	}
	plan := ExecutionPlan{
		ContractVersion: ContractVersion,
		Project:         input.Project,
		Repositories:    make([]RepositoryPlan, 0),
		Setup:           make([]ResolvedCommand, 0),
		Processes:       make([]ResolvedProcess, 0),
		Mounts:          make([]ResolvedMount, 0),
		Volumes:         make([]ResolvedVolume, 0),
		Auth:            AuthPlan{Imports: make([]string, 0)},
		Ports:           make([]PortRequest, 0),
		Bridges:         make([]BridgeGrant, 0),
		Provenance:      make(config.Provenance),
	}
	recordIdentitySources(&plan)

	plan.Agents = resolveAgents(input, imports, plan.Provenance)
	plan.Image, err = resolveImage(input, imports, plan.Provenance)
	if err != nil {
		return ExecutionPlan{}, err
	}
	plan.Repositories, err = resolveRepositories(input, plan.Provenance)
	if err != nil {
		return ExecutionPlan{}, err
	}
	plan.Setup = resolveSetup(input, imports, plan.Provenance)
	plan.Processes = resolveProcesses(input, plan.Provenance)
	plan.Mounts, err = resolveMounts(input, imports, plan.Provenance)
	if err != nil {
		return ExecutionPlan{}, err
	}
	plan.Volumes = resolveVolumes(input, plan.Provenance)
	plan.Auth = resolveAuth(input, plan.Provenance)
	plan.Ports, err = resolvePorts(input, imports, plan.Provenance)
	if err != nil {
		return ExecutionPlan{}, err
	}
	plan.Browser, err = resolveBrowser(input, plan.Provenance)
	if err != nil {
		return ExecutionPlan{}, err
	}
	plan.Bridges, err = resolveBridges(input, imports, plan.Provenance)
	if err != nil {
		return ExecutionPlan{}, err
	}
	plan.Limits, err = resolveLimits(input, imports, plan.Provenance)
	if err != nil {
		return ExecutionPlan{}, err
	}
	if err := SetExecutableHash(&plan); err != nil {
		return ExecutionPlan{}, fmt.Errorf("hash executable plan: %w", err)
	}
	return plan, nil
}

func recordIdentitySources(plan *ExecutionPlan) {
	putSource(plan.Provenance, "/contract_version", standardSource)
	putSource(plan.Provenance, "/project/id", detectedSource)
	putSource(plan.Provenance, "/project/canonical_root", detectedSource)
}

func resolveAgents(input ResolveInput, imports importedLayer, provenance config.Provenance) AgentPlan {
	allowed := append([]string(nil), input.Config.Document.Agents.Allowed...)
	if len(allowed) == 0 {
		allowed = []string{"codex"}
	}
	sort.Strings(allowed)
	var selectedDefault selected[string]
	if input.Defaults.DefaultAgent != "" {
		selectedDefault.choose(input.Defaults.DefaultAgent, standardSource, PriorityDefault)
	}
	if imports.declared["/agents/default"] {
		selectedDefault.choose(imports.document.Agents.Default, imports.source("/agents/default"), PriorityImport)
	}
	if projectDeclares(input.Config, "/agents/default", input.Config.Document.Agents.Default != "") {
		selectedDefault.choose(input.Config.Document.Agents.Default, projectSource(input.Config, "/agents/default"), PriorityProject)
	}
	if !selectedDefault.set {
		selectedDefault.choose(allowed[0], standardSource, PriorityDefault)
	}
	for index, agent := range allowed {
		putSource(provenance, fmt.Sprintf("/agents/allowed/%d", index), projectSource(input.Config, "/agents/allowed"))
		if agent == selectedDefault.value {
			putSource(provenance, "/agents/default", selectedDefault.source)
		}
	}
	return AgentPlan{Allowed: allowed, Default: selectedDefault.value}
}

func resolveImage(input ResolveInput, imports importedLayer, provenance config.Provenance) (ResolvedImage, error) {
	type branch struct {
		name   string
		rank   int
		source config.SourceRef
	}
	branches := make([]branch, 0, 4)
	if input.Defaults.ImageRef != "" {
		branches = append(branches, branch{name: "ref", rank: PriorityDefault, source: standardSource})
	}
	importRef := imports.declaredWithin("/image/ref")
	importBuild := imports.declaredWithin("/image/build")
	if importRef && importBuild {
		return ResolvedImage{}, fmt.Errorf("imported image cannot declare both /image/ref and /image/build")
	}
	if importRef {
		branches = append(branches, branch{name: "ref", rank: PriorityImport, source: imports.source("/image/ref")})
	}
	if importBuild {
		branches = append(branches, branch{name: "build", rank: PriorityImport, source: imports.source("/image/build")})
	}
	projectRef := projectDeclares(input.Config, "/image/ref", input.Config.Document.Image.Ref != "")
	projectBuild := projectDeclares(input.Config, "/image/build", input.Config.Document.Image.Build != nil)
	projectStandard := projectDeclares(input.Config, "/image/standard", input.Config.Document.Image.Standard)
	selectedProjectImages := 0
	for _, selected := range []bool{projectRef, projectBuild, projectStandard} {
		if selected {
			selectedProjectImages++
		}
	}
	if selectedProjectImages > 1 {
		return ResolvedImage{}, fmt.Errorf("project image cannot declare more than one of ref, build, or standard")
	}
	if projectRef {
		branches = append(branches, branch{name: "ref", rank: PriorityProject, source: projectSource(input.Config, "/image/ref")})
	}
	if projectBuild {
		branches = append(branches, branch{name: "build", rank: PriorityProject, source: projectSource(input.Config, "/image/build")})
	}
	if projectStandard {
		branches = append(branches, branch{name: "standard", rank: PriorityProject, source: projectSource(input.Config, "/image/standard")})
	}
	if len(branches) == 0 {
		return ResolvedImage{}, fmt.Errorf("no image was supplied by project, import, or defaults")
	}
	chosen := branches[0]
	for _, candidate := range branches[1:] {
		if candidate.rank > chosen.rank {
			chosen = candidate
		}
	}
	image := ResolvedImage{}
	switch chosen.name {
	case "ref":
		var reference selected[string]
		if input.Defaults.ImageRef != "" {
			reference.choose(input.Defaults.ImageRef, standardSource, PriorityDefault)
		}
		if importRef {
			reference.choose(imports.document.Image.Ref, imports.source("/image/ref"), PriorityImport)
		}
		if projectRef {
			reference.choose(input.Config.Document.Image.Ref, projectSource(input.Config, "/image/ref"), PriorityProject)
		}
		image.Reference = reference.value
		digest, digestErr := pinnedDigest(reference.value)
		if digestErr != nil {
			return ResolvedImage{}, digestErr
		}
		image.InputDigest = digest
		putSource(provenance, "/image/reference", reference.source)
	case "standard":
		if err := validateDigest("standard image", input.Authority.StandardImageDigest); err != nil {
			return ResolvedImage{}, err
		}
		image.Standard = true
		image.Context = "@dsx/standard"
		image.File = "Containerfile"
		image.InputDigest = input.Authority.StandardImageDigest
		putSource(provenance, "/image/standard", chosen.source)
		putSource(provenance, "/image/context", chosen.source)
		putSource(provenance, "/image/file", chosen.source)
	default:
		var contextValue, file, target selected[string]
		args := make(map[string]selected[string])
		if importBuild && imports.document.Image.Build != nil {
			mergeBuild(&contextValue, &file, &target, args, *imports.document.Image.Build, imports.source, PriorityImport)
		}
		if projectBuild {
			build := *input.Config.Document.Image.Build
			lookup := func(pointer string) config.SourceRef { return projectSource(input.Config, pointer) }
			mergeBuild(&contextValue, &file, &target, args, build, lookup, PriorityProject)
		}
		image.Context, image.File, image.Target = contextValue.value, file.value, target.value
		putSource(provenance, "/image/context", contextValue.source)
		putSource(provenance, "/image/file", file.source)
		if target.set {
			putSource(provenance, "/image/target", target.source)
		}
		keys := sortedKeys(args)
		image.BuildArgs = make([]KeyValue, 0, len(keys))
		for index, key := range keys {
			value := args[key]
			image.BuildArgs = append(image.BuildArgs, KeyValue{Key: key, Value: value.value})
			prefix := fmt.Sprintf("/image/build_args/%d", index)
			putSource(provenance, prefix+"/key", value.source)
			putSource(provenance, prefix+"/value", value.source)
		}
		if input.Authority.BuildContext == nil {
			return ResolvedImage{}, fmt.Errorf("build image context %q has no authority digest", image.Context)
		}
		if normalizeAuthorityPath(input.Authority.BuildContext.Path) != normalizeAuthorityPath(image.Context) {
			return ResolvedImage{}, fmt.Errorf("build authority path %q does not match selected context %q", input.Authority.BuildContext.Path, image.Context)
		}
		if err := validateDigest("build context", input.Authority.BuildContext.Digest); err != nil {
			return ResolvedImage{}, err
		}
		image.InputDigest = input.Authority.BuildContext.Digest
	}
	putSource(provenance, "/image/input_digest", standardSource)
	return image, nil
}

func mergeBuild(contextValue, file, target *selected[string], args map[string]selected[string], build config.ImageBuild, source func(string) config.SourceRef, rank int) {
	if build.Context != "" {
		contextValue.choose(build.Context, source("/image/build/context"), rank)
	}
	if build.File != "" {
		file.choose(build.File, source("/image/build/file"), rank)
	}
	if build.Target != "" {
		target.choose(build.Target, source("/image/build/target"), rank)
	}
	for key, candidate := range build.Args {
		value := args[key]
		pointer := "/image/build/args/" + escapePointerToken(key)
		value.choose(candidate, source(pointer), rank)
		args[key] = value
	}
}

func resolveRepositories(input ResolveInput, provenance config.Provenance) ([]RepositoryPlan, error) {
	root := input.Config.Document.Workspace.Root
	rootSource := projectSource(input.Config, "/workspace/root")
	importDigest, err := aggregateContentDigests(input.Authority.ImportedContent)
	if err != nil {
		return nil, err
	}
	members := append([]config.RepositoryMember(nil), input.Config.Document.Workspace.Members...)
	if len(members) == 0 {
		host := filepath.Clean(filepath.Join(input.Project.CanonicalRoot, root))
		repositories := []RepositoryPlan{{Name: "workspace", HostPath: host, GuestPath: "/workspace", TrackedDigest: importDigest}}
		putSource(provenance, "/repositories/0/name", standardSource)
		putSource(provenance, "/repositories/0/host_path", rootSource)
		putSource(provenance, "/repositories/0/guest_path", rootSource)
		putSource(provenance, "/repositories/0/tracked_digest", standardSource)
		return repositories, nil
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	repositories := make([]RepositoryPlan, 0, len(members))
	for index, member := range members {
		inputIndex := memberIndex(input.Config.Document.Workspace.Members, member.Name)
		base := fmt.Sprintf("/workspace/members/%d", inputIndex)
		relative, err := filepath.Rel(filepath.FromSlash(root), filepath.FromSlash(member.Path))
		if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
			return nil, fmt.Errorf("workspace member %q is not below workspace root %q", member.Path, root)
		}
		repositories = append(repositories, RepositoryPlan{
			Name:          member.Name,
			HostPath:      filepath.Clean(filepath.Join(input.Project.CanonicalRoot, filepath.FromSlash(member.Path))),
			GuestPath:     filepath.ToSlash(filepath.Join("/workspace", relative)),
			TrackedDigest: importDigest,
		})
		prefix := fmt.Sprintf("/repositories/%d", index)
		putSource(provenance, prefix+"/name", projectSource(input.Config, base+"/name"))
		putSource(provenance, prefix+"/host_path", projectSource(input.Config, base+"/path"))
		putSource(provenance, prefix+"/guest_path", projectSource(input.Config, base+"/path"))
		putSource(provenance, prefix+"/tracked_digest", standardSource)
	}
	return repositories, nil
}

func memberIndex(members []config.RepositoryMember, name string) int {
	for index, member := range members {
		if member.Name == name {
			return index
		}
	}
	return 0
}

func resolveSetup(input ResolveInput, imports importedLayer, provenance config.Provenance) []ResolvedCommand {
	specs := imports.document.Setup
	lookup := imports.source
	if projectDeclares(input.Config, "/setup", len(input.Config.Document.Setup) != 0) {
		specs = input.Config.Document.Setup
		lookup = func(pointer string) config.SourceRef { return projectSource(input.Config, pointer) }
	}
	commands := make([]ResolvedCommand, 0, len(specs))
	for index, spec := range specs {
		commands = append(commands, resolveCommand(spec, fmt.Sprintf("/setup/%d", index), fmt.Sprintf("/setup/%d", index), lookup, provenance))
	}
	return commands
}

func resolveProcesses(input ResolveInput, provenance config.Provenance) []ResolvedProcess {
	names := sortedKeys(input.Config.Document.Processes)
	processes := make([]ResolvedProcess, 0, len(names))
	for index, name := range names {
		spec := input.Config.Document.Processes[name]
		inputPrefix := "/processes/" + escapePointerToken(name)
		outputPrefix := fmt.Sprintf("/processes/%d", index)
		lookup := func(pointer string) config.SourceRef { return projectSource(input.Config, pointer) }
		process := ResolvedProcess{Name: name}
		process.Command = resolveCommand(spec.CommandSpec, inputPrefix, outputPrefix+"/command", lookup, provenance)
		process.DependsOn = append([]string(nil), spec.DependsOn...)
		sort.Strings(process.DependsOn)
		process.Required = spec.Required != nil && *spec.Required
		process.Terminal = spec.Terminal
		putSource(provenance, outputPrefix+"/name", projectSource(input.Config, inputPrefix))
		for dependencyIndex := range process.DependsOn {
			putSource(provenance, fmt.Sprintf("%s/depends_on/%d", outputPrefix, dependencyIndex), projectSource(input.Config, inputPrefix+"/dependsOn"))
		}
		if spec.Required != nil {
			putSource(provenance, outputPrefix+"/required", projectSource(input.Config, inputPrefix+"/required"))
		} else {
			putSource(provenance, outputPrefix+"/required", standardSource)
		}
		if spec.Terminal {
			putSource(provenance, outputPrefix+"/terminal", projectSource(input.Config, inputPrefix+"/terminal"))
		} else {
			putSource(provenance, outputPrefix+"/terminal", standardSource)
		}
		if spec.Health != nil {
			process.Health = resolveHealth(*spec.Health, inputPrefix+"/health", outputPrefix+"/health", lookup, provenance)
		}
		processes = append(processes, process)
	}
	return processes
}

func resolveHealth(spec config.HealthCheck, inputPrefix, outputPrefix string, source func(string) config.SourceRef, provenance config.Provenance) *ResolvedHealth {
	health := &ResolvedHealth{Retries: spec.Retries}
	switch {
	case spec.HTTP != nil:
		health.Kind, health.Target = "http", spec.HTTP.URL
		putSource(provenance, outputPrefix+"/kind", source(inputPrefix+"/http"))
		putSource(provenance, outputPrefix+"/target", source(inputPrefix+"/http/url"))
	case spec.TCP != nil:
		health.Kind, health.Target = "tcp", spec.TCP.Address
		putSource(provenance, outputPrefix+"/kind", source(inputPrefix+"/tcp"))
		putSource(provenance, outputPrefix+"/target", source(inputPrefix+"/tcp/address"))
	case spec.Command != nil:
		health.Kind = "command"
		command := resolveCommand(*spec.Command, inputPrefix+"/command", outputPrefix+"/command", source, provenance)
		health.Command = &command
		putSource(provenance, outputPrefix+"/kind", source(inputPrefix+"/command"))
	}
	health.IntervalMS = durationMilliseconds(spec.Interval)
	health.TimeoutMS = durationMilliseconds(spec.Timeout)
	if spec.Interval != "" {
		putSource(provenance, outputPrefix+"/interval_ms", source(inputPrefix+"/interval"))
	} else {
		putSource(provenance, outputPrefix+"/interval_ms", standardSource)
	}
	if spec.Timeout != "" {
		putSource(provenance, outputPrefix+"/timeout_ms", source(inputPrefix+"/timeout"))
	} else {
		putSource(provenance, outputPrefix+"/timeout_ms", standardSource)
	}
	if spec.Retries != 0 {
		putSource(provenance, outputPrefix+"/retries", source(inputPrefix+"/retries"))
	} else {
		putSource(provenance, outputPrefix+"/retries", standardSource)
	}
	return health
}

func durationMilliseconds(value string) int64 {
	duration, _ := time.ParseDuration(value)
	return duration.Milliseconds()
}

func resolveCommand(spec config.CommandSpec, inputPrefix, outputPrefix string, source func(string) config.SourceRef, provenance config.Provenance) ResolvedCommand {
	command := ResolvedCommand{
		Argv:      append([]string(nil), spec.Argv...),
		Shell:     spec.Shell,
		ShellPath: spec.ShellPath,
		Cwd:       spec.Cwd,
		Env:       make([]EnvGrant, 0, len(spec.Env)),
	}
	for index := range command.Argv {
		putSource(provenance, fmt.Sprintf("%s/argv/%d", outputPrefix, index), source(fmt.Sprintf("%s/argv/%d", inputPrefix, index)))
	}
	if spec.Shell != "" {
		putSource(provenance, outputPrefix+"/shell", source(inputPrefix+"/shell"))
	}
	if spec.ShellPath != "" {
		putSource(provenance, outputPrefix+"/shell_path", source(inputPrefix+"/shellPath"))
	}
	if spec.Cwd != "" {
		putSource(provenance, outputPrefix+"/cwd", source(inputPrefix+"/cwd"))
	} else {
		putSource(provenance, outputPrefix+"/cwd", standardSource)
	}
	names := sortedKeys(spec.Env)
	for index, name := range names {
		value := spec.Env[name]
		inputValue := inputPrefix + "/env/" + escapePointerToken(name)
		grant := EnvGrant{Name: name}
		valuePointer := inputValue
		switch {
		case value.SecretRef != "":
			grant.Reference, grant.Secret = value.SecretRef, true
			valuePointer += "/secretRef"
		case value.HostEnv != "":
			grant.Reference = value.HostEnv
			valuePointer += "/hostEnv"
		case value.Value != nil:
			grant.Value = *value.Value
			valuePointer += "/value"
		}
		command.Env = append(command.Env, grant)
		prefix := fmt.Sprintf("%s/env/%d", outputPrefix, index)
		putSource(provenance, prefix+"/name", source(inputValue))
		if grant.Reference != "" {
			putSource(provenance, prefix+"/reference", source(valuePointer))
		}
		if value.Value != nil {
			putSource(provenance, prefix+"/value", source(valuePointer))
		}
		putSource(provenance, prefix+"/secret", source(valuePointer))
	}
	return command
}

func resolveMounts(input ResolveInput, imports importedLayer, provenance config.Provenance) ([]ResolvedMount, error) {
	specs := imports.document.Mounts
	lookup := imports.source
	if projectDeclares(input.Config, "/mounts", len(input.Config.Document.Mounts) != 0) {
		specs = input.Config.Document.Mounts
		lookup = func(pointer string) config.SourceRef { return projectSource(input.Config, pointer) }
	}
	hostAuthorities := make(map[string]HostMountAuthority, len(input.Authority.HostMounts))
	for _, authority := range input.Authority.HostMounts {
		if authority.DeclaredPath == "" || authority.CanonicalPath == "" || authority.Identity == "" {
			return nil, fmt.Errorf("host mount authority is incomplete")
		}
		if _, exists := hostAuthorities[authority.DeclaredPath]; exists {
			return nil, fmt.Errorf("duplicate host mount authority for %q", authority.DeclaredPath)
		}
		hostAuthorities[authority.DeclaredPath] = authority
	}
	mounts := make([]ResolvedMount, 0, len(specs))
	usedAuthorities := make(map[string]struct{}, len(hostAuthorities))
	for index, spec := range specs {
		inputPrefix := fmt.Sprintf("/mounts/%d", index)
		outputPrefix := fmt.Sprintf("/mounts/%d", index)
		sourceValue := spec.Source.Path
		sourceIdentity := ""
		sourcePointer := inputPrefix + "/source/path"
		if spec.Source.Volume != "" {
			sourceValue = spec.Source.Volume
			sourcePointer = inputPrefix + "/source/volume"
		}
		if spec.Source.Type == "host" {
			authority, exists := hostAuthorities[spec.Source.Path]
			if !exists {
				return nil, fmt.Errorf("host mount %q has no filesystem authority", spec.Source.Path)
			}
			sourceValue = authority.CanonicalPath
			sourceIdentity = authority.Identity
			usedAuthorities[spec.Source.Path] = struct{}{}
			putSource(provenance, outputPrefix+"/source_identity", detectedSource)
		}
		mounts = append(mounts, ResolvedMount{SourceType: spec.Source.Type, Source: sourceValue, SourceIdentity: sourceIdentity, Target: spec.Target, ReadOnly: spec.ReadOnly})
		putSource(provenance, outputPrefix+"/source_type", lookup(inputPrefix+"/source/type"))
		putSource(provenance, outputPrefix+"/source", lookup(sourcePointer))
		putSource(provenance, outputPrefix+"/target", lookup(inputPrefix+"/target"))
		if spec.ReadOnly {
			putSource(provenance, outputPrefix+"/read_only", lookup(inputPrefix+"/readOnly"))
		} else {
			putSource(provenance, outputPrefix+"/read_only", standardSource)
		}
	}
	if len(usedAuthorities) != len(hostAuthorities) {
		return nil, fmt.Errorf("host mount authority does not match the selected mount configuration")
	}
	return mounts, nil
}

func resolveVolumes(input ResolveInput, provenance config.Provenance) []ResolvedVolume {
	names := sortedKeys(input.Config.Document.Volumes)
	volumes := make([]ResolvedVolume, 0, len(names))
	for index, name := range names {
		spec := input.Config.Document.Volumes[name]
		inputPrefix := "/volumes/" + escapePointerToken(name)
		outputPrefix := fmt.Sprintf("/volumes/%d", index)
		volumes = append(volumes, ResolvedVolume{Name: name, Target: spec.Target, Scope: spec.Scope, Persistent: spec.Persistent})
		putSource(provenance, outputPrefix+"/name", projectSource(input.Config, inputPrefix))
		putSource(provenance, outputPrefix+"/target", projectSource(input.Config, inputPrefix+"/target"))
		putSource(provenance, outputPrefix+"/scope", projectSource(input.Config, inputPrefix+"/scope"))
		if spec.Persistent {
			putSource(provenance, outputPrefix+"/persistent", projectSource(input.Config, inputPrefix+"/persistent"))
		} else {
			putSource(provenance, outputPrefix+"/persistent", standardSource)
		}
	}
	return volumes
}

func resolveAuth(input ResolveInput, provenance config.Provenance) AuthPlan {
	imports := append([]string(nil), input.Config.Document.Auth.Imports...)
	sort.Strings(imports)
	for index := range imports {
		putSource(provenance, fmt.Sprintf("/auth/imports/%d", index), projectSource(input.Config, "/auth/imports"))
	}
	return AuthPlan{Imports: imports}
}

type portCandidate struct {
	name, protocol, bind selected[string]
	guest                selected[uint16]
	host                 selected[config.HostPort]
}

func resolvePorts(input ResolveInput, imports importedLayer, provenance config.Provenance) ([]PortRequest, error) {
	ports := make(map[string]*portCandidate)
	for index, spec := range imports.document.Ports {
		if spec.Name == "" {
			return nil, fmt.Errorf("imported /ports/%d has no name", index)
		}
		mergePort(ports, spec, fmt.Sprintf("/ports/%d", index), imports.source, PriorityImport)
	}
	for index, spec := range input.Config.Document.Ports {
		lookup := func(pointer string) config.SourceRef { return projectSource(input.Config, pointer) }
		mergePort(ports, spec, fmt.Sprintf("/ports/%d", index), lookup, PriorityProject)
	}
	names := sortedKeys(ports)
	resolved := make([]PortRequest, 0, len(names))
	for index, name := range names {
		candidate := ports[name]
		if !candidate.protocol.set {
			candidate.protocol.choose("tcp", standardSource, PriorityDefault)
		}
		if !candidate.bind.set {
			candidate.bind.choose("127.0.0.1", standardSource, PriorityDefault)
		}
		address, err := netip.ParseAddr(candidate.bind.value)
		if err != nil {
			return nil, fmt.Errorf("port %q has invalid bind address %q: %w", name, candidate.bind.value, err)
		}
		port := PortRequest{Name: name, GuestPort: candidate.guest.value, Protocol: candidate.protocol.value, HostIP: address, ExplicitNonLoopbackGrant: !address.IsLoopback()}
		if candidate.host.set && !candidate.host.value.Dynamic {
			host := candidate.host.value.Port
			port.HostPort = &host
		}
		resolved = append(resolved, port)
		prefix := fmt.Sprintf("/ports/%d", index)
		putSource(provenance, prefix+"/name", candidate.name.source)
		putSource(provenance, prefix+"/guest_port", candidate.guest.source)
		putSource(provenance, prefix+"/protocol", candidate.protocol.source)
		putSource(provenance, prefix+"/host_ip", candidate.bind.source)
		if port.HostPort != nil {
			putSource(provenance, prefix+"/host_port", candidate.host.source)
		}
		putSource(provenance, prefix+"/explicit_non_loopback_grant", candidate.bind.source)
	}
	return resolved, nil
}

func mergePort(ports map[string]*portCandidate, spec config.PortConfig, prefix string, source func(string) config.SourceRef, rank int) {
	candidate := ports[spec.Name]
	if candidate == nil {
		candidate = &portCandidate{}
		ports[spec.Name] = candidate
	}
	candidate.name.choose(spec.Name, source(prefix+"/name"), rank)
	candidate.guest.choose(spec.Guest, source(prefix+"/guest"), rank)
	candidate.host.choose(spec.Host, source(prefix+"/host"), rank)
	if spec.Protocol != "" {
		candidate.protocol.choose(spec.Protocol, source(prefix+"/protocol"), rank)
	}
	if spec.Bind != "" {
		candidate.bind.choose(spec.Bind, source(prefix+"/bind"), rank)
	}
}

func resolveBrowser(input ResolveInput, provenance config.Provenance) (*BrowserPlan, error) {
	if input.Authority.BrowserImageReference == "" && input.Authority.BrowserImageDigest == "" {
		return nil, nil
	}
	putSource(provenance, "/browser/image_reference", standardSource)
	putSource(provenance, "/browser/image_digest", standardSource)
	digest, err := pinnedDigest(input.Authority.BrowserImageReference)
	if err != nil {
		return nil, fmt.Errorf("browser image authority: %w", err)
	}
	if err := validateDigest("browser image", input.Authority.BrowserImageDigest); err != nil {
		return nil, err
	}
	if digest != strings.ToLower(input.Authority.BrowserImageDigest) {
		return nil, fmt.Errorf("browser image reference digest does not match authority digest")
	}
	return &BrowserPlan{ImageReference: input.Authority.BrowserImageReference, ImageDigest: digest}, nil
}

func resolveBridges(input ResolveInput, imports importedLayer, provenance config.Provenance) ([]BridgeGrant, error) {
	var internet selected[bool]
	internet.choose(input.Defaults.Internet, standardSource, PriorityDefault)
	if imports.declared["/network/internet"] {
		internet.choose(*imports.document.Network.Internet, imports.source("/network/internet"), PriorityImport)
	}
	if input.Config.Document.Network.Internet != nil {
		internet.choose(*input.Config.Document.Network.Internet, projectSource(input.Config, "/network/internet"), PriorityProject)
	}
	type sourcedBridge struct {
		grant  BridgeGrant
		source map[string]config.SourceRef
	}
	bridges := make([]sourcedBridge, 0, 1+len(input.Config.Document.Network.HostGrants))
	if internet.value {
		bridges = append(bridges, sourcedBridge{grant: BridgeGrant{Kind: "internet", Name: "internet"}, source: map[string]config.SourceRef{"kind": internet.source, "name": internet.source, "read_only": internet.source}})
	}
	for index, grant := range input.Config.Document.Network.HostGrants {
		prefix := fmt.Sprintf("/network/hostGrants/%d", index)
		bridges = append(bridges, sourcedBridge{grant: BridgeGrant{Kind: "host", Name: grant.Name, Destination: grant.Destination, Port: grant.Port}, source: map[string]config.SourceRef{
			"kind": projectSource(input.Config, prefix), "name": projectSource(input.Config, prefix+"/name"), "destination": projectSource(input.Config, prefix+"/destination"), "port": projectSource(input.Config, prefix+"/port"), "read_only": standardSource,
		}})
	}
	if input.Config.Document.AWS.Mode == "leapp" {
		authority := input.Authority.LeappDirectory
		if authority == nil || authority.DeclaredPath == "" || authority.CanonicalPath == "" || authority.Identity == "" {
			return nil, fmt.Errorf("Leapp directory authority is incomplete")
		}
		if authority.DeclaredPath != input.Config.Document.AWS.Directory {
			return nil, fmt.Errorf("Leapp directory authority does not match configured directory")
		}
		bridges = append(bridges, sourcedBridge{grant: BridgeGrant{Kind: "aws", Name: input.Config.Document.AWS.Profile, Destination: authority.CanonicalPath, SourceIdentity: authority.Identity, ReadOnly: true}, source: map[string]config.SourceRef{
			"kind": projectSource(input.Config, "/aws/mode"), "name": projectSource(input.Config, "/aws/profile"), "destination": projectSource(input.Config, "/aws/directory"), "source_identity": detectedSource, "read_only": projectSource(input.Config, "/aws/mode"),
		}})
	} else if input.Authority.LeappDirectory != nil {
		return nil, fmt.Errorf("Leapp directory authority exists without an AWS grant")
	}
	sort.Slice(bridges, func(i, j int) bool {
		if bridges[i].grant.Kind != bridges[j].grant.Kind {
			return bridges[i].grant.Kind < bridges[j].grant.Kind
		}
		return bridges[i].grant.Name < bridges[j].grant.Name
	})
	resolved := make([]BridgeGrant, 0, len(bridges))
	for index, bridge := range bridges {
		resolved = append(resolved, bridge.grant)
		prefix := fmt.Sprintf("/bridges/%d", index)
		putSource(provenance, prefix+"/kind", bridge.source["kind"])
		putSource(provenance, prefix+"/name", bridge.source["name"])
		if bridge.grant.Destination != "" {
			putSource(provenance, prefix+"/destination", bridge.source["destination"])
		}
		if bridge.grant.SourceIdentity != "" {
			putSource(provenance, prefix+"/source_identity", bridge.source["source_identity"])
		}
		if bridge.grant.Port != 0 {
			putSource(provenance, prefix+"/port", bridge.source["port"])
		}
		putSource(provenance, prefix+"/read_only", bridge.source["read_only"])
	}
	return resolved, nil
}

func resolveLimits(input ResolveInput, imports importedLayer, provenance config.Provenance) (ResourceLimits, error) {
	var cpus selected[int]
	var memory selected[int64]
	var workspaces selected[int]
	cpus.choose(input.Defaults.CPUs, standardSource, PriorityDefault)
	memory.choose(input.Defaults.MemoryBytes, standardSource, PriorityDefault)
	workspaces.choose(input.Defaults.MaxConcurrentWorkspaces, standardSource, PriorityDefault)
	if imports.declared["/resources/cpus"] {
		cpus.choose(imports.document.Resources.CPUs, imports.source("/resources/cpus"), PriorityImport)
	}
	if imports.declared["/resources/memory"] {
		bytes, err := parseMemoryBytes(imports.document.Resources.Memory)
		if err != nil {
			return ResourceLimits{}, fmt.Errorf("imported /resources/memory: %w", err)
		}
		memory.choose(bytes, imports.source("/resources/memory"), PriorityImport)
	}
	if imports.declared["/resources/maxConcurrentWorkspaces"] {
		workspaces.choose(imports.document.Resources.MaxConcurrentWorkspaces, imports.source("/resources/maxConcurrentWorkspaces"), PriorityImport)
	}
	resources := input.Config.Document.Resources
	if projectDeclares(input.Config, "/resources/cpus", resources.CPUs != 0) {
		cpus.choose(resources.CPUs, projectSource(input.Config, "/resources/cpus"), PriorityProject)
	}
	if projectDeclares(input.Config, "/resources/memory", resources.Memory != "") {
		bytes, err := parseMemoryBytes(resources.Memory)
		if err != nil {
			return ResourceLimits{}, fmt.Errorf("project /resources/memory: %w", err)
		}
		memory.choose(bytes, projectSource(input.Config, "/resources/memory"), PriorityProject)
	}
	if projectDeclares(input.Config, "/resources/maxConcurrentWorkspaces", resources.MaxConcurrentWorkspaces != 0) {
		workspaces.choose(resources.MaxConcurrentWorkspaces, projectSource(input.Config, "/resources/maxConcurrentWorkspaces"), PriorityProject)
	}
	if input.CLI.CPUs != nil {
		cpus.choose(*input.CLI.CPUs, cliSource("--cpus"), PriorityCLI)
	}
	if input.CLI.Memory != "" {
		bytes, err := parseMemoryBytes(input.CLI.Memory)
		if err != nil {
			return ResourceLimits{}, fmt.Errorf("CLI --memory: %w", err)
		}
		memory.choose(bytes, cliSource("--memory"), PriorityCLI)
	}
	putSource(provenance, "/limits/cpus", cpus.source)
	putSource(provenance, "/limits/memory_bytes", memory.source)
	putSource(provenance, "/limits/max_concurrent_workspaces", workspaces.source)
	return ResourceLimits{CPUs: cpus.value, MemoryBytes: memory.value, MaxConcurrentWorkspaces: workspaces.value}, nil
}

func parseMemoryBytes(value string) (int64, error) {
	var multiplier int64
	switch {
	case strings.HasSuffix(value, "MiB"):
		multiplier = 1024 * 1024
		value = strings.TrimSuffix(value, "MiB")
	case strings.HasSuffix(value, "GiB"):
		multiplier = 1024 * 1024 * 1024
		value = strings.TrimSuffix(value, "GiB")
	default:
		return 0, fmt.Errorf("memory must use MiB or GiB")
	}
	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil || amount <= 0 || amount > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("memory amount is invalid")
	}
	return amount * multiplier, nil
}

func projectDeclares(validated config.ValidatedConfig, pointer string, inferred bool) bool {
	if inferred {
		return true
	}
	if _, ok := validated.Provenance[pointer]; ok {
		return true
	}
	_, ok := validated.SourceLocations[pointer]
	return ok
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (layer importedLayer) source(pointer string) config.SourceRef {
	for {
		if source, ok := layer.sources[pointer]; ok {
			return source
		}
		index := strings.LastIndexByte(pointer, '/')
		if index < 0 {
			return importedSource(config.SourceRef{Kind: "imported"})
		}
		pointer = pointer[:index]
	}
}

func (layer importedLayer) declaredWithin(pointer string) bool {
	for declared := range layer.declared {
		if declared == pointer || strings.HasPrefix(declared, pointer+"/") {
			return true
		}
	}
	return false
}

func decodeImports(values []ImportedValue) (importedLayer, error) {
	layer := importedLayer{sources: make(map[string]config.SourceRef), declared: make(map[string]bool)}
	sorted := append([]ImportedValue(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Pointer < sorted[j].Pointer })
	for _, imported := range sorted {
		if layer.declared[imported.Pointer] {
			return importedLayer{}, fmt.Errorf("duplicate imported pointer %q", imported.Pointer)
		}
		parts, err := parsePointer(imported.Pointer)
		if err != nil {
			return importedLayer{}, err
		}
		if err := layer.apply(imported.Pointer, parts, imported.Value); err != nil {
			return importedLayer{}, err
		}
		layer.declared[imported.Pointer] = true
		layer.sources[imported.Pointer] = importedSource(imported.Source)
	}
	return layer, nil
}

func (layer *importedLayer) apply(pointer string, parts []string, value any) error {
	typeError := func(expected string) error {
		return fmt.Errorf("imported pointer %q requires %s, got %T", pointer, expected, value)
	}
	if len(parts) == 2 && parts[0] == "image" && parts[1] == "ref" {
		candidate, ok := value.(string)
		if !ok {
			return typeError("string")
		}
		layer.document.Image.Ref = candidate
		return nil
	}
	if len(parts) == 2 && parts[0] == "image" && parts[1] == "build" {
		candidate, ok := value.(config.ImageBuild)
		if !ok {
			return typeError("config.ImageBuild")
		}
		layer.document.Image.Build = &candidate
		return nil
	}
	if len(parts) >= 3 && parts[0] == "image" && parts[1] == "build" {
		if layer.document.Image.Build == nil {
			layer.document.Image.Build = &config.ImageBuild{}
		}
		build := layer.document.Image.Build
		if len(parts) == 3 {
			candidate, ok := value.(string)
			if !ok {
				return typeError("string")
			}
			switch parts[2] {
			case "context":
				build.Context = candidate
			case "file":
				build.File = candidate
			case "target":
				build.Target = candidate
			default:
				return fmt.Errorf("unsupported imported pointer %q", pointer)
			}
			return nil
		}
		if len(parts) == 4 && parts[2] == "args" {
			candidate, ok := value.(string)
			if !ok {
				return typeError("string")
			}
			if build.Args == nil {
				build.Args = make(map[string]string)
			}
			build.Args[parts[3]] = candidate
			return nil
		}
	}
	if len(parts) == 1 && parts[0] == "setup" {
		candidate, ok := value.([]config.CommandSpec)
		if !ok {
			return typeError("[]config.CommandSpec")
		}
		layer.document.Setup = append([]config.CommandSpec(nil), candidate...)
		return nil
	}
	if len(parts) == 1 && parts[0] == "mounts" {
		candidate, ok := value.([]config.MountSpec)
		if !ok {
			return typeError("[]config.MountSpec")
		}
		layer.document.Mounts = append([]config.MountSpec(nil), candidate...)
		return nil
	}
	if len(parts) == 1 && parts[0] == "ports" {
		candidate, ok := value.([]config.PortConfig)
		if !ok {
			return typeError("[]config.PortConfig")
		}
		layer.document.Ports = append([]config.PortConfig(nil), candidate...)
		return nil
	}
	if len(parts) == 2 && (parts[0] == "setup" || parts[0] == "mounts" || parts[0] == "ports") {
		index, err := strconv.Atoi(parts[1])
		if err != nil || index < 0 {
			return fmt.Errorf("unsupported imported pointer %q", pointer)
		}
		switch parts[0] {
		case "setup":
			candidate, ok := value.(config.CommandSpec)
			if !ok {
				return typeError("config.CommandSpec")
			}
			layer.document.Setup = grow(layer.document.Setup, index)
			layer.document.Setup[index] = candidate
		case "mounts":
			candidate, ok := value.(config.MountSpec)
			if !ok {
				return typeError("config.MountSpec")
			}
			layer.document.Mounts = grow(layer.document.Mounts, index)
			layer.document.Mounts[index] = candidate
		case "ports":
			candidate, ok := value.(config.PortConfig)
			if !ok {
				return typeError("config.PortConfig")
			}
			layer.document.Ports = grow(layer.document.Ports, index)
			layer.document.Ports[index] = candidate
		}
		return nil
	}
	if len(parts) == 2 && parts[0] == "agents" && parts[1] == "default" {
		candidate, ok := value.(string)
		if !ok {
			return typeError("string")
		}
		layer.document.Agents.Default = candidate
		return nil
	}
	if len(parts) == 2 && parts[0] == "network" && parts[1] == "internet" {
		candidate, ok := value.(bool)
		if !ok {
			return typeError("bool")
		}
		layer.document.Network.Internet = &candidate
		return nil
	}
	if len(parts) == 2 && parts[0] == "resources" {
		switch parts[1] {
		case "cpus":
			candidate, ok := value.(int)
			if !ok {
				return typeError("int")
			}
			layer.document.Resources.CPUs = candidate
		case "memory":
			candidate, ok := value.(string)
			if !ok {
				return typeError("string")
			}
			layer.document.Resources.Memory = candidate
		case "maxConcurrentWorkspaces":
			candidate, ok := value.(int)
			if !ok {
				return typeError("int")
			}
			layer.document.Resources.MaxConcurrentWorkspaces = candidate
		default:
			return fmt.Errorf("unsupported imported pointer %q", pointer)
		}
		return nil
	}
	return fmt.Errorf("unsupported imported pointer %q", pointer)
}

func grow[T any](values []T, index int) []T {
	if index < len(values) {
		return values
	}
	return append(values, make([]T, index-len(values)+1)...)
}

func pinnedDigest(reference string) (string, error) {
	const marker = "@sha256:"
	index := strings.LastIndex(reference, marker)
	if index < 1 {
		return "", fmt.Errorf("image reference %q is not digest-pinned", reference)
	}
	digest := strings.ToLower(reference[index+len(marker):])
	if err := validateDigest("image reference", digest); err != nil {
		return "", err
	}
	return digest, nil
}

func validateDigest(kind, digest string) error {
	if len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
		return fmt.Errorf("%s authority digest must be a lowercase SHA-256 digest", kind)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("%s authority digest must be a lowercase SHA-256 digest", kind)
	}
	return nil
}

func normalizeAuthorityPath(value string) string {
	return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(value))), "./")
}

func aggregateContentDigests(inputs []ContentDigest) (string, error) {
	if len(inputs) == 0 {
		return "", nil
	}
	sorted := append([]ContentDigest(nil), inputs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		return sorted[i].Digest < sorted[j].Digest
	})
	hash := sha256.New()
	_, _ = hash.Write([]byte("dsx.imported-content/v1\x00"))
	previousPath := ""
	for index, input := range sorted {
		path := normalizeAuthorityPath(input.Path)
		if path == "" || path == "." || filepath.IsAbs(filepath.FromSlash(path)) {
			return "", fmt.Errorf("imported content authority path %q is invalid", input.Path)
		}
		if index > 0 && path == previousPath {
			return "", fmt.Errorf("duplicate imported content authority path %q", path)
		}
		if err := validateDigest("imported content", input.Digest); err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(hash, "%d:%s:%s\x00", len(path), path, input.Digest)
		previousPath = path
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
