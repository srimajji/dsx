package plan

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/model"
)

func TestPrecedencePerLeafAndDeterministicSmoke(t *testing.T) {
	input := smokeResolveInput(false)
	plan, diagnostics, err := NewResolver().Resolve(context.Background(), input)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("Resolve() diagnostics = %#v", diagnostics)
	}

	if plan.Agent != "cli-agent" {
		t.Fatalf("Agent = %q, want CLI value", plan.Agent)
	}
	if plan.Limits.CPUs != 8 || plan.Limits.MemoryBytes != 6*1024*1024*1024 || plan.Limits.MaxConcurrentClones != 3 {
		t.Fatalf("Limits = %#v, want CLI CPU/memory and project clones", plan.Limits)
	}
	if plan.Image.Context != "project-context" || plan.Image.File != "Projectfile" || plan.Image.Target != "import-target" {
		t.Fatalf("Image = %#v, want project required leaves and imported target", plan.Image)
	}
	if got, want := plan.Image.BuildArgs, []KeyValue{{Key: "a", Value: "project-a"}, {Key: "b", Value: "import-b"}, {Key: "z", Value: "project-z"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgs = %#v, want %#v", got, want)
	}
	if got := namesOfProcesses(plan.Processes); !reflect.DeepEqual(got, []string{"api", "web"}) {
		t.Fatalf("process order = %#v", got)
	}
	if got := namesOfPorts(plan.Ports); !reflect.DeepEqual(got, []string{"forwarded", "web"}) {
		t.Fatalf("port order = %#v", got)
	}
	web := plan.Ports[1]
	if web.GuestPort != 3000 || web.HostPort != nil || web.Protocol != "udp" || web.HostIP.String() != "127.0.0.2" || web.ExplicitNonLoopbackGrant {
		t.Fatalf("web port = %#v, want clone-dynamic host port with approved guest, protocol, and loopback bind", web)
	}
	if len(plan.Bridges) != 1 || plan.Bridges[0].Kind != "host" {
		t.Fatalf("Bridges = %#v, project false must override imported/default internet", plan.Bridges)
	}
	if plan.Browser == nil || !plan.Browser.Enabled {
		t.Fatalf("Browser = %#v, want CLI enabled", plan.Browser)
	}

	firstJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("Marshal(first plan): %v", err)
	}
	permuted, err := ResolvePlan(smokeResolveInput(true))
	if err != nil {
		t.Fatalf("ResolvePlan(permuted) error = %v", err)
	}
	secondJSON, err := json.Marshal(permuted)
	if err != nil {
		t.Fatalf("Marshal(permuted plan): %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("map insertion order changed resolved JSON\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}
}

func TestClonePlansForceConfiguredFixedHostPortsDynamicWhileLiveRemainsFixed(t *testing.T) {
	firstInput := smokeResolveInput(false)
	firstInput.Sandbox.Name = "first"
	first, err := ResolvePlan(firstInput)
	if err != nil {
		t.Fatal(err)
	}
	secondInput := smokeResolveInput(false)
	secondInput.Sandbox.Name = "second"
	second, err := ResolvePlan(secondInput)
	if err != nil {
		t.Fatal(err)
	}
	for name, resolved := range map[string]ExecutionPlan{"first": first, "second": second} {
		web := resolved.Ports[1]
		if web.HostPort != nil || web.GuestPort != 3000 || web.Protocol != "udp" || web.HostIP.String() != "127.0.0.2" || web.ExplicitNonLoopbackGrant {
			t.Fatalf("%s clone port = %#v, want nil dynamic host port with UDP guest 3000 bound to loopback 127.0.0.2", name, web)
		}
		encoded, marshalErr := json.Marshal(resolved)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(encoded), `"host_port":3100`) {
			t.Fatalf("%s clone inspection retained fixed host port: %s", name, encoded)
		}
	}
	if first.ExecutableHash != second.ExecutableHash {
		t.Fatal("sandbox display identity unexpectedly changed executable approval hash")
	}
	fixedClone := first
	fixedPort := uint16(3100)
	fixedClone.Ports[1].HostPort = &fixedPort
	if err := SetExecutableHash(&fixedClone); err != nil {
		t.Fatal(err)
	}
	if fixedClone.ExecutableHash == first.ExecutableHash {
		t.Fatal("clone dynamic host-port transformation is absent from executable hash")
	}

	liveInput := smokeResolveInput(false)
	liveInput.Mode = model.ModeLive
	liveInput.Sandbox.Name = "main"
	live, err := ResolvePlan(liveInput)
	if err != nil {
		t.Fatal(err)
	}
	if live.Ports[1].HostPort == nil || *live.Ports[1].HostPort != 3100 || live.Ports[1].GuestPort != 3000 || live.Ports[1].Protocol != "udp" || live.Ports[1].HostIP.String() != "127.0.0.2" || live.Ports[1].ExplicitNonLoopbackGrant {
		t.Fatalf("live fixed host port = %#v, want host 3100 with UDP guest 3000 bound to loopback 127.0.0.2", live.Ports[1])
	}
	if live.ExecutableHash == first.ExecutableHash {
		t.Fatal("clone port transformation is absent from executable hash")
	}
}

func TestProvenanceExactLocationsAndEveryEffectiveLeaf(t *testing.T) {
	plan, err := ResolvePlan(smokeResolveInput(false))
	if err != nil {
		t.Fatalf("ResolvePlan() error = %v", err)
	}
	assertSource := func(pointer, kind, path string, line, column, priority int) {
		t.Helper()
		got, ok := plan.Provenance[pointer]
		if !ok {
			t.Fatalf("no provenance for %s", pointer)
		}
		want := config.SourceRef{Kind: kind, Path: path, Line: line, Column: column, Priority: priority}
		if got != want {
			t.Fatalf("provenance[%q] = %#v, want %#v", pointer, got, want)
		}
	}
	assertSource("/agent", "cli", "--agent", 0, 0, PriorityCLI)
	assertSource("/limits/cpus", "cli", "--cpus", 0, 0, PriorityCLI)
	assertSource("/limits/max_concurrent_clones", "project", ".dsx/config.jsonc", 40, 9, PriorityProject)
	assertSource("/image/context", "project", ".dsx/config.jsonc", 12, 7, PriorityProject)
	assertSource("/image/target", "detected", "package.json", 5, 13, PriorityImport)
	assertSource("/ports/1/guest_port", "project", ".dsx/config.jsonc", 31, 17, PriorityProject)
	assertSource("/ports/1/protocol", "detected", "package.json", 20, 3, PriorityImport)

	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("Marshal(plan): %v", err)
	}
	if strings.Contains(string(encoded), "super-secret-value") {
		t.Fatalf("resolved JSON exposed a secret value: %s", encoded)
	}
	var tree map[string]any
	if err := json.Unmarshal(encoded, &tree); err != nil {
		t.Fatalf("Unmarshal(plan): %v", err)
	}
	delete(tree, "provenance")
	delete(tree, "executable_hash") // DSX-013 owns this leaf.
	for _, pointer := range jsonLeafPointers(tree, "") {
		if _, ok := plan.Provenance[pointer]; !ok {
			t.Errorf("effective leaf %s has no provenance", pointer)
		}
	}
}

func TestProvenanceRejectsUnsupportedImports(t *testing.T) {
	tests := []struct {
		name     string
		imported ImportedValue
		contains string
	}{
		{name: "unknown pointer", imported: ImportedValue{Pointer: "/processes/host", Value: config.ProcessSpec{}}, contains: "unsupported imported pointer"},
		{name: "incompatible type", imported: ImportedValue{Pointer: "/resources/cpus", Value: "eight"}, contains: "requires int"},
		{name: "invalid pointer", imported: ImportedValue{Pointer: "/image/~2ref", Value: "bad"}, contains: "invalid RFC 6901 escape"},
		{name: "duplicate pointer", imported: ImportedValue{Pointer: "/agents/default", Value: "one"}, contains: "duplicate imported pointer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := smokeResolveInput(false)
			input.Imported = []ImportedValue{test.imported}
			if test.name == "duplicate pointer" {
				input.Imported = append(input.Imported, test.imported)
			}
			_, err := ResolvePlan(input)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("ResolvePlan() error = %v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestInputDigestPinnedImageAndBrowserFailClosed(t *testing.T) {
	input := ResolveInput{
		Config: config.ValidatedConfig{Document: config.ConfigDocument{
			Workspace: config.WorkspaceConfig{Root: "."},
			Image:     config.ImageConfig{Ref: "example/dev@sha256:" + strings.Repeat("a", 64)},
		}},
		Project:  ProjectIdentity{CanonicalRoot: "/project"},
		Mode:     model.ModeLive,
		Defaults: DefaultValues{Internet: true, CPUs: 2, MemoryBytes: 2 << 30, MaxConcurrentClones: 1},
	}
	resolved, err := ResolvePlan(input)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Image.InputDigest != strings.Repeat("a", 64) {
		t.Fatalf("InputDigest = %q, want digest from pinned image reference", resolved.Image.InputDigest)
	}
	if resolved.Agent != "codex" {
		t.Fatalf("Agent = %q, want codex default", resolved.Agent)
	}

	enabled := true
	input.CLI.Browser = &enabled
	if _, err := ResolvePlan(input); err == nil || !strings.Contains(err.Error(), "browser image authority") {
		t.Fatalf("ResolvePlan() browser error = %v, want missing pinned authority", err)
	}
	input.Authority.BrowserImageDigest = strings.Repeat("b", 64)
	input.Authority.BrowserImageReference = "example/browser@sha256:" + strings.Repeat("b", 64)
	resolved, err = ResolvePlan(input)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Browser.ImageDigest != strings.Repeat("b", 64) || resolved.Browser.ImageReference != input.Authority.BrowserImageReference {
		t.Fatalf("Browser = %#v", resolved.Browser)
	}
}

func TestMemberPathCompositeRootResolution(t *testing.T) {
	input := ResolveInput{
		Config: config.ValidatedConfig{Document: config.ConfigDocument{
			Workspace: config.WorkspaceConfig{Root: "monorepo", Members: []config.RepositoryMember{{Name: "api", Path: "monorepo/services/api"}}},
			Image:     config.ImageConfig{Ref: "example/dev@sha256:" + strings.Repeat("a", 64)},
		}},
		Project:  ProjectIdentity{CanonicalRoot: "/project"},
		Mode:     model.ModeLive,
		Defaults: DefaultValues{CPUs: 2, MemoryBytes: 2 << 30, MaxConcurrentClones: 1},
	}
	resolved, err := ResolvePlan(input)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolved.Repositories[0].HostPath, filepath.Join("/project", "monorepo/services/api"); got != want {
		t.Fatalf("HostPath = %q, want %q", got, want)
	}
	if got, want := resolved.Repositories[0].GuestPath, "/workspace/services/api"; got != want {
		t.Fatalf("GuestPath = %q, want %q", got, want)
	}
}

func smokeResolveInput(permuted bool) ResolveInput {
	secretReference := "provider-token"
	processEntries := []struct {
		name string
		spec config.ProcessSpec
	}{
		{name: "web", spec: config.ProcessSpec{CommandSpec: config.CommandSpec{Argv: []string{"pnpm", "dev"}, Cwd: "/workspace", Env: map[string]config.EnvValue{"TOKEN": {SecretRef: secretReference}, "MODE": {Value: stringPointer("dev")}}}, DependsOn: []string{"api"}, Required: boolPointer(true)}},
		{name: "api", spec: config.ProcessSpec{CommandSpec: config.CommandSpec{Argv: []string{"go", "run", "./cmd/api"}, Cwd: "/workspace"}, Required: boolPointer(true)}},
	}
	volumeEntries := []struct {
		name string
		spec config.VolumeSpec
	}{
		{name: "node_modules", spec: config.VolumeSpec{Target: "/workspace/node_modules", Scope: "sandbox"}},
		{name: "cache", spec: config.VolumeSpec{Target: "/workspace/.cache", Scope: "project", Persistent: true}},
	}
	authEntries := []struct {
		name string
		spec config.AuthProfileConfig
	}{
		{name: "codex-main", spec: config.AuthProfileConfig{Harness: "codex", Persistence: "global"}},
		{name: "claude-work", spec: config.AuthProfileConfig{Harness: "claude", Persistence: "project"}},
	}
	if permuted {
		reverse(processEntries)
		reverse(volumeEntries)
		reverse(authEntries)
	}
	processes := make(map[string]config.ProcessSpec)
	for _, entry := range processEntries {
		processes[entry.name] = entry.spec
	}
	volumes := make(map[string]config.VolumeSpec)
	for _, entry := range volumeEntries {
		volumes[entry.name] = entry.spec
	}
	auth := make(map[string]config.AuthProfileConfig)
	for _, entry := range authEntries {
		auth[entry.name] = entry.spec
	}
	buildArgs := make(map[string]string)
	if permuted {
		buildArgs["z"], buildArgs["a"] = "project-z", "project-a"
	} else {
		buildArgs["a"], buildArgs["z"] = "project-a", "project-z"
	}
	locations := map[string]config.SourceLocation{
		"/workspace/root":                {Line: 3, Column: 11},
		"/workspace/members/0/name":      {Line: 4, Column: 15},
		"/workspace/members/0/path":      {Line: 4, Column: 31},
		"/workspace/members/1/name":      {Line: 5, Column: 15},
		"/workspace/members/1/path":      {Line: 5, Column: 32},
		"/image/build":                   {Line: 11, Column: 5},
		"/image/build/context":           {Line: 12, Column: 7},
		"/image/build/file":              {Line: 13, Column: 7},
		"/image/build/args/a":            {Line: 14, Column: 12},
		"/image/build/args/z":            {Line: 14, Column: 30},
		"/agents/default":                {Line: 24, Column: 16},
		"/processes/web":                 {Line: 25, Column: 5},
		"/processes/api":                 {Line: 26, Column: 5},
		"/ports/0/name":                  {Line: 30, Column: 10},
		"/ports/0/guest":                 {Line: 31, Column: 17},
		"/ports/0/host":                  {Line: 32, Column: 16},
		"/network/internet":              {Line: 35, Column: 17},
		"/resources/cpus":                {Line: 38, Column: 9},
		"/resources/memory":              {Line: 39, Column: 9},
		"/resources/maxConcurrentClones": {Line: 40, Column: 9},
	}
	project := config.ValidatedConfig{
		SourcePath:      ".dsx/config.jsonc",
		SourceLocations: locations,
		Document: config.ConfigDocument{
			Workspace:    config.WorkspaceConfig{Root: ".", Members: []config.RepositoryMember{{Name: "zeta", Path: "services/zeta"}, {Name: "api", Path: "services/api"}}},
			Image:        config.ImageConfig{Build: &config.ImageBuild{Context: "project-context", File: "Projectfile", Args: buildArgs}},
			Setup:        []config.CommandSpec{{Argv: []string{"pnpm", "install"}, Cwd: "/workspace"}},
			Processes:    processes,
			Volumes:      volumes,
			Mounts:       []config.MountSpec{{Source: config.MountSource{Type: "volume", Volume: "node_modules"}, Target: "/workspace/node_modules"}},
			Agents:       config.AgentConfig{Default: "project-agent"},
			AuthProfiles: auth,
			Browser:      config.BrowserConfig{Enabled: false},
			Network:      config.NetworkConfig{Internet: boolPointer(false), HostGrants: []config.HostGrant{{Name: "database", Destination: "db.internal", Port: 5432}}},
			Ports:        []config.PortConfig{{Name: "web", Guest: 3000, Host: config.HostPort{Port: 3100}}},
			Resources:    config.ResourceLimits{CPUs: 4, Memory: "4GiB", MaxConcurrentClones: 3},
		},
	}
	importSource := config.SourceRef{Kind: "detected", Path: "package.json", Line: 20, Column: 3}
	return ResolveInput{
		Config:    project,
		Project:   ProjectIdentity{ID: model.ProjectID("abcdefghijklmnopqrst"), CanonicalRoot: "/work/project"},
		Sandbox:   SandboxIdentity{Name: model.SandboxName("smoke"), RunID: model.RunID("018f0000-0000-7000-8000-000000000000")},
		Mode:      model.ModeClone,
		Ownership: OwnershipPlan{Labels: []KeyValue{{Key: "dsx.project", Value: "abcdefghijklmnopqrst"}}, ResourceName: "dsx-smoke"},
		CLI:       CLIOverrides{Agent: "cli-agent", Browser: boolPointer(true), CPUs: intPointer(8), Memory: "6GiB"},
		Imported: []ImportedValue{
			{Pointer: "/agents/default", Value: "import-agent", Source: importSource},
			{Pointer: "/image/build/context", Value: "import-context", Source: config.SourceRef{Kind: "detected", Path: "package.json", Line: 3, Column: 11}},
			{Pointer: "/image/build/file", Value: "Dockerfile", Source: config.SourceRef{Kind: "detected", Path: "package.json", Line: 4, Column: 11}},
			{Pointer: "/image/build/target", Value: "import-target", Source: config.SourceRef{Kind: "detected", Path: "package.json", Line: 5, Column: 13}},
			{Pointer: "/image/build/args/a", Value: "import-a", Source: importSource},
			{Pointer: "/image/build/args/b", Value: "import-b", Source: importSource},
			{Pointer: "/network/internet", Value: true, Source: importSource},
			{Pointer: "/ports", Value: []config.PortConfig{{Name: "forwarded", Guest: 8080, Host: config.HostPort{Dynamic: true}, Protocol: "tcp", Bind: "127.0.0.1"}, {Name: "web", Guest: 9000, Host: config.HostPort{Port: 9001}, Protocol: "udp", Bind: "127.0.0.2"}}, Source: importSource},
			{Pointer: "/resources/cpus", Value: 3, Source: importSource},
			{Pointer: "/resources/memory", Value: "3GiB", Source: importSource},
			{Pointer: "/resources/maxConcurrentClones", Value: 2, Source: importSource},
		},
		Defaults: DefaultValues{ImageRef: "ghcr.io/dsx/default@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Agent: "default-agent", Internet: false, CPUs: 2, MemoryBytes: 2 * 1024 * 1024 * 1024, MaxConcurrentClones: 1},
		Authority: AuthorityInputs{
			BuildContext:          &ContentDigest{Path: "project-context", Digest: strings.Repeat("b", 64)},
			BrowserImageReference: "example/browser@sha256:" + strings.Repeat("c", 64),
			BrowserImageDigest:    strings.Repeat("c", 64),
			ImportedContent:       []ContentDigest{{Path: "package.json", Digest: strings.Repeat("d", 64)}},
		},
	}
}

func namesOfProcesses(processes []ResolvedProcess) []string {
	names := make([]string, len(processes))
	for index := range processes {
		names[index] = processes[index].Name
	}
	return names
}

func namesOfPorts(ports []PortRequest) []string {
	names := make([]string, len(ports))
	for index := range ports {
		names[index] = ports[index].Name
	}
	return names
}

func jsonLeafPointers(value any, pointer string) []string {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var leaves []string
		for _, key := range keys {
			leaves = append(leaves, jsonLeafPointers(typed[key], pointer+"/"+escapePointerToken(key))...)
		}
		return leaves
	case []any:
		var leaves []string
		for index, child := range typed {
			leaves = append(leaves, jsonLeafPointers(child, pointer+"/"+jsonIndex(index))...)
		}
		return leaves
	default:
		return []string{pointer}
	}
}

func jsonIndex(index int) string {
	const digits = "0123456789"
	if index == 0 {
		return "0"
	}
	var reversed [20]byte
	position := len(reversed)
	for index > 0 {
		position--
		reversed[position] = digits[index%10]
		index /= 10
	}
	return string(reversed[position:])
}

func TestResolveManagedStandardImage(t *testing.T) {
	validated, diagnostics := config.ParseBytes("standard.jsonc", []byte(`{
		"schemaVersion": 1,
		"workspace": {"root": "."},
		"image": {"standard": true}
	}`))
	if len(diagnostics) != 0 {
		t.Fatalf("ParseBytes() diagnostics = %#v", diagnostics)
	}
	inputDigest := strings.Repeat("a", 64)
	resolved, diagnostics, err := NewResolver().Resolve(context.Background(), ResolveInput{
		Config:  validated,
		Project: ProjectIdentity{ID: "abcdefghijklmnopqrst", CanonicalRoot: "/project"},
		Sandbox: SandboxIdentity{Name: "main"},
		Mode:    model.ModeLive,
		Defaults: DefaultValues{
			Agent: "codex", Internet: true, CPUs: 2, MemoryBytes: 2 << 30, MaxConcurrentClones: 1,
		},
		Authority: AuthorityInputs{
			StandardImageDigest:   inputDigest,
			BrowserImageReference: "example/browser@sha256:" + strings.Repeat("b", 64),
			BrowserImageDigest:    strings.Repeat("b", 64),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("Resolve() diagnostics = %#v", diagnostics)
	}
	if !resolved.Image.Standard || resolved.Image.Reference != "" ||
		resolved.Image.Context != "@dsx/standard" || resolved.Image.File != "Containerfile" ||
		resolved.Image.InputDigest != inputDigest {
		t.Fatalf("managed standard image = %#v", resolved.Image)
	}
}

func reverse[T any](values []T) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func boolPointer(value bool) *bool       { return &value }
func intPointer(value int) *int          { return &value }
func stringPointer(value string) *string { return &value }
