package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/ownership"
	"github.com/srimajji/dsx/internal/runtime"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type call struct {
	a []string
	o []byte
	e error
	c int
}
type cr struct {
	t *testing.T
	q []call
}

func (r *cr) Run(_ context.Context, x Command) (Result, error) {
	r.t.Helper()
	if len(r.q) == 0 {
		r.t.Fatalf("extra %#v", x.Args)
	}
	w := r.q[0]
	r.q = r.q[1:]
	if x.Executable != testContainerExecutable || !reflect.DeepEqual(x.Args, w.a) {
		r.t.Fatalf("got %#v want %#v", x.Args, w.a)
	}
	if x.Stdout != nil {
		_, _ = x.Stdout.Write(w.o)
	}
	return Result{Stdout: w.o, Stderr: w.o, ExitCode: w.c}, w.e
}
func (r *cr) done() {
	if len(r.q) > 0 {
		r.t.Fatalf("%d calls left", len(r.q))
	}
}
func f(t *testing.T, n string) []byte {
	b, e := os.ReadFile("testdata/" + n)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func ad(t *testing.T, r Runner) *Adapter {
	a, e := NewAdapter(r, testContainerExecutable)
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func oi(t *testing.T, kind runtime.ResourceKind, role string) ownership.Identity {
	root := "/Volumes/Dev/work/tracking-chrome-extension"
	projectID, _ := model.NewProjectID(root)
	workspace, _ := model.ParseWorkspaceName("feature-a")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000001")
	identity, err := ownership.NewIdentity(projectID, root, workspace, runID, kind, role)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
func nf(a []string, noun, name string) call {
	return call{a: a, o: []byte("Error: " + noun + " not found: " + name), e: errors.New("exit 1"), c: 1}
}
func TestBuild(t *testing.T) {
	r := &cr{t: t, q: []call{{a: []string{"build", "--progress", "plain", "--tag", "registry.example/dev:fixed", "--file", "/tmp/c/Containerfile", "--target", "dev", "--build-arg", "A=b", "--label", "x=y", "/tmp/c"}}, {a: []string{"image", "inspect", "registry.example/dev:fixed"}, o: f(t, "image-inspect-1.2.2.json")}}}
	image, e := ad(t, r).EnsureImage(context.Background(), runtime.ImageSpec{Reference: "registry.example/dev:fixed", Context: "/tmp/c", File: "/tmp/c/Containerfile", Target: "dev", BuildArgs: []runtime.Label{{Key: "A", Value: "b"}}, Labels: []runtime.Label{{Key: "x", Value: "y"}}})
	if e != nil {
		t.Fatal(e)
	}
	if image.Reference != "registry.example/dsx/dev:fixed" || image.Digest != "sha256:"+strings.Repeat("a", 64) || !image.Local {
		t.Fatalf("built image = %#v, want inspected local tag and digest", image)
	}
	r.done()
}

func TestBuildCreatesMissingManagedImage(t *testing.T) {
	ref := "dsx.local/standard:abcdef123456"
	input := "abcdef123456abcdef123456abcdef123456abcdef123456abcdef123456abcd"
	label := runtime.Label{Key: "dev.dsx.standard-input", Value: input}
	r := &cr{t: t, q: []call{
		nf([]string{"image", "inspect", ref}, "image", ref),
		{a: []string{"build", "--progress", "plain", "--tag", ref, "--file", "/tmp/c/Containerfile", "--label", label.Key + "=" + label.Value, "/tmp/c"}},
		{a: []string{"image", "inspect", ref}, o: f(t, "image-inspect-managed-1.2.2.json")},
	}}
	image, err := ad(t, r).EnsureImage(context.Background(), runtime.ImageSpec{
		Reference: ref, Context: "/tmp/c", File: "/tmp/c/Containerfile", Labels: []runtime.Label{label}, Reuse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if image.Digest != "sha256:"+strings.Repeat("a", 64) || !image.Local {
		t.Fatalf("built image = %#v", image)
	}
	r.done()
}

func TestBuildReusesExistingManagedImage(t *testing.T) {
	ref := "dsx.local/standard:abcdef123456"
	input := "abcdef123456abcdef123456abcdef123456abcdef123456abcdef123456abcd"
	label := runtime.Label{Key: "dev.dsx.standard-input", Value: input}
	r := &cr{t: t, q: []call{
		{a: []string{"image", "inspect", ref}, o: f(t, "image-inspect-managed-1.2.2.json")},
	}}
	image, err := ad(t, r).EnsureImage(context.Background(), runtime.ImageSpec{
		Reference: ref, Context: "/tmp/c", File: "/tmp/c/Containerfile", Labels: []runtime.Label{label}, Reuse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if image.Digest != "sha256:"+strings.Repeat("a", 64) || !image.Local {
		t.Fatalf("reused image = %#v", image)
	}
	r.done()
}

func TestBuildReplacesManagedImageWithWrongAuthorityLabel(t *testing.T) {
	ref := "dsx.local/standard:abcdef123456"
	input := "abcdef123456abcdef123456abcdef123456abcdef123456abcdef123456abcd"
	label := runtime.Label{Key: "dev.dsx.standard-input", Value: input}
	r := &cr{t: t, q: []call{
		{a: []string{"image", "inspect", ref}, o: f(t, "image-inspect-1.2.2.json")},
		{a: []string{"build", "--progress", "plain", "--tag", ref, "--file", "/tmp/c/Containerfile", "--label", label.Key + "=" + label.Value, "/tmp/c"}},
		{a: []string{"image", "inspect", ref}, o: f(t, "image-inspect-managed-1.2.2.json")},
	}}
	if _, err := ad(t, r).EnsureImage(context.Background(), runtime.ImageSpec{
		Reference: ref, Context: "/tmp/c", File: "/tmp/c/Containerfile", Labels: []runtime.Label{label}, Reuse: true,
	}); err != nil {
		t.Fatal(err)
	}
	r.done()
}
func TestEnsureImage(t *testing.T) {
	ref := "registry.example/dev@sha256:" + strings.Repeat("a", 64)
	r := &cr{t: t, q: []call{
		nf([]string{"image", "inspect", ref}, "image", ref),
		nf([]string{"image", "inspect", "registry.example/dev"}, "image", "registry.example/dev"),
		{a: []string{"image", "pull", "--progress", "plain", ref}},
		{a: []string{"image", "inspect", ref}, o: f(t, "image-inspect-pinned-variant-1.2.2.json")},
	}}
	image, err := ad(t, r).EnsureImage(context.Background(), runtime.ImageSpec{Reference: ref})
	if err != nil {
		t.Fatal(err)
	}
	if image.Reference != ref || image.Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("image = %#v", image)
	}
	r.done()
	unrelated := "registry.example/dev@sha256:" + strings.Repeat("c", 64)
	unrelatedRunner := &cr{t: t, q: []call{
		{a: []string{"image", "inspect", unrelated}, o: f(t, "image-inspect-1.2.2.json")},
	}}
	if _, err := ad(t, unrelatedRunner).EnsureImage(context.Background(), runtime.ImageSpec{Reference: unrelated}); err == nil {
		t.Fatal("accepted unrelated inspected digest")
	}
	unrelatedRunner.done()
	localRef := "registry.example/local:fixed"
	localPinned := localRef + "@sha256:" + strings.Repeat("a", 64)
	localRunner := &cr{t: t, q: []call{
		nf([]string{"image", "inspect", localPinned}, "image", localPinned),
		{a: []string{"image", "inspect", localRef}, o: f(t, "image-inspect-pinned-variant-1.2.2.json")},
	}}
	localImage, err := ad(t, localRunner).EnsureImage(context.Background(), runtime.ImageSpec{Reference: localPinned})
	if err != nil || localImage.Reference != localRef || localImage.Digest != "sha256:"+strings.Repeat("a", 64) || !localImage.Local {
		t.Fatalf("local image = %#v, %v", localImage, err)
	}
	localRunner.done()
	if _, e := ad(t, &cr{t: t}).EnsureImage(context.Background(), runtime.ImageSpec{Reference: "latest"}); e == nil {
		t.Fatal("unpinned accepted")
	}
}
func TestCreate(t *testing.T) {
	i := oi(t, runtime.ResourceWorkspace, "workspace")
	p := uint16(8080)
	a := []string{
		"create", "--name", i.Name(), "--user", "1000", "--workdir", "/workspace",
		"--mount", "type=volume,source=workspace,target=/workspace",
		"--mount", "type=volume,source=cache,target=/cache",
		"--network", "dsx-tracking-chrome-feature-a-network-1abbf9",
		"--publish", "127.0.0.1:8080:3000/tcp",
	}
	a = labelArgs(a, i.Labels())
	a = append(a, "--entrypoint", "/guest", "image:tag")
	r := &cr{t: t, q: []call{
		{a: []string{"image", "inspect", "image:tag"}, o: f(t, "image-inspect-1.2.2.json")},
		{a: a, o: []byte(i.Name() + "\n")},
		{a: []string{"inspect", i.Name()}, o: f(t, "container-inspect-1.2.2.json")},
	}}
	s := runtime.WorkspaceSpec{
		Name: i.Name(), Image: runtime.Image{Reference: "image:tag", Digest: "sha256:" + strings.Repeat("a", 64), Local: true},
		Entrypoint: []string{"/guest"}, WorkingDir: "/workspace", User: "1000",
		Mounts: []runtime.Mount{
			{Source: "workspace", Target: "/workspace", Type: "volume", Authority: runtime.MountAuthorityVolume},
			{Source: "cache", Target: "/cache", Type: "volume", Authority: runtime.MountAuthorityVolume},
		},
		Networks: []string{"dsx-tracking-chrome-feature-a-network-1abbf9"},
		Ports:    []runtime.PortRequest{{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: &p, GuestPort: 3000, Protocol: "tcp"}},
		Labels:   i.Labels(),
	}
	if _, e := ad(t, r).CreateWorkspace(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	r.done()
	mismatch := s
	mismatch.Image.Digest = "sha256:" + strings.Repeat("b", 64)
	mismatchImage := bytes.ReplaceAll(f(t, "image-inspect-1.2.2.json"), []byte("sha256:"+strings.Repeat("a", 64)), []byte(mismatch.Image.Digest))
	mismatchRunner := &cr{t: t, q: []call{
		{a: []string{"image", "inspect", "image:tag"}, o: mismatchImage},
		{a: a, o: []byte(i.Name() + "\n")},
		{a: []string{"inspect", i.Name()}, o: f(t, "container-inspect-1.2.2.json")},
		{a: []string{"inspect", i.Name()}, o: f(t, "container-inspect-1.2.2.json")},
		{a: []string{"delete", i.Name()}},
		nf([]string{"inspect", i.Name()}, "container", i.Name()),
	}}
	if _, e := ad(t, mismatchRunner).CreateWorkspace(context.Background(), mismatch); e == nil {
		t.Fatal("local image digest mismatch accepted")
	}
	mismatchRunner.done()
	for _, bad := range []netip.Addr{netip.MustParseAddr("0.0.0.0")} {
		s.Ports[0].HostIP = bad
		if _, e := ad(t, &cr{t: t}).CreateWorkspace(context.Background(), s); e == nil {
			t.Fatal("nonloopback accepted")
		}
	}
	s.Ports[0].HostIP = netip.MustParseAddr("127.0.0.1")
	s.Ports[0].HostPort = nil
	if _, e := validWorkspace(s); e != nil {
		t.Fatalf("dynamic loopback port rejected: %v", e)
	}
	s.Ports[0].HostPort = &p
	s.User = "root"
	if _, e := ad(t, &cr{t: t}).CreateWorkspace(context.Background(), s); e == nil {
		t.Fatal("root accepted")
	}
}
func TestDynamicPortUsesExactStructuredArgument(t *testing.T) {
	request := runtime.PortRequest{
		HostIP: netip.MustParseAddr("127.0.0.1"), GuestPort: 3000, Protocol: "tcp",
	}
	if got, want := portArg(request), "127.0.0.1::3000/tcp"; got != want {
		t.Fatalf("portArg() = %q, want %q", got, want)
	}
}
func TestCreateAuthLoginUsesProjectScopedIsolatedSpecAndExactArgv(t *testing.T) {
	root := "/Volumes/Dev/work/tracking-chrome-extension"
	projectID, _ := model.NewProjectID(root)
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000001")
	labels, err := runtime.AuthLoginOwnershipLabels(projectID, runID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	name, _ := runtime.CanonicalAuthLoginName(root, "codex")
	digest := "sha256:" + strings.Repeat("a", 64)
	volumeFixture, _ := json.Marshal([]any{map[string]any{
		"id": name, "configuration": map[string]any{"name": name, "labels": labelMap(labels)},
	}})
	containerFixture, _ := json.Marshal([]any{map[string]any{
		"configuration": map[string]any{
			"id": name, "labels": labelMap(labels),
			"image":    map[string]any{"descriptor": map[string]any{"digest": digest}},
			"mounts":   []any{map[string]any{"type": "volume", "source": name, "destination": "/auth", "options": []string{}}},
			"networks": []any{}, "publishedPorts": []any{},
		},
		"status": map[string]any{"state": "stopped", "networks": []any{}},
	}})
	volumeArgs := labelArgs([]string{"volume", "create"}, labels)
	volumeArgs = append(volumeArgs, name)
	spec := runtime.AuthLoginSpec{
		Name: name, CanonicalRoot: root, Harness: "codex",
		Image:      runtime.Image{Reference: "image:test@" + digest, Digest: digest},
		Entrypoint: []string{"/usr/bin/codex", "login"}, Env: []string{"TERM=xterm"},
		WorkingDir: "/tmp", User: "1000:1000",
		AuthVolume: runtime.Mount{Source: name, Target: "/auth", Type: "volume", Authority: runtime.MountAuthorityVolume},
		Labels:     labels,
	}
	createArgs := []string{"create", "--name", name, "--user", "1000:1000", "--workdir", "/tmp", "--env", "TERM=xterm", "--mount", "type=volume,source=" + name + ",target=/auth"}
	createArgs = labelArgs(createArgs, labels)
	createArgs = append(createArgs, "--entrypoint", "/usr/bin/codex", "image:test@"+digest, "login")
	runner := &cr{t: t, q: []call{
		{a: volumeArgs, o: []byte(name + "\n")},
		{a: []string{"volume", "inspect", name}, o: volumeFixture},
		{a: createArgs, o: []byte(name + "\n")},
		{a: []string{"inspect", name}, o: containerFixture},
	}}
	adapter := ad(t, runner)
	if _, err := adapter.CreateAuthLoginVolume(context.Background(), runtime.AuthLoginVolumeSpec{Name: name, CanonicalRoot: root, Harness: "codex", Labels: labels}); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.CreateAuthLogin(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	runner.done()

	hostMount := spec
	hostMount.AuthVolume = runtime.Mount{Source: "/Users/developer", Target: "/auth", Type: "bind", Authority: runtime.MountAuthorityVolume}
	if err := validAuthLogin(hostMount); err == nil {
		t.Fatal("auth login accepted host source/home mount")
	}
}

func TestCreateBrowserUsesExactIsolatedArgv(t *testing.T) {
	spec := browserSpec(t)
	args := []string{
		"create", "--name", spec.Name,
		"--env", "PLAYWRIGHT_BROWSERS_PATH=/ms-playwright",
		"--network", spec.Networks[0],
	}
	args = labelArgs(args, spec.Labels)
	args = append(args,
		"--cpus", "2",
		"--memory", "1073741824",
		"--entrypoint", "/usr/bin/node",
		"browser:test@sha256:"+strings.Repeat("a", 64),
		"/srv/mcp.js", "--port", "8931",
	)
	runner := &cr{t: t, q: []call{
		{a: args, o: []byte(spec.Name + "\n")},
		{a: []string{"inspect", spec.Name}, o: f(t, "browser-inspect-1.2.2.json")},
	}}
	resource, err := ad(t, runner).CreateBrowser(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if resource.ID != runtime.ResourceID(spec.Name) || resource.Name != spec.Name || resource.Kind != runtime.ResourceBrowser {
		t.Fatalf("CreateBrowser() = %#v", resource)
	}
	runner.done()
}

func TestCreateBrowserVerifiesLocalImageBeforeCreate(t *testing.T) {
	spec := browserSpec(t)
	spec.Image.Local = true
	runner := &cr{t: t, q: []call{
		{a: []string{"image", "inspect", spec.Image.Reference}, o: f(t, "image-inspect-1.2.2.json")},
		{a: browserCreateArgs(spec), o: []byte(spec.Name + "\n")},
		{a: []string{"inspect", spec.Name}, o: f(t, "browser-inspect-1.2.2.json")},
	}}
	if _, err := ad(t, runner).CreateBrowser(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	runner.done()
}

func TestCreateBrowserRejectsInvalidSpecification(t *testing.T) {
	tests := []struct {
		name   string
		change func(*runtime.BrowserSpec)
	}{
		{"no network", func(spec *runtime.BrowserSpec) { spec.Networks = nil }},
		{"two networks", func(spec *runtime.BrowserSpec) { spec.Networks = append(spec.Networks, "second") }},
		{"empty name", func(spec *runtime.BrowserSpec) { spec.Name = "" }},
		{"empty image reference", func(spec *runtime.BrowserSpec) { spec.Image.Reference = "" }},
		{"empty image digest", func(spec *runtime.BrowserSpec) { spec.Image.Digest = "" }},
		{"mismatched pinned image", func(spec *runtime.BrowserSpec) {
			spec.Image.Reference = "browser:test@sha256:" + strings.Repeat("b", 64)
		}},
		{"pinned local reference", func(spec *runtime.BrowserSpec) {
			spec.Image.Local = true
			spec.Image.Reference += "@sha256:" + strings.Repeat("a", 64)
		}},
		{"empty entrypoint", func(spec *runtime.BrowserSpec) { spec.Entrypoint = nil }},
		{"empty entrypoint value", func(spec *runtime.BrowserSpec) { spec.Entrypoint[1] = "" }},
		{"too many entrypoint values", func(spec *runtime.BrowserSpec) {
			spec.Entrypoint = make([]string, maxItems+1)
			for index := range spec.Entrypoint {
				spec.Entrypoint[index] = "argument"
			}
		}},
		{"NUL entrypoint value", func(spec *runtime.BrowserSpec) { spec.Entrypoint[1] = "bad\x00value" }},
		{"invalid environment", func(spec *runtime.BrowserSpec) { spec.Env = []string{""} }},
		{"duplicate environment", func(spec *runtime.BrowserSpec) { spec.Env = []string{"A=1", "A=2"} }},
		{"too much environment", func(spec *runtime.BrowserSpec) {
			spec.Env = make([]string, maxItems+1)
			for index := range spec.Env {
				spec.Env[index] = "A" + strconv.Itoa(index) + "=value"
			}
		}},
		{"empty network", func(spec *runtime.BrowserSpec) { spec.Networks[0] = "" }},
		{"negative CPU", func(spec *runtime.BrowserSpec) { spec.CPUs = -1 }},
		{"excessive CPU", func(spec *runtime.BrowserSpec) { spec.CPUs = maxCPUs + 1 }},
		{"negative memory", func(spec *runtime.BrowserSpec) { spec.MemoryBytes = -1 }},
		{"too little memory", func(spec *runtime.BrowserSpec) { spec.MemoryBytes = minMemoryBytes - 1 }},
		{"excessive memory", func(spec *runtime.BrowserSpec) { spec.MemoryBytes = maxMemoryBytes + 1 }},
		{"wrong labels", func(spec *runtime.BrowserSpec) { spec.Labels = oi(t, runtime.ResourceWorkspace, "workspace").Labels() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := browserSpec(t)
			test.change(&spec)
			runner := &cr{t: t}
			if _, err := ad(t, runner).CreateBrowser(context.Background(), spec); err == nil || model.ErrorCodeOf(err) != model.CodeInvalidInput {
				t.Fatalf("CreateBrowser() error = %v", err)
			}
			runner.done()
		})
	}
}

func TestCreateBrowserEnforcesInspectPostconditions(t *testing.T) {
	tests := []struct {
		name    string
		inspect func(*testing.T) []byte
	}{
		{"mount", func(t *testing.T) []byte {
			return replaceFixture(t, f(t, "browser-inspect-1.2.2.json"), `"mounts": []`, `"mounts": [{"type":"virtiofs","source":"/host","destination":"/workspace","options":[]}]`)
		}},
		{"published port", func(t *testing.T) []byte {
			return replaceFixture(t, f(t, "browser-inspect-1.2.2.json"), `"publishedPorts": []`, `"publishedPorts": [{"hostAddress":"127.0.0.1","hostPort":8931,"containerPort":8931,"proto":"tcp","count":1}]`)
		}},
		{"network", func(t *testing.T) []byte {
			return bytes.ReplaceAll(f(t, "browser-inspect-1.2.2.json"), []byte("dsx-tracking-chrome-feature-a-network-1abbf9"), []byte("foreign-network"))
		}},
		{"image", func(t *testing.T) []byte {
			return replaceFixture(t, f(t, "browser-inspect-1.2.2.json"), `"digest": "sha256:`+strings.Repeat("a", 64)+`"`, `"digest": "sha256:`+strings.Repeat("b", 64)+`"`)
		}},
		{"labels", func(t *testing.T) []byte {
			return replaceFixture(t, f(t, "browser-inspect-1.2.2.json"), `"dev.dsx.role": "browser"`, `"dev.dsx.role": "other"`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := browserSpec(t)
			args := browserCreateArgs(spec)
			runner := &cr{t: t, q: []call{
				{a: args, o: []byte(spec.Name + "\n")},
				{a: []string{"inspect", spec.Name}, o: test.inspect(t)},
			}}
			if _, err := ad(t, runner).CreateBrowser(context.Background(), spec); err == nil || model.ErrorCodeOf(err) != model.CodeUnavailable {
				t.Fatalf("CreateBrowser() error = %v", err)
			}
			runner.done()
		})
	}
}

func TestDecodeContainerNetworkAddresses(t *testing.T) {
	snapshots, err := decodeContainers(f(t, "browser-inspect-1.2.2.json"))
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("decodeContainers() count=%d error=%v", len(snapshots), err)
	}
	network := "dsx-tracking-chrome-feature-a-network-1abbf9"
	want := map[string][]netip.Addr{
		network: {
			netip.MustParseAddr("192.168.64.5"),
			netip.MustParseAddr("fdb1:370c:1cb4:c412::5"),
		},
	}
	if !reflect.DeepEqual(snapshots[0].NetworkAddresses, want) {
		t.Fatalf("NetworkAddresses = %#v, want %#v", snapshots[0].NetworkAddresses, want)
	}

	tests := []struct {
		name    string
		inspect func(*testing.T) []byte
	}{
		{"invalid prefix", func(t *testing.T) []byte {
			return replaceFixture(t, f(t, "browser-inspect-1.2.2.json"), `"ipv4Address":"192.168.64.5/24"`, `"ipv4Address":"not-an-address"`)
		}},
		{"wrong family", func(t *testing.T) []byte {
			return replaceFixture(t, f(t, "browser-inspect-1.2.2.json"), `"ipv4Address":"192.168.64.5/24"`, `"ipv4Address":"fdb1:370c:1cb4:c412::5/64"`)
		}},
		{"unknown status network", func(t *testing.T) []byte {
			return replaceFixture(t, f(t, "browser-inspect-1.2.2.json"), `"network":"dsx-tracking-chrome-feature-a-network-1abbf9",`+"\n"+`        "variant"`, `"network":"foreign-network",`+"\n"+`        "variant"`)
		}},
		{"duplicate status network", ambiguousBrowserInspect},
		{"duplicate address across networks", duplicateNetworkAddressInspect},
		{"duplicate configured network", func(t *testing.T) []byte {
			return replaceFixture(t, f(t, "browser-inspect-1.2.2.json"), `[{"network":"dsx-tracking-chrome-feature-a-network-1abbf9","options":{"hostname":"browser"}}]`, `[{"network":"dsx-tracking-chrome-feature-a-network-1abbf9"},{"network":"dsx-tracking-chrome-feature-a-network-1abbf9"}]`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeContainers(test.inspect(t)); err == nil {
				t.Fatal("decodeContainers() accepted invalid network address evidence")
			}
		})
	}
}

func TestCreateNamed(t *testing.T) {
	for _, x := range []struct {
		k runtime.ResourceKind
		n string
	}{{runtime.ResourceNetwork, "network"}, {runtime.ResourceVolume, "volume"}} {
		i := oi(t, x.k, x.n)
		a := labelArgs([]string{x.n, "create"}, i.Labels())
		a = append(a, i.Name())
		r := &cr{t: t, q: []call{{a: a, o: []byte(i.Name() + "\n")}, {a: []string{x.n, "inspect", i.Name()}, o: f(t, x.n+"-inspect-1.2.2.json")}}}
		var e error
		if x.k == runtime.ResourceNetwork {
			_, e = ad(t, r).CreateNetwork(context.Background(), runtime.NetworkSpec{Name: i.Name(), Labels: i.Labels()})
		} else {
			_, e = ad(t, r).CreateVolume(context.Background(), runtime.VolumeSpec{Name: i.Name(), Labels: i.Labels()})
		}
		if e != nil {
			t.Fatal(e)
		}
		r.done()
	}
}
func TestStart(t *testing.T) {
	expected := expectedWorkspace(t)
	id := string(expected.ID)
	r := &cr{t: t, q: []call{
		{a: []string{"inspect", id}, o: f(t, "container-inspect-1.2.2.json")},
		{a: []string{"start", id}},
		{a: []string{"inspect", id}, o: f(t, "container-inspect-1.2.2.json")},
	}}
	if e := ad(t, r).StartWorkspace(context.Background(), expected); e != nil {
		t.Fatal(e)
	}
	r.done()
}
func TestStartBrowserRequiresOwnerNetworkPrivateIPv4(t *testing.T) {
	expected := expectedBrowser(t)
	id := string(expected.ID)
	started := replaceFixture(t, f(t, "browser-inspect-1.2.2.json"), `"state":"stopped"`, `"state":"running"`)
	tests := []struct {
		name    string
		inspect []byte
		ok      bool
	}{
		{"private IPv4", started, true},
		{"missing private IPv4", replaceFixture(t, started, `"ipv4Address":"192.168.64.5/24"`, `"ipv4Address":""`), false},
		{"public IPv4", replaceFixture(t, started, `"ipv4Address":"192.168.64.5/24"`, `"ipv4Address":"203.0.113.5/24"`), false},
		{"ambiguous network address", replaceFixture(t, ambiguousBrowserInspect(t), `"state":"stopped"`, `"state":"running"`), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &cr{t: t, q: []call{
				{a: []string{"inspect", id}, o: f(t, "browser-inspect-1.2.2.json")},
				{a: []string{"start", id}},
				{a: []string{"inspect", id}, o: test.inspect},
			}}
			err := ad(t, runner).StartWorkspace(context.Background(), expected)
			if test.ok && err != nil {
				t.Fatal(err)
			}
			if !test.ok && (err == nil || model.ErrorCodeOf(err) != model.CodeUnavailable) {
				t.Fatalf("StartWorkspace() error = %v", err)
			}
			runner.done()
		})
	}
}
func TestExec(t *testing.T) {
	expected := expectedWorkspace(t)
	id := string(expected.ID)
	r := &cr{t: t, q: []call{
		{a: []string{"inspect", id}, o: f(t, "container-inspect-1.2.2.json")},
		{a: []string{"exec", "--interactive", "--tty", "--env", "A=b", "--workdir", "/workspace", "--user", "1000", id, "echo", "a b"}, o: []byte("out"), e: errors.New("exit"), c: 7},
	}}
	var b bytes.Buffer
	x, e := ad(t, r).Exec(context.Background(), expected, runtime.ExecSpec{Argv: []string{"echo", "a b"}, Env: []string{"A=b"}, WorkingDir: "/workspace", User: "1000", Terminal: true}, runtime.ExecIO{Stdin: strings.NewReader("x"), Stdout: &b})
	if e != nil || x.Code == nil || *x.Code != 7 || b.String() != "out" {
		t.Fatalf("%#v %q %v", x, b.String(), e)
	}
	r.done()
}

func TestPrepareExecReturnsValidatedShellFreeProcess(t *testing.T) {
	expected := expectedWorkspace(t)
	id := string(expected.ID)
	runner := &cr{t: t, q: []call{{a: []string{"inspect", id}, o: f(t, "container-inspect-1.2.2.json")}}}
	process, err := ad(t, runner).PrepareExec(context.Background(), expected, runtime.ExecSpec{
		Argv:       []string{"printf", "%s", "a b"},
		WorkingDir: "/workspace",
		User:       "1000:1000",
		Terminal:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--interactive", "--tty", "--workdir", "/workspace", "--user", "1000:1000", id, "printf", "%s", "a b"}
	if process.Executable != "/opt/test/bin/container" || !reflect.DeepEqual(process.Args, want) || !reflect.DeepEqual(process.Env, probeEnvironment) {
		t.Fatalf("PrepareExec() = %#v, want argv %#v", process, want)
	}
	runner.done()
}

func TestPrepareExecKeepsStagedSecretsOutOfHostArgvAndEnvironment(t *testing.T) {
	expected := expectedWorkspace(t)
	id := string(expected.ID)
	runner := &cr{t: t, q: []call{{a: []string{"inspect", id}, o: f(t, "container-inspect-1.2.2.json")}}}
	environmentFile := "/tmp/dsx-run/00000000-0000-7000-8000-000000000000/env-00000000000000000000000000000000"
	process, err := ad(t, runner).PrepareExec(context.Background(), expected, runtime.ExecSpec{
		Argv:       []string{"/usr/local/libexec/dsx/dsx-guest", "exec", "--env-file", environmentFile, "--", "/usr/local/bin/opencode"},
		Env:        []string{"TERM=xterm-256color"},
		WorkingDir: "/workspace",
		User:       "1000:1000",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--env", "TERM=xterm-256color", "--workdir", "/workspace", "--user", "1000:1000", id, "/usr/local/libexec/dsx/dsx-guest", "exec", "--env-file", environmentFile, "--", "/usr/local/bin/opencode"}
	if !reflect.DeepEqual(process.Args, want) || !reflect.DeepEqual(process.Env, probeEnvironment) {
		t.Fatalf("PrepareExec() = %#v, want argv %#v", process, want)
	}
	secret := `{"headers":{"Authorization":"Bearer must-not-leak"}}`
	if strings.Contains(strings.Join(process.Args, "\x00"), secret) || strings.Contains(strings.Join(process.Env, "\x00"), secret) {
		t.Fatalf("secret entered host process: %#v", process)
	}
	runner.done()
}
func TestCopy(t *testing.T) {
	expected := expectedWorkspace(t)
	id := string(expected.ID)
	r := &cr{t: t, q: []call{
		{a: []string{"inspect", id}, o: f(t, "container-inspect-1.2.2.json")},
		{a: []string{"copy", "/tmp/a", id + ":/a"}},
		{a: []string{"inspect", id}, o: f(t, "container-inspect-1.2.2.json")},
		{a: []string{"copy", id + ":/b", "/tmp/b"}},
	}}
	a := ad(t, r)
	if e := a.CopyTo(context.Background(), expected, "/tmp/a", "/a"); e != nil {
		t.Fatal(e)
	}
	if e := a.CopyFrom(context.Background(), expected, "/b", "/tmp/b"); e != nil {
		t.Fatal(e)
	}
	r.done()
}
func TestInspect(t *testing.T) {
	id := "dsx-tracking-chrome-feature-a-workspace-1abbf9"
	r := &cr{t: t, q: []call{{a: []string{"inspect", id}, o: f(t, "container-inspect-1.2.2.json")}}}
	s, e := ad(t, r).Inspect(context.Background(), runtime.ResourceID(id))
	if e != nil || len(s.Mounts) != 2 || len(s.Networks) != 1 || len(s.Ports) != 1 || len(s.Labels) != 7 || len(s.NetworkAddresses[s.Networks[0]]) != 2 {
		t.Fatalf("%#v %v", s, e)
	}
	r.done()
}
func TestList(t *testing.T) {
	r := &cr{t: t, q: []call{{a: []string{"list", "--all", "--format", "json"}, o: f(t, "container-inspect-1.2.2.json")}, {a: []string{"network", "list", "--format", "json"}, o: f(t, "network-inspect-1.2.2.json")}, {a: []string{"volume", "list", "--format", "json"}, o: f(t, "volume-inspect-1.2.2.json")}}}
	a := ad(t, r)
	for _, k := range []runtime.ResourceKind{runtime.ResourceWorkspace, runtime.ResourceNetwork, runtime.ResourceVolume} {
		v, e := a.List(context.Background(), k)
		if e != nil || len(v) != 1 {
			t.Fatalf("%s %#v %v", k, v, e)
		}
	}
	r.done()
}
func TestSignal(t *testing.T) {
	expected := expectedWorkspace(t)
	id := string(expected.ID)
	r := &cr{t: t, q: []call{
		{a: []string{"inspect", id}, o: f(t, "container-inspect-1.2.2.json")},
		{a: []string{"kill", "--signal", "TERM", id}},
	}}
	if e := ad(t, r).Signal(context.Background(), expected, "SIGTERM"); e != nil {
		t.Fatal(e)
	}
	r.done()
}
func TestStop(t *testing.T) {
	expected := expectedWorkspace(t)
	id := string(expected.ID)
	stopped := bytes.Replace(f(t, "container-inspect-1.2.2.json"), []byte(`"state":"running"`), []byte(`"state":"stopped"`), 1)
	r := &cr{t: t, q: []call{
		{a: []string{"inspect", id}, o: f(t, "container-inspect-1.2.2.json")},
		{a: []string{"stop", "--signal", "TERM", "--time", "10", id}},
		{a: []string{"inspect", id}, o: stopped},
	}}
	if e := ad(t, r).Stop(context.Background(), expected, runtime.StopPolicy{TimeoutSeconds: 10, Signal: "TERM"}); e != nil {
		t.Fatal(e)
	}
	r.done()
}
func TestDelete(t *testing.T) {
	identity := oi(t, runtime.ResourceWorkspace, "workspace")
	resource := runtime.Resource{ID: runtime.ResourceID(identity.Name()), Name: identity.Name(), Kind: runtime.ResourceWorkspace}
	expected := runtime.ResourceSnapshot{Resource: resource, Labels: identity.Labels()}
	r := &cr{t: t, q: []call{{a: []string{"inspect", resource.Name}, o: f(t, "container-inspect-1.2.2.json")}, {a: []string{"delete", resource.Name}}, nf([]string{"inspect", resource.Name}, "container", resource.Name), nf([]string{"inspect", resource.Name}, "container", resource.Name)}}
	a := ad(t, r)
	if e := a.Delete(context.Background(), expected); e != nil {
		t.Fatal(e)
	}
	if e := a.Delete(context.Background(), expected); e != nil {
		t.Fatal(e)
	}
	r.done()
	for _, invalid := range []runtime.Resource{{ID: "buildkit", Name: "buildkit", Kind: runtime.ResourceWorkspace}, {ID: "x", Name: "x", Kind: "builder"}, {}} {
		if e := ad(t, &cr{t: t}).Delete(context.Background(), runtime.ResourceSnapshot{Resource: invalid, Labels: identity.Labels()}); e == nil {
			t.Fatalf("accepted %#v", invalid)
		}
	}
	mismatched := expected
	mismatched.Labels = append([]runtime.Label(nil), expected.Labels...)
	mismatched.Labels[2].Value = "bbbbbbbbbbbbbbbbbbbb"
	mismatchRunner := &cr{t: t, q: []call{{a: []string{"inspect", resource.Name}, o: f(t, "container-inspect-1.2.2.json")}}}
	if e := ad(t, mismatchRunner).Delete(context.Background(), mismatched); e == nil {
		t.Fatal("deleted resource after ownership labels changed")
	}
	mismatchRunner.done()
}
func TestLegacyWorkspaceIsCleanupOnlyAtRuntimeBoundary(t *testing.T) {
	fixture, legacy := legacyWorkspaceFixture(t)
	if err := ad(t, &cr{t: t}).StartWorkspace(context.Background(), legacy); err == nil || model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("StartWorkspace(legacy) error = %v", err)
	}
	id := string(legacy.ID)
	runner := &cr{t: t, q: []call{
		{a: []string{"inspect", id}, o: fixture},
		{a: []string{"delete", id}},
		nf([]string{"inspect", id}, "container", id),
	}}
	if err := ad(t, runner).Delete(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	runner.done()
}

func TestListRetainsUnlabelledDSXNamesAsAmbiguousCleanupCandidates(t *testing.T) {
	fixture := []byte(`[
		{"configuration":{"id":"dsx-legacy-name","labels":{}},"status":{"state":"stopped"}},
		{"configuration":{"id":"foreign-name","labels":{}},"status":{"state":"stopped"}}
	]`)
	runner := &cr{t: t, q: []call{{a: []string{"list", "--all", "--format", "json"}, o: fixture}}}
	resources, err := ad(t, runner).List(context.Background(), runtime.ResourceWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0].Name != "dsx-legacy-name" || resources[0].Kind != "" {
		t.Fatalf("cleanup candidates = %#v", resources)
	}
	runner.done()
}

func TestCreateRejectsHostileLabelPathAndPostcondition(t *testing.T) {
	i := oi(t, runtime.ResourceWorkspace, "workspace")
	a := ad(t, &cr{t: t})
	if _, err := a.CreateVolume(context.Background(), runtime.VolumeSpec{
		Name: i.Name(), Labels: []runtime.Label{{Key: "dev.dsx.managed\n", Value: "true"}},
	}); err == nil {
		t.Fatal("accepted hostile label")
	}
	spec := runtime.WorkspaceSpec{
		Name: i.Name(), Image: runtime.Image{Reference: "image:tag", Digest: "sha256:" + strings.Repeat("a", 64)},
		Entrypoint: []string{"/bin/true"}, WorkingDir: "/workspace", User: "1000", Labels: i.Labels(),
		Mounts: []runtime.Mount{{Source: "/tmp/../etc", Target: "/workspace", Type: "bind", Authority: runtime.MountAuthorityVolume}},
	}
	if _, err := a.CreateWorkspace(context.Background(), spec); err == nil {
		t.Fatal("accepted unclean host path")
	}
	if _, err := a.afterCreate(context.Background(), Result{Stdout: []byte("wrong\n")}, i.Name(), runtime.ResourceWorkspace, i.Labels()); err == nil {
		t.Fatal("accepted mismatched create output")
	}
}

func TestInspectDoesNotTreatOperationalNotFoundTextAsAbsence(t *testing.T) {

	id := runtime.ResourceID("dsx-tracking-chrome-feature-a-workspace-1abbf9")
	runner := &cr{t: t, q: []call{{a: []string{"inspect", string(id)}, o: []byte("Error: runtime socket not found"), e: errors.New("exit 1"), c: 1}}}
	_, err := ad(t, runner).Inspect(context.Background(), id)
	if err == nil || errors.Is(err, runtime.ErrResourceNotFound) || model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatalf("Inspect() error = %v", err)
	}
	runner.done()
}
func TestValidBuildAllowsSeparatelyStagedDockerfileOutsideContext(t *testing.T) {
	spec := runtime.ImageSpec{
		Reference: "registry.example/dev:staged",
		Context:   runtime.HostPath("/private/tmp/dsx-stage/services/api"),
		File:      runtime.HostPath("/private/tmp/dsx-stage/Dockerfile"),
	}
	if err := validBuild(spec); err != nil {
		t.Fatalf("validBuild() = %v", err)
	}
}
func TestExecAllowsOnlyExactRootReadOnlyConfigStaging(t *testing.T) {
	spec := runtime.ExecSpec{
		Argv: []string{
			"/usr/local/libexec/dsx/dsx-guest", "stage-file", "--read-only",
			"--child-uid", "1000", "--child-gid", "1000", "--path",
			"/tmp/dsx-readonly/00000000-0000-7000-8000-000000000000/settings.json",
		},
		WorkingDir: "/workspace",
		User:       "0:0",
	}
	if err := validExec(spec); err != nil {
		t.Fatalf("exact root read-only staging rejected: %v", err)
	}
	mutations := []runtime.ExecSpec{
		{Argv: append([]string(nil), spec.Argv...), WorkingDir: spec.WorkingDir, User: spec.User, Env: []string{"SECRET=value"}},
		{Argv: []string{"/bin/sh", "-c", "true"}, WorkingDir: spec.WorkingDir, User: spec.User},
		{Argv: append([]string(nil), spec.Argv...), WorkingDir: spec.WorkingDir, User: spec.User},
	}
	mutations[2].Argv[len(mutations[2].Argv)-1] = "/tmp/dsx-run/00000000-0000-7000-8000-000000000000/auth/settings.json"
	for index, mutation := range mutations {
		if err := validExec(mutation); err == nil {
			t.Errorf("root staging mutation %d was accepted: %#v", index, mutation)
		}
	}
}

func TestExecAllowsOnlyExactRootReadOnlyCleanup(t *testing.T) {
	spec := runtime.ExecSpec{
		Argv:       []string{"/usr/local/libexec/dsx/dsx-guest", "remove-read-only", "--path", "/tmp/dsx-readonly/00000000-0000-7000-8000-000000000000"},
		WorkingDir: "/workspace",
		User:       "0:0",
	}
	if err := validExec(spec); err != nil {
		t.Fatalf("exact root cleanup rejected: %v", err)
	}
	spec.Argv[len(spec.Argv)-1] += "/nested"
	if err := validExec(spec); err == nil {
		t.Fatal("nested root cleanup was accepted")
	}
}

func TestWorkspaceAllowsOnlyPinnedRootGuestSupervisor(t *testing.T) {
	helperRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(helperRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(helperRoot, "dsx-guest"), []byte("helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity := oi(t, runtime.ResourceWorkspace, "workspace")
	spec := runtime.WorkspaceSpec{
		Name: identity.Name(), Image: runtime.Image{Reference: "image:tag", Digest: "sha256:" + strings.Repeat("a", 64)},
		Entrypoint: []string{"/bin/true"}, WorkingDir: "/workspace", User: "0:0", Labels: identity.Labels(),
		Networks: []string{oi(t, runtime.ResourceNetwork, "network").Name()},
	}
	spec.User = "0:0"
	if _, err := validWorkspace(spec); err == nil {
		t.Fatal("arbitrary root workspace was accepted")
	}
	spec.Entrypoint = []string{
		"/usr/local/libexec/dsx/dsx-guest", "serve",
		"--socket", "/run/dsx/control.sock",
		"--child-uid", "501", "--child-gid", "20",
	}
	spec.Mounts = []runtime.Mount{
		{Source: helperRoot, Target: "/usr/local/libexec/dsx", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityGuestHelper},
		{Source: "dsx-owned-workspace", Target: "/workspace", Type: "volume", Authority: runtime.MountAuthorityVolume},
	}
	if _, err := validWorkspace(spec); err != nil {
		t.Fatalf("pinned root supervisor rejected: %v", err)
	}
	if argument, err := mountArg(spec.Mounts[0]); err != nil ||
		argument != "type=bind,source="+helperRoot+",target=/usr/local/libexec/dsx,readonly" {
		t.Fatalf("guest helper serialization = %q, %v", argument, err)
	}
	spec.Entrypoint = append(spec.Entrypoint, "--initialize-workspace", "/workspace")
	if _, err := validWorkspace(spec); err != nil {
		t.Fatalf("owned-volume root supervisor rejected: %v", err)
	}
	spec.Mounts[len(spec.Mounts)-1] = runtime.Mount{Source: "/tmp/project", Target: "/workspace", Type: "bind", Authority: runtime.MountAuthorityVolume}
	if _, err := validWorkspace(spec); err == nil {
		t.Fatal("host-mounted workspace initialization was accepted")
	}
	spec.Mounts[len(spec.Mounts)-1] = runtime.Mount{Source: "dsx-owned-workspace", Target: "/workspace", Type: "volume", Authority: runtime.MountAuthorityVolume}
	spec.Entrypoint = spec.Entrypoint[:8]
	spec.Mounts = append(spec.Mounts, runtime.Mount{Source: "/tmp/hostile", Target: "/usr/local/libexec/dsx/dsx-guest", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityVolume})
	if _, err := validWorkspace(spec); err == nil {
		t.Fatal("nested helper replacement mount was accepted")
	}

	spec.Mounts = spec.Mounts[:2]
	spec.Entrypoint[2] = "--other"
	if _, err := validWorkspace(spec); err == nil {
		t.Fatal("modified root supervisor authority was accepted")
	}
}
func TestWorkspaceHostAWSMirrorMountRequiresExactCapability(t *testing.T) {
	helperRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(helperRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(helperRoot, "dsx-guest"), []byte("helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity := oi(t, runtime.ResourceWorkspace, "workspace")
	spec := runtime.WorkspaceSpec{
		Name: identity.Name(), Image: runtime.Image{Reference: "image:tag", Digest: "sha256:" + strings.Repeat("a", 64)},
		Entrypoint: []string{
			"/usr/local/libexec/dsx/dsx-guest", "serve",
			"--socket", "/run/dsx/control.sock",
			"--child-uid", "501", "--child-gid", "20",
			"--initialize-workspace", "/workspace",
		},
		WorkingDir: "/workspace", User: "0:0", Labels: identity.Labels(),
		Networks: []string{oi(t, runtime.ResourceNetwork, "network").Name()},
		Mounts: []runtime.Mount{
			{Source: helperRoot, Target: "/usr/local/libexec/dsx", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityGuestHelper},
			{Source: "dsx-owned-workspace", Target: "/workspace", Type: "volume", Authority: runtime.MountAuthorityVolume},
		},
	}
	if _, err := validWorkspace(spec); err != nil {
		t.Fatalf("AWS-none workspace rejected: %v", err)
	}
	for _, mount := range spec.Mounts {
		if mount.Authority == runtime.MountAuthorityHostAWSMirror {
			t.Fatal("AWS-none workspace received a host AWS publication mount")
		}
	}

	stateRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stableChannel := filepath.Join(stateRoot, "host-aws-workspaces", "aaaaaaaaaaaaaaaaaaaa", "main", "01890f5c-7b00-7000-8000-000000000001", "publication")
	if err := os.MkdirAll(stableChannel, 0o700); err != nil {
		t.Fatal(err)
	}
	spec.HostAWSMirrorSource = runtime.HostPath(stableChannel)
	spec.Mounts = append(spec.Mounts, runtime.Mount{
		Source: stableChannel, Target: "/run/dsx/aws", Type: "bind", ReadOnly: true,
		Authority: runtime.MountAuthorityHostAWSMirror,
	})
	if _, err := validWorkspace(spec); err != nil {
		t.Fatalf("disabled host-default workspace rejected: %v", err)
	}
	if len(spec.Env) != 0 {
		t.Fatalf("disabled host-default workspace received AWS environment: %#v", spec.Env)
	}
	if argument, err := mountArg(spec.Mounts[len(spec.Mounts)-1]); err != nil ||
		argument != "type=bind,source="+stableChannel+",target=/run/dsx/aws,readonly" {
		t.Fatalf("host AWS publication serialization = %q, %v", argument, err)
	}

	missing := spec
	missing.Mounts = missing.Mounts[:len(missing.Mounts)-1]
	if _, err := validWorkspace(missing); err == nil {
		t.Fatal("host-default capability without its exact publication mount was accepted")
	}
	ungranted := spec
	ungranted.HostAWSMirrorSource = ""
	if _, err := validWorkspace(ungranted); err == nil {
		t.Fatal("host AWS publication mount without a workspace capability was accepted")
	}
	replaced := spec
	replaced.Mounts = append([]runtime.Mount(nil), spec.Mounts...)
	replaced.Mounts[len(replaced.Mounts)-1].Source = filepath.Join(stateRoot, "host-source")
	if _, err := validWorkspace(replaced); err == nil {
		t.Fatal("host AWS source replacement was accepted")
	}
	for name, mutate := range map[string]func(*runtime.Mount){
		"writable":     func(mount *runtime.Mount) { mount.ReadOnly = false },
		"wrong target": func(mount *runtime.Mount) { mount.Target = "/run/dsx/aws-other" },
		"wrong type":   func(mount *runtime.Mount) { mount.Type = "volume" },
	} {
		mutated := spec
		mutated.Mounts = append([]runtime.Mount(nil), spec.Mounts...)
		mutate(&mutated.Mounts[len(mutated.Mounts)-1])
		if _, err := validWorkspace(mutated); err == nil {
			t.Errorf("%s host AWS publication mount was accepted", name)
		}
	}
}

func TestWorkspaceAllowsOnlyNarrowReviewedHostMountOutsideSourceAndHome(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Join(base, "project")
	reviewedSource := filepath.Join(base, "reviewed")
	if err := os.MkdirAll(projectRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(reviewedSource, 0700); err != nil {
		t.Fatal(err)
	}
	projectID, _ := model.NewProjectID(projectRoot)
	workspace, _ := model.ParseWorkspaceName("feature-a")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000001")
	workspaceOwner, _ := ownership.NewIdentity(projectID, projectRoot, workspace, runID, runtime.ResourceWorkspace, "workspace")
	networkOwner, _ := ownership.NewIdentity(projectID, projectRoot, workspace, runID, runtime.ResourceNetwork, "network")
	volumeOwner, _ := ownership.NewIdentity(projectID, projectRoot, workspace, runID, runtime.ResourceVolume, "source")
	spec := runtime.WorkspaceSpec{
		Name: workspaceOwner.Name(), CanonicalRoot: runtime.HostPath(projectRoot),
		Image:      runtime.Image{Reference: "image:test", Digest: "sha256:" + strings.Repeat("a", 64)},
		Entrypoint: []string{"/bin/true"}, WorkingDir: "/workspace", User: "1000:1000",
		Mounts: []runtime.Mount{
			{Source: volumeOwner.Name(), Target: "/workspace", Type: "volume", Authority: runtime.MountAuthorityVolume},
			{Source: reviewedSource, Target: "/reviewed", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityReviewedHost},
		},
		Networks: []string{networkOwner.Name()}, Labels: workspaceOwner.Labels(),
	}
	if _, err := validWorkspace(spec); err != nil {
		t.Fatalf("reviewed host mount rejected: %v", err)
	}
	if argument, err := mountArg(spec.Mounts[1]); err != nil || argument != "type=bind,source="+reviewedSource+",target=/reviewed,readonly" {
		t.Fatalf("reviewed mount serialization = %q, %v", argument, err)
	}

	unsafe := spec
	unsafe.Mounts = append([]runtime.Mount(nil), spec.Mounts...)
	unsafe.Mounts[1].Source = projectRoot
	if _, err := validWorkspace(unsafe); err == nil {
		t.Fatal("reviewed host mount accepted project source root")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	unsafe = spec
	unsafe.Mounts = append([]runtime.Mount(nil), spec.Mounts...)
	unsafe.Mounts[1].Source = canonicalHome
	if _, err := validWorkspace(unsafe); err == nil {
		t.Fatal("reviewed host mount accepted host home")
	}
	unsafe = spec
	unsafe.Mounts = append([]runtime.Mount(nil), spec.Mounts...)
	unsafe.Mounts[1].Target = "/workspace/reviewed"
	if _, err := validWorkspace(unsafe); err == nil {
		t.Fatal("reviewed host mount accepted protected workspace target")
	}
	link := filepath.Join(base, "reviewed-link")
	if err := os.Symlink(reviewedSource, link); err != nil {
		t.Fatal(err)
	}
	unsafe = spec
	unsafe.Mounts = append([]runtime.Mount(nil), spec.Mounts...)
	unsafe.Mounts[1].Source = link
	if _, err := validWorkspace(unsafe); err == nil {
		t.Fatal("reviewed host mount accepted symlink source")
	}
}

func TestWorkspaceMountsRejectAllHostSourceAndHomePaths(t *testing.T) {
	for _, source := range []string{
		"/Volumes/Dev/project",
		"/Users/dsx",
		"/Users/dsx/project",
		"/home/dsx",
		"/root",
		"/private/tmp/dsx-source",
	} {
		mount := runtime.Mount{
			Source: source, Target: "/workspace", Type: "bind",
			Authority: runtime.MountAuthorityVolume,
		}
		if _, err := mountArg(mount); err == nil {
			t.Errorf("mountArg(%q) accepted a host source or home path", source)
		}
	}
}

func TestMountAuthorityAllowsOnlyVolumesAndPinnedGuestHelper(t *testing.T) {
	for _, mount := range []runtime.Mount{
		{Source: "/Volumes/Dev/reviewed", Target: "/reviewed", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityVolume},
		{Source: "/Volumes/Dev/reviewed", Target: "/reviewed", Type: "bind", ReadOnly: true},
		{Source: "cache", Target: "/cache", Type: "volume"},
		{Source: "cache", Target: "/usr/local/libexec/dsx", Type: "volume", Authority: runtime.MountAuthorityGuestHelper},
	} {
		if _, err := mountArg(mount); err == nil {
			t.Errorf("mountArg(%#v) accepted wrong authority/source pairing", mount)
		}
	}
	volume := runtime.Mount{Source: "cache", Target: "/cache", Type: "volume", Authority: runtime.MountAuthorityVolume}
	if argument, err := mountArg(volume); err != nil || argument != "type=volume,source=cache,target=/cache" {
		t.Fatalf("volume serialization = %q, %v", argument, err)
	}
}

func TestStartRejectsRuntimeGrantReplacementAtMutationBoundary(t *testing.T) {
	expected := expectedWorkspace(t)
	id := string(expected.ID)
	altered := bytes.Replace(f(t, "container-inspect-1.2.2.json"), []byte(`"destination":"/workspace"`), []byte(`"destination":"/stolen"`), 1)
	runner := &cr{t: t, q: []call{{a: []string{"inspect", id}, o: altered}}}
	err := ad(t, runner).StartWorkspace(context.Background(), expected)
	if err == nil || model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("StartWorkspace() error = %v", err)
	}
	runner.done()
}

func browserSpec(t *testing.T) runtime.BrowserSpec {
	t.Helper()
	identity := oi(t, runtime.ResourceBrowser, "browser")
	network := oi(t, runtime.ResourceNetwork, "network")
	return runtime.BrowserSpec{
		Name:        identity.Name(),
		Image:       runtime.Image{Reference: "browser:test", Digest: "sha256:" + strings.Repeat("a", 64)},
		Entrypoint:  []string{"/usr/bin/node", "/srv/mcp.js", "--port", "8931"},
		Env:         []string{"PLAYWRIGHT_BROWSERS_PATH=/ms-playwright"},
		Networks:    []string{network.Name()},
		Labels:      identity.Labels(),
		CPUs:        2,
		MemoryBytes: 1 << 30,
	}
}

func browserCreateArgs(spec runtime.BrowserSpec) []string {
	args := []string{
		"create", "--name", spec.Name,
		"--env", spec.Env[0],
		"--network", spec.Networks[0],
	}
	args = labelArgs(args, spec.Labels)
	args = append(args,
		"--cpus", strconv.Itoa(spec.CPUs),
		"--memory", strconv.FormatInt(spec.MemoryBytes, 10),
		"--entrypoint", spec.Entrypoint[0],
		imageRef(spec.Image),
	)
	return append(args, spec.Entrypoint[1:]...)
}

func replaceFixture(t *testing.T, input []byte, old, replacement string) []byte {
	t.Helper()
	if count := bytes.Count(input, []byte(old)); count != 1 {
		t.Fatalf("fixture occurrence count for %q = %d, want 1", old, count)
	}
	return bytes.Replace(input, []byte(old), []byte(replacement), 1)
}

func ambiguousBrowserInspect(t *testing.T) []byte {
	t.Helper()
	var containers []appleContainer
	if err := json.Unmarshal(f(t, "browser-inspect-1.2.2.json"), &containers); err != nil || len(containers) != 1 || len(containers[0].Status.Networks) != 1 {
		t.Fatalf("decode browser fixture for mutation: count=%d error=%v", len(containers), err)
	}

	containers[0].Status.Networks = append(containers[0].Status.Networks, containers[0].Status.Networks[0])
	output, err := json.Marshal(containers)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func duplicateNetworkAddressInspect(t *testing.T) []byte {
	t.Helper()
	var containers []appleContainer
	if err := json.Unmarshal(f(t, "browser-inspect-1.2.2.json"), &containers); err != nil || len(containers) != 1 || len(containers[0].Status.Networks) != 1 {
		t.Fatalf("decode browser fixture for mutation: count=%d error=%v", len(containers), err)
	}
	containers[0].Configuration.Networks = append(containers[0].Configuration.Networks, appleContainerNetwork{Network: "second-network"})
	duplicate := containers[0].Status.Networks[0]
	duplicate.Network = "second-network"
	containers[0].Status.Networks = append(containers[0].Status.Networks, duplicate)
	output, err := json.Marshal(containers)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func expectedWorkspace(t *testing.T) runtime.ResourceSnapshot {
	t.Helper()
	snapshots, err := decodeContainers(f(t, "container-inspect-1.2.2.json"))
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("decode expected workspace: count=%d err=%v", len(snapshots), err)
	}
	return snapshots[0]
}
func expectedBrowser(t *testing.T) runtime.ResourceSnapshot {
	t.Helper()
	snapshots, err := decodeContainers(f(t, "browser-inspect-1.2.2.json"))
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("decode expected browser: count=%d err=%v", len(snapshots), err)
	}
	return snapshots[0]
}

func legacyWorkspaceFixture(t *testing.T) ([]byte, runtime.ResourceSnapshot) {
	t.Helper()
	currentName := "dsx-tracking-chrome-feature-a-workspace-1abbf9"
	legacyName := "dsx-dk57swfmuu5gpt6knms5-feature-a-workspace"
	fixture := bytes.ReplaceAll(f(t, "container-inspect-1.2.2.json"), []byte(currentName), []byte(legacyName))
	fixture = bytes.ReplaceAll(fixture, []byte(`"dsx.ownership/v2"`), []byte(`"dsx.ownership/v1"`))
	fixture = bytes.ReplaceAll(fixture, []byte(`"dev.dsx.workspace"`), []byte(`"dev.dsx.sandbox"`))
	snapshots, err := decodeContainers(fixture)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("decode legacy fixture: count=%d err=%v", len(snapshots), err)
	}
	return fixture, snapshots[0]
}
func labelMap(labels []runtime.Label) map[string]string {
	result := make(map[string]string, len(labels))
	for _, label := range labels {
		result[label.Key] = label.Value
	}
	return result
}
