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
func oi(t *testing.T, k runtime.ResourceKind, role string) ownership.Identity {
	p, _ := model.ParseProjectID("abcdefghijklmnopqrst")
	s, _ := model.ParseSandboxName("main")
	u, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000001")
	i, e := ownership.NewIdentity(p, s, u, k, role)
	if e != nil {
		t.Fatal(e)
	}
	return i
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
		"--mount", "type=bind,source=/Volumes/Dev/project,target=/workspace,readonly",
		"--mount", "type=volume,source=cache,target=/cache",
		"--network", "dsx-abcdefghijklmnopqrst-main-network",
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
			{Source: "/Volumes/Dev/project", Target: "/workspace", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityRepository},
			{Source: "cache", Target: "/cache", Type: "volume", Authority: runtime.MountAuthorityVolume},
		},
		Networks: []string{"dsx-abcdefghijklmnopqrst-main-network"},
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
	if _, e := ad(t, &cr{t: t}).CreateWorkspace(context.Background(), s); e == nil {
		t.Fatal("dynamic accepted")
	}
	s.User = "root"
	if _, e := ad(t, &cr{t: t}).CreateWorkspace(context.Background(), s); e == nil {
		t.Fatal("root accepted")
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
			return bytes.ReplaceAll(f(t, "browser-inspect-1.2.2.json"), []byte("dsx-abcdefghijklmnopqrst-main-network"), []byte("foreign-network"))
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
	network := "dsx-abcdefghijklmnopqrst-main-network"
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
			return replaceFixture(t, f(t, "browser-inspect-1.2.2.json"), `"network":"dsx-abcdefghijklmnopqrst-main-network",`+"\n"+`        "variant"`, `"network":"foreign-network",`+"\n"+`        "variant"`)
		}},
		{"duplicate status network", ambiguousBrowserInspect},
		{"duplicate address across networks", duplicateNetworkAddressInspect},
		{"duplicate configured network", func(t *testing.T) []byte {
			return replaceFixture(t, f(t, "browser-inspect-1.2.2.json"), `[{"network":"dsx-abcdefghijklmnopqrst-main-network","options":{"hostname":"browser"}}]`, `[{"network":"dsx-abcdefghijklmnopqrst-main-network"},{"network":"dsx-abcdefghijklmnopqrst-main-network"}]`)
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
	id := "dsx-abcdefghijklmnopqrst-main-workspace"
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

func TestWorkspaceRejectsSensitiveReadOnlyWritableHostAliases(t *testing.T) {
	for _, test := range []struct {
		name   string
		source func(*testing.T, string) string
	}{
		{
			name: "writable parent",
			source: func(_ *testing.T, sensitive string) string {
				return filepath.Dir(sensitive)
			},
		},
		{
			name: "writable descendant",
			source: func(_ *testing.T, sensitive string) string {
				return filepath.Join(sensitive, "credentials")
			},
		},
		{
			name: "symlink alias",
			source: func(t *testing.T, sensitive string) string {
				alias := filepath.Join(t.TempDir(), "aws-alias")
				if err := os.Symlink(sensitive, alias); err != nil {
					t.Fatal(err)
				}
				return alias
			},
		},
		{
			name: "same inode alias",
			source: func(t *testing.T, sensitive string) string {
				writable := t.TempDir()
				if err := os.Link(filepath.Join(sensitive, "credentials"), filepath.Join(writable, "credentials-alias")); err != nil {
					t.Fatal(err)
				}
				return writable
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			sensitive := filepath.Join(parent, "aws")
			if err := os.Mkdir(sensitive, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sensitive, "config"), []byte("[profile default]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sensitive, "credentials"), []byte("credential-content-must-not-appear"), 0o600); err != nil {
				t.Fatal(err)
			}
			spec := sensitiveWorkspaceSpec(t, sensitive, test.source(t, sensitive))
			_, err := validWorkspace(spec)
			if err == nil {
				t.Fatal("accepted writable alias of sensitive read-only source")
			}
			if strings.Contains(err.Error(), "credential-content-must-not-appear") {
				t.Fatalf("validation error exposed credential contents: %v", err)
			}
		})
	}
}

func TestWorkspaceAllowsDisjointWritableAndSensitiveReadOnlySources(t *testing.T) {
	sensitive := t.TempDir()
	if err := os.WriteFile(filepath.Join(sensitive, "config"), []byte("[profile default]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sensitive, "credentials"), []byte("credential-content-must-not-appear"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validWorkspace(sensitiveWorkspaceSpec(t, sensitive, t.TempDir())); err != nil {
		t.Fatalf("disjoint writable and sensitive read-only sources rejected: %v", err)
	}
}

func sensitiveWorkspaceSpec(t *testing.T, sensitive, writable string) runtime.WorkspaceSpec {
	t.Helper()
	identity := oi(t, runtime.ResourceWorkspace, "workspace")
	return runtime.WorkspaceSpec{
		Name:       identity.Name(),
		Image:      runtime.Image{Reference: "image:tag", Digest: "sha256:" + strings.Repeat("a", 64)},
		Entrypoint: []string{"/bin/true"},
		WorkingDir: "/workspace",
		User:       "1000",
		Labels:     identity.Labels(),
		Mounts: []runtime.Mount{
			{Source: writable, Target: "/workspace", Type: "bind", Authority: runtime.MountAuthorityRepository},
			{Source: sensitive, Target: "/run/dsx/aws", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityLeappMirror},
		},
	}
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
		Mounts: []runtime.Mount{{Source: "/tmp/../etc", Target: "/workspace", Type: "bind", Authority: runtime.MountAuthorityRepository}},
	}
	if _, err := a.CreateWorkspace(context.Background(), spec); err == nil {
		t.Fatal("accepted unclean host path")
	}
	if _, err := a.afterCreate(context.Background(), Result{Stdout: []byte("wrong\n")}, i.Name(), runtime.ResourceWorkspace, i.Labels()); err == nil {
		t.Fatal("accepted mismatched create output")
	}
}

func TestInspectDoesNotTreatOperationalNotFoundTextAsAbsence(t *testing.T) {

	id := runtime.ResourceID("dsx-abcdefghijklmnopqrst-main-workspace")
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
	spec.Mounts = []runtime.Mount{{Source: helperRoot, Target: "/usr/local/libexec/dsx", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityGuestHelper}}
	if _, err := validWorkspace(spec); err != nil {
		t.Fatalf("pinned root supervisor rejected: %v", err)
	}
	if argument, err := mountArg(spec.Mounts[0]); err != nil ||
		argument != "type=bind,source="+helperRoot+",target=/usr/local/libexec/dsx,readonly" {
		t.Fatalf("guest helper serialization = %q, %v", argument, err)
	}
	spec.Entrypoint = append(spec.Entrypoint, "--initialize-workspace", "/workspace")
	spec.Mounts = append(spec.Mounts, runtime.Mount{Source: "dsx-owned-workspace", Target: "/workspace", Type: "volume", Authority: runtime.MountAuthorityInternal})
	if _, err := validWorkspace(spec); err != nil {
		t.Fatalf("owned-volume root supervisor rejected: %v", err)
	}
	spec.Mounts[len(spec.Mounts)-1] = runtime.Mount{Source: "/tmp/project", Target: "/workspace", Type: "bind", Authority: runtime.MountAuthorityRepository}
	if _, err := validWorkspace(spec); err == nil {
		t.Fatal("host-mounted workspace initialization was accepted")
	}
	spec.Mounts = spec.Mounts[:1]
	spec.Entrypoint = spec.Entrypoint[:8]
	spec.Mounts = append(spec.Mounts, runtime.Mount{Source: "/tmp/hostile", Target: "/usr/local/libexec/dsx/dsx-guest", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityConfiguredHost})
	if _, err := validWorkspace(spec); err == nil {
		t.Fatal("nested helper replacement mount was accepted")
	}

	spec.Mounts = spec.Mounts[:1]
	spec.Entrypoint[2] = "--other"
	if _, err := validWorkspace(spec); err == nil {
		t.Fatal("modified root supervisor authority was accepted")
	}
}
func TestHostMountPathRejectsRuntimeSocketAncestors(t *testing.T) {
	for _, source := range []string{
		"/private",
		"/private/var",
		"/private/var/run",
		"/private/tmp",
		"/var",
		"/var/run",
		"/run",
		"/tmp",
		"/usr/local/libexec",
	} {
		if err := hostMountPath(runtime.HostPath(source)); err == nil {
			t.Errorf("hostMountPath(%q) accepted runtime/control overlap", source)
		}
		if _, err := mountArg(runtime.Mount{Source: source, Target: "/reviewed", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityConfiguredHost}); err == nil {
			t.Errorf("mountArg(%q) accepted runtime/control overlap", source)
		}
	}
	if err := hostMountPath("/Volumes/Dev/project/data"); err != nil {
		t.Fatalf("safe host mount rejected: %v", err)
	}
}

func TestMountAuthorityKeepsUsersSourcesScoped(t *testing.T) {
	repository := runtime.Mount{
		Source:    "/Users/dsx/project",
		Target:    "/workspace",
		Type:      "bind",
		Authority: runtime.MountAuthorityRepository,
	}
	if argument, err := mountArg(repository); err != nil || argument != "type=bind,source=/Users/dsx/project,target=/workspace" {
		t.Fatalf("approved repository serialization = %q, %v", argument, err)
	}

	leapp := runtime.Mount{
		Source:    "/Users/dsx/Library/Application Support/dsx/leapp/project/run/mirror",
		Target:    "/run/dsx/aws",
		Type:      "bind",
		ReadOnly:  true,
		Authority: runtime.MountAuthorityLeappMirror,
	}
	if _, err := mountArg(leapp); err != nil {
		t.Fatalf("DSX Leapp mirror serialization failed: %v", err)
	}

	optional := runtime.Mount{
		Source:    "/Users/dsx/reviewed",
		Target:    "/reviewed",
		Type:      "bind",
		ReadOnly:  true,
		Authority: runtime.MountAuthorityConfiguredHost,
	}
	if _, err := mountArg(optional); err == nil {
		t.Fatal("optional /Users mount was accepted")
	}

	repository.Source = "/Users/dsx"
	if _, err := mountArg(repository); err == nil {
		t.Fatal("complete home was accepted as a repository")
	}
}

func TestMountAuthorityRejectsWrongSourcePairingAndMissingAuthority(t *testing.T) {
	tests := []runtime.Mount{
		{Source: "/Users/dsx/project", Target: "/reviewed", Type: "bind", Authority: runtime.MountAuthorityRepository},
		{Source: "/Users/dsx/Library/Application Support/dsx/leapp/mirror", Target: "/workspace", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityLeappMirror},
		{Source: "cache", Target: "/cache", Type: "volume", Authority: runtime.MountAuthorityRepository},
		{Source: "/Volumes/Dev/reviewed", Target: "/reviewed", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityVolume},
		{Source: "/Volumes/Dev/reviewed", Target: "/reviewed", Type: "bind", ReadOnly: true},
	}
	for _, mount := range tests {
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
