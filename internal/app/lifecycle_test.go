package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	agentimage "github.com/srimajji/dsx/images/agent"
	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
	statefs "github.com/srimajji/dsx/internal/state/fs"
)

type lifecycleCopy struct {
	source      string
	destination string
	contents    []byte
	mode        os.FileMode
}

type lifecycleRuntime struct {
	resources         map[runtime.ResourceID]runtime.ResourceSnapshot
	calls             []string
	preparedSpecs     []runtime.ExecSpec
	execSpecs         []runtime.ExecSpec
	execInputs        [][]byte
	copies            []lifecycleCopy
	workspaceSpec     runtime.WorkspaceSpec
	browserSpec       runtime.BrowserSpec
	imageSpec         runtime.ImageSpec
	failStart         error
	failBrowserCreate error
	failBrowserStop   error
	failBrowserDelete error
	failCopyTo        error
	cancelOnCreate    context.CancelFunc
	mutateOnEnsure    func()
	ensureImage       func(runtime.ImageSpec) error
	execExit          runtime.Exit
	execOutput        func(runtime.ExecSpec) ([]byte, []byte)
	capabilities      *runtime.Capabilities
	probeErr          error
}

type lifecycleGuest struct {
	runtime    *lifecycleRuntime
	starts     int
	reconciles int
	plan       plan.ExecutionPlan
	status     guestproto.StatusResult
	startErr   error
}

func (guest *lifecycleGuest) Reconcile(context.Context, runtime.ResourceSnapshot) error {
	guest.reconciles++
	if guest.runtime != nil {
		guest.runtime.calls = append(guest.runtime.calls, "guest:reconcile")
	}
	return nil
}

func (guest *lifecycleGuest) Start(_ context.Context, _ runtime.ResourceSnapshot, execution plan.ExecutionPlan, generation uint64) (guestproto.StartResult, error) {
	guest.starts++
	guest.plan = execution
	if guest.runtime != nil {
		guest.runtime.calls = append(guest.runtime.calls, "guest:start")
	}
	if guest.startErr != nil {
		return guestproto.StartResult{}, guest.startErr
	}
	return guestproto.StartResult{Generation: generation + 1}, nil
}

func (guest *lifecycleGuest) Status(context.Context, runtime.ResourceSnapshot) (guestproto.StatusResult, error) {
	return guest.status, nil
}

func (guest *lifecycleGuest) Shutdown(context.Context, runtime.ResourceSnapshot) error {
	return nil
}

func newLifecycleRuntime() *lifecycleRuntime {
	return &lifecycleRuntime{resources: make(map[runtime.ResourceID]runtime.ResourceSnapshot)}
}

func (fake *lifecycleRuntime) Probe(context.Context) (runtime.Capabilities, error) {
	if fake.probeErr != nil {
		return runtime.Capabilities{}, fake.probeErr
	}
	if fake.capabilities != nil {
		return *fake.capabilities, nil
	}
	return runtime.Capabilities{
		ServiceHealthy: true, BuilderHealthy: true, MachineReadableInspection: true,
		Labels: true, Networks: true, Volumes: true, Copy: true, FixedPublication: true, PTY: true, Resize: true,
	}, nil
}
func (fake *lifecycleRuntime) EnsureImage(_ context.Context, spec runtime.ImageSpec) (runtime.Image, error) {
	fake.calls = append(fake.calls, "image")
	fake.imageSpec = spec
	if fake.mutateOnEnsure != nil {
		fake.mutateOnEnsure()
	}
	if fake.ensureImage != nil {
		if err := fake.ensureImage(spec); err != nil {
			return runtime.Image{}, err
		}
	}
	if marker := strings.LastIndex(spec.Reference, "@sha256:"); marker >= 0 {
		return runtime.Image{Reference: spec.Reference[:marker], Digest: spec.Reference[marker+1:]}, nil
	}
	return runtime.Image{Reference: spec.Reference, Digest: "sha256:" + strings.Repeat("b", 64)}, nil
}
func (fake *lifecycleRuntime) CreateVolume(_ context.Context, spec runtime.VolumeSpec) (runtime.Resource, error) {
	return fake.create(spec.Name, runtime.ResourceVolume, spec.Labels), nil
}
func (fake *lifecycleRuntime) CreateNetwork(_ context.Context, spec runtime.NetworkSpec) (runtime.Resource, error) {
	return fake.create(spec.Name, runtime.ResourceNetwork, spec.Labels), nil
}
func (fake *lifecycleRuntime) CreateWorkspace(_ context.Context, spec runtime.WorkspaceSpec) (runtime.Resource, error) {
	fake.workspaceSpec = spec
	resource := fake.create(spec.Name, runtime.ResourceWorkspace, spec.Labels)
	snapshot := fake.resources[resource.ID]
	snapshot.ImageDigest = spec.Image.Digest
	snapshot.Mounts = append([]runtime.Mount(nil), spec.Mounts...)
	snapshot.Networks = append([]string(nil), spec.Networks...)
	snapshot.NetworkAddresses = map[string][]netip.Addr{
		spec.Networks[0]: {netip.MustParseAddr("192.168.64.10")},
	}
	for _, port := range spec.Ports {
		snapshot.Ports = append(snapshot.Ports, runtime.PortBinding{HostIP: port.HostIP, HostPort: *port.HostPort, GuestPort: port.GuestPort, Protocol: port.Protocol})
	}
	fake.resources[resource.ID] = snapshot
	if fake.cancelOnCreate != nil {
		fake.cancelOnCreate()
	}
	return resource, nil
}
func (fake *lifecycleRuntime) CreateBrowser(_ context.Context, spec runtime.BrowserSpec) (runtime.Resource, error) {
	fake.browserSpec = spec
	if fake.failBrowserCreate != nil {
		return runtime.Resource{}, fake.failBrowserCreate
	}
	resource := fake.create(spec.Name, runtime.ResourceBrowser, spec.Labels)
	snapshot := fake.resources[resource.ID]
	snapshot.ImageDigest = spec.Image.Digest
	snapshot.Networks = append([]string(nil), spec.Networks...)
	snapshot.NetworkAddresses = map[string][]netip.Addr{
		spec.Networks[0]: {netip.MustParseAddr("192.168.64.10")},
	}
	fake.resources[resource.ID] = snapshot
	return resource, nil
}
func (fake *lifecycleRuntime) create(name string, kind runtime.ResourceKind, labels []runtime.Label) runtime.Resource {
	fake.calls = append(fake.calls, "create:"+string(kind))
	resource := runtime.Resource{ID: runtime.ResourceID(name), Name: name, Kind: kind}
	fake.resources[resource.ID] = runtime.ResourceSnapshot{Resource: resource, State: "stopped", Labels: append([]runtime.Label(nil), labels...)}
	return resource
}
func (fake *lifecycleRuntime) StartWorkspace(_ context.Context, expected runtime.ResourceSnapshot) error {
	fake.calls = append(fake.calls, "start")
	if fake.failStart != nil {
		return fake.failStart
	}
	snapshot := fake.resources[expected.ID]
	snapshot.State = "running"
	fake.resources[expected.ID] = snapshot
	return nil
}
func (fake *lifecycleRuntime) PrepareExec(_ context.Context, expected runtime.ResourceSnapshot, spec runtime.ExecSpec) (runtime.ProcessSpec, error) {
	fake.calls = append(fake.calls, "prepare:"+strings.Join(spec.Argv, " "))
	fake.preparedSpecs = append(fake.preparedSpecs, cloneRuntimeExecSpec(spec))
	return runtime.ProcessSpec{
		Executable: "/usr/bin/container",
		Args:       append([]string{"exec", "--interactive", "--tty", string(expected.ID)}, spec.Argv...),
		Env:        []string{"PATH=/usr/bin"},
	}, nil
}
func (fake *lifecycleRuntime) Exec(_ context.Context, _ runtime.ResourceSnapshot, spec runtime.ExecSpec, streams runtime.ExecIO) (runtime.Exit, error) {
	fake.calls = append(fake.calls, "exec:"+strings.Join(spec.Argv, " "))
	fake.execSpecs = append(fake.execSpecs, cloneRuntimeExecSpec(spec))
	if streams.Stdin != nil {
		input, err := io.ReadAll(streams.Stdin)
		if err != nil {
			return runtime.Exit{}, err
		}
		fake.execInputs = append(fake.execInputs, input)
	}
	stdout, stderr := []byte("shell-ok\n"), []byte(nil)
	joinedArgv := strings.Join(spec.Argv, " ")
	if isHarnessAttestationRead(spec) {
		stdout, _ = os.ReadFile("../../images/agent/harnesses.lock.json")
	} else if strings.Contains(joinedArgv, " export-file --kind auth ") {
		stdout = []byte(`{"token":"refreshed"}`)
	}
	if fake.execOutput != nil {
		stdout, stderr = fake.execOutput(spec)
	}
	if streams.Stdout != nil {
		_, _ = streams.Stdout.Write(stdout)
	}
	if streams.Stderr != nil {
		_, _ = streams.Stderr.Write(stderr)
	}
	if fake.execExit.Code == nil && fake.execExit.Signal == "" {
		code := 0
		return runtime.Exit{Code: &code}, nil
	}
	return fake.execExit, nil
}

func isHarnessAttestationRead(spec runtime.ExecSpec) bool {
	arguments := spec.Argv
	if len(arguments) < 3 {
		return false
	}
	arguments = arguments[len(arguments)-3:]
	return arguments[0] == "/bin/cat" && arguments[1] == "--" && arguments[2] == "/usr/local/share/dsx/harnesses.lock.json"
}
func (fake *lifecycleRuntime) CopyTo(_ context.Context, _ runtime.ResourceSnapshot, source runtime.HostPath, destination runtime.GuestPath) error {
	fake.calls = append(fake.calls, "copy:"+string(source)+"->"+string(destination))
	copy := lifecycleCopy{source: string(source), destination: string(destination)}
	if info, err := os.Lstat(string(source)); err == nil {
		copy.mode = info.Mode()
	}
	copy.contents, _ = os.ReadFile(string(source))
	fake.copies = append(fake.copies, copy)
	return fake.failCopyTo
}

func cloneRuntimeExecSpec(spec runtime.ExecSpec) runtime.ExecSpec {
	spec.Argv = append([]string(nil), spec.Argv...)
	spec.Env = append([]string(nil), spec.Env...)
	return spec
}
func (fake *lifecycleRuntime) CopyFrom(_ context.Context, _ runtime.ResourceSnapshot, source runtime.GuestPath, destination runtime.HostPath) error {
	fake.calls = append(fake.calls, "copy:"+string(source)+"->"+string(destination))
	return os.WriteFile(string(destination), []byte(`{"token":"refreshed"}`), 0o600)
}
func (fake *lifecycleRuntime) Inspect(_ context.Context, id runtime.ResourceID) (runtime.ResourceSnapshot, error) {
	snapshot, found := fake.resources[id]
	if !found {
		return runtime.ResourceSnapshot{}, runtime.ErrResourceNotFound
	}
	return snapshot, nil
}
func (fake *lifecycleRuntime) List(_ context.Context, kind runtime.ResourceKind) ([]runtime.ResourceSnapshot, error) {
	result := make([]runtime.ResourceSnapshot, 0)
	for _, resource := range fake.resources {
		if resource.Kind == kind {
			result = append(result, resource)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}
func (fake *lifecycleRuntime) Signal(context.Context, runtime.ResourceSnapshot, runtime.Signal) error {
	return nil
}
func (fake *lifecycleRuntime) Stop(_ context.Context, expected runtime.ResourceSnapshot, _ runtime.StopPolicy) error {
	fake.calls = append(fake.calls, "stop")
	if expected.Kind == runtime.ResourceBrowser && fake.failBrowserStop != nil {
		return fake.failBrowserStop
	}
	snapshot := fake.resources[expected.ID]
	snapshot.State = "stopped"
	fake.resources[expected.ID] = snapshot
	return nil
}
func (fake *lifecycleRuntime) Delete(_ context.Context, snapshot runtime.ResourceSnapshot) error {
	resource := snapshot.Resource
	if resource.Kind == runtime.ResourceBrowser && fake.failBrowserDelete != nil {
		return fake.failBrowserDelete
	}
	fake.calls = append(fake.calls, "delete:"+string(resource.Kind))
	delete(fake.resources, resource.ID)
	return nil
}

func TestLifecycleBuildsManagedStandardImageFromEmbeddedInputs(t *testing.T) {
	service, root, _, fake, _ := lifecycleFixture(t)
	configuration := []byte(`{"schemaVersion":1,"workspace":{"root":"."},"image":{"standard":true}}`)
	if err := os.WriteFile(filepath.Join(root, ".dsx", "config.jsonc"), configuration, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := inspectLifecycleHash(t, service.inspection, root)
	fake.ensureImage = func(spec runtime.ImageSpec) error {
		if spec.Reference != "dsx.local/standard:"+agentimage.InputDigest()[:12] || !spec.Reuse {
			t.Fatalf("managed standard image spec = %#v", spec)
		}
		if pathWithin(root, string(spec.Context)) {
			t.Fatalf("managed standard context is inside project: %q", spec.Context)
		}
		digest, err := digestBuildInput(string(spec.Context), config.ImageBuild{Context: ".", File: "Containerfile"})
		if err != nil {
			t.Fatal(err)
		}
		if digest != agentimage.InputDigest() {
			t.Fatalf("builder input digest = %q, want %q", digest, agentimage.InputDigest())
		}
		return nil
	}
	started, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash})
	if err != nil {
		t.Fatal(err)
	}
	if started.State != model.StateRunning {
		t.Fatalf("Start() = %#v", started)
	}
}

func TestLifecycleStartStopResumeShellAndClean(t *testing.T) {
	service, root, manifests, fake, hash := lifecycleFixture(t)
	ctx := context.Background()
	started, err := service.Start(ctx, StartRequest{Root: root, ApproveConfig: hash})
	if err != nil {
		t.Fatal(err)
	}
	if started.State != model.StateRunning || started.Existing {
		t.Fatalf("Start() = %#v", started)
	}
	stored := oneStoredManifest(t, manifests, started.ProjectID)
	if stored.State != model.StateRunning || len(stored.Resources) != 2 {
		t.Fatalf("running manifest = %#v", stored)
	}
	if got := lifecycleCallKinds(fake.calls); !reflect.DeepEqual(got, []string{"image", "create:network", "create:workspace", "start"}) {
		t.Fatalf("creation calls = %#v", got)
	}
	var workspaceMounted, helperMounted bool
	for _, mount := range fake.workspaceSpec.Mounts {
		workspaceMounted = workspaceMounted || (mount.Source == root && mount.Target == "/workspace" && mount.Type == "bind" &&
			!mount.ReadOnly && mount.Authority == runtime.MountAuthorityRepository)
		helperMounted = helperMounted || (mount.Target == DefaultGuestHelperDirectory && mount.Type == "bind" &&
			mount.ReadOnly && mount.Authority == runtime.MountAuthorityGuestHelper)
	}
	if !workspaceMounted || !helperMounted {
		t.Fatalf("workspace/helper mounts = %#v", fake.workspaceSpec.Mounts)
	}
	for _, label := range fake.workspaceSpec.Labels {
		if label.Key == state.OwnershipRunLabel && label.Value != string(started.RunID) {
			t.Fatalf("workspace run label = %q, want %q", label.Value, started.RunID)
		}
	}

	stopped, err := service.Stop(ctx, StopRequest{Root: root})
	if err != nil || stopped.State != model.StateStopped {
		t.Fatalf("Stop() = %#v, %v", stopped, err)
	}
	resumed, err := service.Start(ctx, StartRequest{Root: root, ApproveConfig: hash})
	if err != nil || !resumed.Existing || resumed.RunID != started.RunID || resumed.State != model.StateRunning {
		t.Fatalf("resume Start() = %#v, %v", resumed, err)
	}
	var output bytes.Buffer
	shell, err := service.Shell(ctx, ShellRequest{Root: root, Argv: []string{"/bin/sh", "-lc", "printf ok"}, Stdout: &output})
	if err != nil || shell.Exit.Code == nil || *shell.Exit.Code != 0 || output.String() != "shell-ok\n" {
		t.Fatalf("Shell() = %#v, output %q, error %v", shell, output.String(), err)
	}
	interactiveCode := 23
	interactive, err := service.Shell(ctx, ShellRequest{
		Root:     root,
		Argv:     []string{"printf", "%s", "a b"},
		Terminal: true,
		RunInteractive: func(_ context.Context, child InteractiveChild) (runtime.Exit, error) {
			want := []string{"/usr/bin/container", "exec", "--interactive", "--tty", string(fake.workspaceSpec.Name), DefaultGuestHelperPath, "exec", "--", "printf", "%s", "a b"}
			if !reflect.DeepEqual(child.Argv, want) {
				t.Fatalf("interactive argv = %#v, want %#v", child.Argv, want)
			}
			return runtime.Exit{Code: &interactiveCode}, nil
		},
	})
	if err != nil || interactive.Exit.Code == nil || *interactive.Exit.Code != interactiveCode {
		t.Fatalf("interactive Shell() = %#v, %v", interactive, err)
	}

	unrelated := runtime.Resource{ID: "unrelated", Name: "unrelated", Kind: runtime.ResourceNetwork}
	fake.resources[unrelated.ID] = runtime.ResourceSnapshot{Resource: unrelated, State: "running"}
	cleaned, err := service.Clean(ctx, CleanRequest{Root: root, Confirmed: true})
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.DeletedResources != 2 || cleaned.DeletedManifests != 1 || len(cleaned.Preserved) != 0 {
		t.Fatalf("Clean() = %#v", cleaned)
	}
	if _, found := fake.resources[unrelated.ID]; !found || len(fake.resources) != 1 {
		t.Fatalf("cleanup changed unrelated runtime inventory: %#v", fake.resources)
	}
	if remaining, err := manifests.ListProjectManifests(ctx, started.ProjectID); err != nil || len(remaining) != 0 {
		t.Fatalf("remaining manifests = %#v, %v", remaining, err)
	}
	if got := lifecycleCallKinds(fake.calls[len(fake.calls)-3:]); !reflect.DeepEqual(got, []string{"stop", "delete:workspace", "delete:network"}) {
		t.Fatalf("cleanup order = %#v", got)
	}
}

func TestShellApprovalCreatesAndAttachesWithinApplicationTransaction(t *testing.T) {
	service, root, manifests, fake, hash := lifecycleFixture(t)
	zero := 0
	fake.execExit = runtime.Exit{Code: &zero}
	var output bytes.Buffer
	result, err := service.Shell(context.Background(), ShellRequest{
		Root: root, ApproveConfig: hash, Argv: []string{"/bin/true"}, Stdout: &output,
	})
	if err != nil || result.Exit.Code == nil || *result.Exit.Code != 0 {
		t.Fatalf("Shell() = %#v, error %v", result, err)
	}
	projectID, projectErr := projectIDForRoot(root)
	if projectErr != nil {
		t.Fatal(projectErr)
	}
	stored := oneStoredManifest(t, manifests, projectID)
	if stored.State != model.StateRunning {
		t.Fatalf("manifest state = %q", stored.State)
	}
	if got := lifecycleCallKinds(fake.calls); !reflect.DeepEqual(got, []string{"image", "create:network", "create:workspace", "start"}) {
		t.Fatalf("start-or-attach calls = %#v", got)
	}
	if output.String() != "shell-ok\n" {
		t.Fatalf("shell output = %q", output.String())
	}
}

func TestShellWithoutApprovalRejectsRuntimeGrantDrift(t *testing.T) {
	service, root, _, fake, hash := lifecycleFixture(t)
	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); err != nil {
		t.Fatal(err)
	}
	workspaceID := runtime.ResourceID(fake.workspaceSpec.Name)
	snapshot := fake.resources[workspaceID]
	snapshot.Mounts = append(snapshot.Mounts, runtime.Mount{Type: "bind", Source: "/private/tmp/unapproved", Target: "/unapproved", Authority: runtime.MountAuthorityConfiguredHost})
	fake.resources[workspaceID] = snapshot
	_, err := service.Shell(context.Background(), ShellRequest{Root: root, Argv: []string{"/bin/true"}})
	if model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("Shell() error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "exec:") || strings.HasPrefix(call, "prepare:") {
			t.Fatalf("grant-drift shell reached exec: %#v", fake.calls)
		}
	}
}

func TestInteractiveShellReleasesProjectLockBeforeChildWait(t *testing.T) {
	service, root, _, _, hash := lifecycleFixture(t)
	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	shellDone := make(chan error, 1)
	code := 0
	go func() {
		_, shellErr := service.Shell(context.Background(), ShellRequest{
			Root: root, Terminal: true,
			RunInteractive: func(context.Context, InteractiveChild) (runtime.Exit, error) {
				close(entered)
				<-release
				return runtime.Exit{Code: &code}, nil
			},
		})
		shellDone <- shellErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("interactive shell did not start")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := service.Stop(stopCtx, StopRequest{Root: root}); err != nil {
		t.Fatalf("Stop() while shell waits = %v", err)
	}
	close(release)
	if err := <-shellDone; err != nil {
		t.Fatalf("Shell() = %v", err)
	}
}
func TestLifecycleStartsGuestGraphAndExposesSanitizedProcessData(t *testing.T) {
	service, root, _, fake, _ := lifecycleFixture(t)
	writeLifecycleConfig(t, root, `,"processes":{"web":{"argv":["/bin/sh","-c","echo ready"],"required":true}}`)
	guest := &lifecycleGuest{
		runtime: fake,
		status: guestproto.StatusResult{
			Generation: 1,
			Processes:  []guestproto.ProcessStatus{{ID: "web", State: guestproto.StateReady, Ready: true, Required: true, Log: "ready\n"}},
		},
	}
	service.guest = guest
	hash := inspectLifecycleHash(t, service.inspection, root)
	started, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash})
	if err != nil {
		t.Fatal(err)
	}
	if started.State != model.StateRunning || guest.reconciles != 1 || guest.starts != 1 || len(guest.plan.Processes) != 1 {
		t.Fatalf("start=%#v guest reconciles=%d starts=%d plan=%#v", started, guest.reconciles, guest.starts, guest.plan.Processes)
	}
	if fake.workspaceSpec.User != "0:0" || !reflect.DeepEqual(fake.workspaceSpec.Entrypoint, []string{
		DefaultGuestHelperPath, "serve", "--socket", DefaultGuestSocketPath, "--child-uid", "1000", "--child-gid", "1000",
	}) {
		t.Fatalf("supervised workspace authority = user %q entrypoint %#v", fake.workspaceSpec.User, fake.workspaceSpec.Entrypoint)
	}
	startIndex, reconcileIndex, guestIndex := -1, -1, -1
	for index, call := range fake.calls {
		switch call {
		case "start":
			startIndex = index
		case "guest:reconcile":
			reconcileIndex = index
		case "guest:start":
			guestIndex = index
		}
	}
	if startIndex < 0 || reconcileIndex <= startIndex || guestIndex <= reconcileIndex {
		t.Fatalf("helper/reconcile/process ordering = %#v", fake.calls)
	}
	helperPath, err := service.guestHelperSource()
	if err != nil {
		t.Fatal(err)
	}
	helperMounted := false
	for _, mount := range fake.workspaceSpec.Mounts {
		if mount.Source == filepath.Dir(string(helperPath)) && mount.Target == DefaultGuestHelperDirectory && mount.Type == "bind" && mount.ReadOnly {
			helperMounted = true
		}
	}
	if !helperMounted {
		t.Fatalf("trusted guest helper mount missing: %#v", fake.workspaceSpec.Mounts)
	}
	status, err := service.ProcessStatus(context.Background(), ProcessStatusRequest{Root: root})
	if err != nil || status.Generation != 1 || len(status.Processes) != 1 || !status.Processes[0].Ready {
		t.Fatalf("ProcessStatus() = %#v, %v", status, err)
	}
	logs, err := service.ProcessLogs(context.Background(), ProcessLogsRequest{Root: root, Target: "web"})
	if err != nil || logs.Log != "ready\n" {
		t.Fatalf("ProcessLogs() = %#v, %v", logs, err)
	}
	var shellReady ShellReady
	if _, err := service.Shell(context.Background(), ShellRequest{
		Root: root, Argv: []string{"/bin/true"},
		BeforeExec: func(ready ShellReady) error {
			shellReady = ready
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(shellReady.Processes) != 1 || !shellReady.Processes[0].Ready {
		t.Fatalf("shell readiness = %#v", shellReady)
	}
	if _, err := service.Stop(context.Background(), StopRequest{Root: root}); err != nil {
		t.Fatal(err)
	}
	resumed, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Existing || resumed.State != model.StateRunning || guest.starts != 2 {
		t.Fatalf("resume=%#v guest starts=%d", resumed, guest.starts)
	}
}

func TestLifecycleRejectsProjectControlledGuestHelperBeforeMutation(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	writeLifecycleConfig(t, root, `,"processes":{"web":{"argv":["/bin/true"],"required":true}}`)
	helper := filepath.Join(root, "dsx-guest")
	if err := os.WriteFile(helper, []byte("helper"), 0o755); err != nil {
		t.Fatal(err)
	}
	service.guest = &lifecycleGuest{}
	service.guestHelperSource = func() (runtime.HostPath, error) { return runtime.HostPath(helper), nil }
	hash := inspectLifecycleHash(t, service.inspection, root)
	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatalf("Start() = %v (code %q), want unavailable", err, model.ErrorCodeOf(err))
	}
	if len(fake.calls) != 0 {
		t.Fatalf("untrusted helper reached runtime: %#v", fake.calls)
	}
	projectID, err := projectIDForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if records, err := manifests.ListProjectManifests(context.Background(), projectID); err != nil || len(records) != 0 {
		t.Fatalf("untrusted helper manifests = %#v, %v", records, err)
	}
}

func TestProcessStatusPersistsRequiredGuestFailure(t *testing.T) {
	service, root, manifests, _, _ := lifecycleFixture(t)
	writeLifecycleConfig(t, root, `,"processes":{"web":{"argv":["/bin/true"],"required":true}}`)
	guest := &lifecycleGuest{status: guestproto.StatusResult{
		Generation: 1,
		Processes: []guestproto.ProcessStatus{{
			ID: "web", Generation: 1, State: guestproto.StateReady, Ready: true, Required: true,
		}},
	}}
	service.guest = guest
	hash := inspectLifecycleHash(t, service.inspection, root)
	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); err != nil {
		t.Fatal(err)
	}
	guest.status = guestproto.StatusResult{
		Generation: 1,
		Failed:     true,
		Processes: []guestproto.ProcessStatus{{
			ID: "web", Generation: 1, State: guestproto.StateExited, Required: true,
		}},
	}
	if _, err := service.ProcessStatus(context.Background(), ProcessStatusRequest{Root: root}); err != nil {
		t.Fatal(err)
	}
	projectID, err := projectIDForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := oneStoredManifest(t, manifests, projectID)
	if manifest.State != model.StateFailed || manifest.Failure != "required guest process failed" {
		t.Fatalf("failed manifest = %#v", manifest)
	}
}

func TestProcessStatusWithoutConfiguredGraphReportsRunningWorkspace(t *testing.T) {
	service, root, _, _, _ := lifecycleFixture(t)
	hash := inspectLifecycleHash(t, service.inspection, root)
	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); err != nil {
		t.Fatal(err)
	}
	service.guest = nil
	status, err := service.ProcessStatus(context.Background(), ProcessStatusRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if status.Generation != 0 || status.Failed || status.Processes == nil || len(status.Processes) != 0 {
		t.Fatalf("status = %#v", status)
	}
}

func TestProcessStatusDoesNotPersistRecoverableUnhealthyState(t *testing.T) {
	service, root, manifests, _, _ := lifecycleFixture(t)
	writeLifecycleConfig(t, root, `,"processes":{"web":{"argv":["/bin/true"],"required":true}}`)
	guest := &lifecycleGuest{status: guestproto.StatusResult{
		Generation: 1,
		Processes:  []guestproto.ProcessStatus{{ID: "web", Generation: 1, State: guestproto.StateReady, Ready: true, Required: true}},
	}}
	service.guest = guest
	hash := inspectLifecycleHash(t, service.inspection, root)
	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); err != nil {
		t.Fatal(err)
	}
	guest.status = guestproto.StatusResult{
		Generation: 1,
		Failed:     true,
		Processes:  []guestproto.ProcessStatus{{ID: "web", Generation: 1, State: guestproto.StateUnhealthy, Required: true}},
	}
	status, err := service.ProcessStatus(context.Background(), ProcessStatusRequest{Root: root})
	if err != nil || !status.Failed {
		t.Fatalf("ProcessStatus() = %#v, %v", status, err)
	}
	projectID, err := projectIDForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if manifest := oneStoredManifest(t, manifests, projectID); manifest.State != model.StateRunning {
		t.Fatalf("recoverable unhealthy status persisted terminal state: %#v", manifest)
	}
	guest.status = guestproto.StatusResult{
		Generation: 1,
		Processes:  []guestproto.ProcessStatus{{ID: "web", Generation: 1, State: guestproto.StateReady, Ready: true, Required: true}},
	}
	if _, err := service.Shell(context.Background(), ShellRequest{Root: root, Argv: []string{"/bin/true"}}); err != nil {
		t.Fatalf("Shell() after health recovery = %v", err)
	}
}

func TestStartExistingPersistsTerminalRequiredGuestFailure(t *testing.T) {
	service, root, manifests, _, _ := lifecycleFixture(t)
	writeLifecycleConfig(t, root, `,"processes":{"web":{"argv":["/bin/true"],"required":true}}`)
	guest := &lifecycleGuest{status: guestproto.StatusResult{
		Generation: 1,
		Processes:  []guestproto.ProcessStatus{{ID: "web", Generation: 1, State: guestproto.StateReady, Ready: true, Required: true}},
	}}
	service.guest = guest
	hash := inspectLifecycleHash(t, service.inspection, root)
	started, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash})
	if err != nil {
		t.Fatal(err)
	}
	guest.status = guestproto.StatusResult{
		Generation: 1,
		Failed:     true,
		Processes:  []guestproto.ProcessStatus{{ID: "web", Generation: 1, State: guestproto.StateExited, Required: true}},
	}
	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); !errors.Is(err, errTerminalRequiredGuestFailure) {
		t.Fatalf("Start() error = %v", err)
	}
	manifest := oneStoredManifest(t, manifests, started.ProjectID)
	if manifest.State != model.StateFailed || manifest.Failure != "required guest process failed" {
		t.Fatalf("terminal failure manifest = %#v", manifest)
	}
}

func TestLifecycleCleanRequiresConfirmationAndCleansAllProjects(t *testing.T) {
	service, firstRoot, manifests, fake, firstHash := lifecycleFixture(t)
	if _, err := service.Clean(context.Background(), CleanRequest{Root: firstRoot}); model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("unconfirmed Clean() error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	if len(fake.calls) != 0 {
		t.Fatalf("unconfirmed cleanup reached runtime: %#v", fake.calls)
	}
	secondRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeLifecycleConfig(t, secondRoot, "")
	secondHash := inspectLifecycleHash(t, service.inspection, secondRoot)
	for _, start := range []StartRequest{{Root: firstRoot, ApproveConfig: firstHash}, {Root: secondRoot, ApproveConfig: secondHash}} {
		if _, err := service.Start(context.Background(), start); err != nil {
			t.Fatal(err)
		}
	}
	cleaned, err := service.Clean(context.Background(), CleanRequest{All: true, Confirmed: true})
	if err != nil {
		t.Fatal(err)
	}

	if cleaned.Projects != 2 || cleaned.DeletedManifests != 2 || cleaned.DeletedResources != 4 || len(cleaned.Preserved) != 0 {
		t.Fatalf("Clean(All) = %#v", cleaned)
	}
	if remaining, err := manifests.ListAllManifests(context.Background()); err != nil || len(remaining) != 0 {
		t.Fatalf("remaining manifests = %#v, %v", remaining, err)
	}
	if len(fake.resources) != 0 {
		t.Fatalf("remaining runtime resources = %#v", fake.resources)
	}
}
func TestLifecycleCapabilityGatePrecedesIntentAndRuntimeMutation(t *testing.T) {
	service, root, manifests, fake, hash := lifecycleFixture(t)
	fake.capabilities = &runtime.Capabilities{ServiceHealthy: true}
	_, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash})
	if model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatalf("Start() error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	if len(fake.calls) != 0 || len(fake.resources) != 0 {
		t.Fatalf("capability rejection mutated runtime: calls=%#v resources=%#v", fake.calls, fake.resources)
	}
	projectID, projectErr := projectIDForRoot(root)
	if projectErr != nil {
		t.Fatal(projectErr)
	}
	if remaining, listErr := manifests.ListProjectManifests(context.Background(), projectID); listErr != nil || len(remaining) != 0 {
		t.Fatalf("capability rejection manifests = %#v, %v", remaining, listErr)
	}
}
func TestCleanAllReportsAndPreservesManifestlessLabeledResource(t *testing.T) {
	service, root, _, fake, _ := lifecycleFixture(t)
	projectID, err := projectIDForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	resource := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: "orphan-network", Name: "orphan-network", Kind: runtime.ResourceNetwork},
		Labels: []runtime.Label{
			{Key: state.OwnershipManagedLabel, Value: "true"},
			{Key: state.OwnershipContractLabel, Value: state.OwnershipContractValue},
			{Key: state.OwnershipProjectLabel, Value: string(projectID)},
			{Key: state.OwnershipSandboxLabel, Value: "main"},
			{Key: state.OwnershipRunLabel, Value: "01890f5c-7b00-7000-8000-000000000001"},
			{Key: state.OwnershipKindLabel, Value: string(runtime.ResourceNetwork)},
			{Key: state.OwnershipRoleLabel, Value: "network"},
		},
		State: "created",
	}
	fake.resources[resource.ID] = resource
	incomplete := runtime.ResourceSnapshot{
		Resource: runtime.Resource{
			ID: "incomplete-network", Name: "dsx-" + string(projectID) + "-main-incomplete", Kind: runtime.ResourceNetwork,
		},
		Labels: []runtime.Label{{Key: state.OwnershipProjectLabel, Value: string(projectID)}},
		State:  "created",
	}
	fake.resources[incomplete.ID] = incomplete
	cleaned, err := service.Clean(context.Background(), CleanRequest{All: true, Confirmed: true})
	if model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("Clean(All) error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	if !reflect.DeepEqual(cleaned.Preserved, []string{"dsx-" + string(projectID) + "-main-incomplete", "orphan-network"}) {
		t.Fatalf("Clean(All) preserved = %#v", cleaned.Preserved)
	}
	if _, found := fake.resources[resource.ID]; !found {
		t.Fatal("manifestless complete-label resource was deleted without manifest corroboration")
	}
	if _, found := fake.resources[incomplete.ID]; !found {
		t.Fatal("manifestless incomplete-label resource was deleted or omitted")
	}
}

func TestLifecycleStartupFailureRollsBackInReverseOrder(t *testing.T) {
	service, root, manifests, fake, hash := lifecycleFixture(t)
	fake.failStart = errors.New("injected start failure")
	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); !errors.Is(err, fake.failStart) {
		t.Fatalf("Start() error = %v", err)
	}
	projectID, err := projectIDForRoot(root)

	if err != nil {
		t.Fatal(err)
	}
	if remaining, err := manifests.ListProjectManifests(context.Background(), projectID); err != nil || len(remaining) != 0 {
		t.Fatalf("rollback manifests = %#v, %v", remaining, err)
	}
	if len(fake.resources) != 0 {
		t.Fatalf("rollback resources = %#v", fake.resources)
	}
	if got := lifecycleCallKinds(fake.calls); !reflect.DeepEqual(got, []string{"image", "create:network", "create:workspace", "start", "delete:workspace", "delete:network"}) {
		t.Fatalf("rollback calls = %#v", got)
	}
}
func TestLifecycleResumeRejectsRuntimeGrantDrift(t *testing.T) {
	service, root, _, fake, hash := lifecycleFixture(t)
	started, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Stop(context.Background(), StopRequest{Root: root}); err != nil {
		t.Fatal(err)
	}
	workspaceID := runtime.ResourceID(fake.workspaceSpec.Name)
	snapshot := fake.resources[workspaceID]
	snapshot.Mounts = append(snapshot.Mounts, runtime.Mount{Source: "/tmp/host-home", Target: "/host-home", Type: "bind", Authority: runtime.MountAuthorityConfiguredHost})
	fake.resources[workspaceID] = snapshot
	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("resume with drift error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	if fake.resources[workspaceID].State != "stopped" {
		t.Fatalf("drifted workspace restarted: %#v", fake.resources[workspaceID])
	}
	if started.State != model.StateRunning {
		t.Fatalf("initial start = %#v", started)
	}
	if _, err := service.Clean(context.Background(), CleanRequest{Root: root, Confirmed: true}); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleCleanupPreservesExactIDWithContradictoryKind(t *testing.T) {
	service, root, manifests, fake, hash := lifecycleFixture(t)
	started, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash})
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := runtime.ResourceID(fake.workspaceSpec.Name)
	snapshot := fake.resources[workspaceID]
	snapshot.Kind = runtime.ResourceBrowser
	fake.resources[workspaceID] = snapshot

	cleaned, err := service.Clean(context.Background(), CleanRequest{Root: root, Confirmed: true})
	if model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("Clean() error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	if len(cleaned.Preserved) != 1 || cleaned.Preserved[0] != string(workspaceID) {
		t.Fatalf("Clean() preserved = %#v", cleaned.Preserved)
	}
	if _, found := fake.resources[workspaceID]; !found {
		t.Fatal("contradictory exact-ID workspace was deleted")
	}
	remaining, listErr := manifests.ListProjectManifests(context.Background(), started.ProjectID)
	if listErr != nil || len(remaining) != 1 {
		t.Fatalf("recovery manifest = %#v, %v", remaining, listErr)
	}
}

func TestLifecycleCancellationStillUsesOwnershipSafeRollback(t *testing.T) {
	service, root, manifests, fake, hash := lifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fake.cancelOnCreate = cancel
	if _, err := service.Start(ctx, StartRequest{Root: root, ApproveConfig: hash}); err == nil {
		t.Fatal("Start() succeeded after cancellation")
	}
	projectID, err := projectIDForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if remaining, err := manifests.ListProjectManifests(context.Background(), projectID); err != nil || len(remaining) != 0 {
		t.Fatalf("cancellation manifests = %#v, %v", remaining, err)
	}
	if len(fake.resources) != 0 {
		t.Fatalf("cancellation resources = %#v", fake.resources)
	}
}

func TestLifecycleRejectsStaleApprovalBeforeRuntimeMutation(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	_, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: strings.Repeat("f", 64)})
	if model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("Start() error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	projectID, projectErr := projectIDForRoot(root)
	if projectErr != nil {
		t.Fatal(projectErr)
	}
	if records, listErr := manifests.ListProjectManifests(context.Background(), projectID); listErr != nil || len(records) != 0 {
		t.Fatalf("stale approval manifests = %#v, %v", records, listErr)
	}
	if len(fake.calls) != 0 || len(fake.resources) != 0 {
		t.Fatalf("stale approval mutated runtime: calls=%#v resources=%#v", fake.calls, fake.resources)
	}
}

func TestLifecycleRevalidatesHostMountIdentityBeforeCreate(t *testing.T) {
	service, root, manifests, fake, hash := lifecycleFixture(t)
	hostMount := filepath.Join(root, "approved-host")
	if err := os.Mkdir(hostMount, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLifecycleConfig(t, root, `,"mounts":[{"source":{"type":"host","path":"`+filepath.ToSlash(hostMount)+`"},"target":"/approved","readOnly":true}]`)
	hash = inspectLifecycleHash(t, service.inspection, root)
	fake.mutateOnEnsure = func() {
		original := hostMount + ".old"
		if err := os.Rename(hostMount, original); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(hostMount, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash})
	if model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("Start() error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	projectID, projectErr := projectIDForRoot(root)
	if projectErr != nil {
		t.Fatal(projectErr)
	}
	if records, listErr := manifests.ListProjectManifests(context.Background(), projectID); listErr != nil || len(records) != 0 {
		t.Fatalf("mount swap manifests = %#v, %v", records, listErr)
	}
	if len(fake.resources) != 0 {
		t.Fatalf("mount swap left runtime resources: %#v", fake.resources)
	}
}

func TestLifecycleAssignsConfiguredHostMountAuthority(t *testing.T) {
	service, root, _, fake, _ := lifecycleFixture(t)
	hostMount := filepath.Join(root, "approved-host")
	if err := os.Mkdir(hostMount, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLifecycleConfig(t, root, `,"mounts":[{"source":{"type":"host","path":"`+filepath.ToSlash(hostMount)+`"},"target":"/approved","readOnly":true}]`)
	hash := inspectLifecycleHash(t, service.inspection, root)
	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); err != nil {
		t.Fatal(err)
	}
	for _, mount := range fake.workspaceSpec.Mounts {
		if mount.Target == "/approved" {
			if mount.Source != hostMount || mount.Type != "bind" || !mount.ReadOnly ||
				mount.Authority != runtime.MountAuthorityConfiguredHost {
				t.Fatalf("configured host mount = %#v", mount)
			}
			return
		}
	}
	t.Fatalf("configured host mount absent: %#v", fake.workspaceSpec.Mounts)
}

func TestWorkspaceSpecRejectsCompleteHostHomeRepository(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	execution := plan.ExecutionPlan{
		Project:      plan.ProjectIdentity{ID: "aaaaaaaaaaaaaaaaaaaa", CanonicalRoot: home},
		Sandbox:      plan.SandboxIdentity{Name: "main", RunID: "01890f5c-7b00-7000-8000-000000000099"},
		Repositories: []plan.RepositoryPlan{{Name: "workspace", HostPath: home, GuestPath: "/workspace"}},
	}
	_, err = workspaceSpecForPlan(execution, runtime.Image{}, state.ResourceRecord{Name: "workspace"}, "network", nil, nil, "1000:1000", "", "")
	if model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("complete home repository error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
}

func TestLifecycleBuildUsesPrivateStagedInputAndCleansAfterSuccess(t *testing.T) {
	service, root, _, fake, _ := lifecycleFixture(t)
	writeLifecycleBuildConfig(t, root)
	dockerfileBytes := []byte("FROM scratch\nCOPY source.txt /source.txt\n")
	sourceBytes := []byte("approved source bytes")
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), dockerfileBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), sourceBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	approved := inspectLifecyclePlan(t, service.inspection, root)
	var stageRoot string
	fake.ensureImage = func(spec runtime.ImageSpec) error {
		stageRoot = filepath.Clean(string(spec.Context))
		if stageRoot == root || pathWithin(root, stageRoot) {
			t.Fatalf("EnsureImage context = %q, want staging outside %q", stageRoot, root)
		}
		if filepath.Clean(string(spec.File)) != filepath.Join(stageRoot, "Dockerfile") {
			t.Fatalf("EnsureImage file = %q, want staged Dockerfile", spec.File)
		}
		info, err := os.Stat(stageRoot)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("staging mode = %#o, want 0700", info.Mode().Perm())
		}
		gotDockerfile, err := os.ReadFile(string(spec.File))
		if err != nil {
			t.Fatal(err)
		}
		gotSource, err := os.ReadFile(filepath.Join(stageRoot, "source.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotDockerfile, dockerfileBytes) || !bytes.Equal(gotSource, sourceBytes) {
			t.Fatalf("staged bytes differ: Dockerfile=%q source=%q", gotDockerfile, gotSource)
		}
		stagedDigest, err := digestBuildInput(stageRoot, config.ImageBuild{Context: ".", File: "Dockerfile"})
		if err != nil {
			t.Fatal(err)
		}
		if stagedDigest != approved.Image.InputDigest {
			t.Fatalf("staged digest = %q, want approved %q", stagedDigest, approved.Image.InputDigest)
		}
		return nil
	}
	started, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: approved.ExecutableHash})
	if err != nil {
		t.Fatal(err)
	}
	if started.State != model.StateRunning || stageRoot == "" {
		t.Fatalf("Start() = %#v, stage %q", started, stageRoot)
	}
	if _, err := os.Stat(stageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory remains after success: %q, error %v", stageRoot, err)
	}
}
func TestLifecycleRejectsStagedBuildMutationDuringBuilderConsumption(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	writeLifecycleBuildConfig(t, root)
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\nCOPY source.txt /source.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("approved source"), 0o600); err != nil {
		t.Fatal(err)
	}
	approved := inspectLifecyclePlan(t, service.inspection, root)
	var stageRoot string
	fake.ensureImage = func(spec runtime.ImageSpec) error {
		stageRoot = filepath.Clean(string(spec.Context))
		return os.WriteFile(filepath.Join(stageRoot, "source.txt"), []byte("mutated by builder race"), 0o600)
	}
	_, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: approved.ExecutableHash})
	if model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("Start() error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	projectID, projectErr := projectIDForRoot(root)
	if projectErr != nil {
		t.Fatal(projectErr)
	}
	if records, listErr := manifests.ListProjectManifests(context.Background(), projectID); listErr != nil || len(records) != 0 {
		t.Fatalf("builder mutation manifests = %#v, %v", records, listErr)
	}
	if len(fake.resources) != 0 {
		t.Fatalf("builder mutation created runtime resources: %#v", fake.resources)
	}
	if _, statErr := os.Stat(stageRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("staging directory remains after rejection: %q, error %v", stageRoot, statErr)
	}
}

func TestLifecycleBuildCleansStagingAfterEnsureImageFailure(t *testing.T) {
	service, root, _, fake, _ := lifecycleFixture(t)
	writeLifecycleBuildConfig(t, root)
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("approved"), 0o600); err != nil {
		t.Fatal(err)
	}
	approved := inspectLifecyclePlan(t, service.inspection, root)
	injected := errors.New("injected EnsureImage failure")
	var stageRoot string
	fake.ensureImage = func(spec runtime.ImageSpec) error {
		stageRoot = filepath.Clean(string(spec.Context))
		if _, err := os.Stat(stageRoot); err != nil {
			t.Fatalf("staging missing during EnsureImage: %v", err)
		}
		return injected
	}
	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: approved.ExecutableHash}); !errors.Is(err, injected) {
		t.Fatalf("Start() error = %v, want injected failure", err)
	}
	if stageRoot == "" {
		t.Fatal("EnsureImage was not called")
	}
	if _, err := os.Stat(stageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory remains after EnsureImage failure: %q, error %v", stageRoot, err)
	}
	if len(fake.resources) != 0 {
		t.Fatalf("EnsureImage failure created resources: %#v", fake.resources)
	}
}

func TestLifecycleBuildRejectsReplacementDuringStagingBeforeEnsureImage(t *testing.T) {
	service, root, _, fake, _ := lifecycleFixture(t)
	writeLifecycleBuildConfig(t, root)
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\nCOPY source.txt /source.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(root, "source.txt")
	sourceBytes := []byte("approved")
	if err := os.WriteFile(sourcePath, sourceBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	approved := inspectLifecyclePlan(t, service.inspection, root)
	previousHook := buildInputStageOpened
	defer func() {
		buildInputStageOpened = previousHook
	}()
	var mutationErr error
	mutated := false
	buildInputStageOpened = func(path string) {
		if path != sourcePath || mutated {
			return
		}
		mutated = true
		if err := os.Rename(sourcePath, sourcePath+".approved"); err != nil {
			mutationErr = err
			return
		}
		mutationErr = os.WriteFile(sourcePath, sourceBytes, 0o600)
	}
	_, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: approved.ExecutableHash})
	if mutationErr != nil {
		t.Fatal(mutationErr)
	}
	if !mutated {
		t.Fatal("staging mutation hook was not reached")
	}
	if model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("Start() error = %v (code %q), want unapproved", err, model.ErrorCodeOf(err))
	}
	if len(fake.calls) != 0 {
		t.Fatalf("replacement reached runtime: calls=%#v", fake.calls)
	}
	if len(fake.resources) != 0 {
		t.Fatalf("replacement created resources: %#v", fake.resources)
	}
}
func TestLiveStartReturnsWithPersistentBridgeAndStopClosesExactLease(t *testing.T) {
	service, root, _, fake, _ := lifecycleFixture(t)
	writeLifecycleConfig(t, root, `,"network":{"internet":false,"hostGrants":[{"name":"team-db","destination":"10.40.0.9","port":5432}]},"processes":{"web":{"argv":["/bin/true"],"required":true}}`)
	hash := inspectLifecycleHash(t, service.inspection, root)
	guest := &lifecycleGuest{runtime: fake, status: guestproto.StatusResult{
		Generation: 1,
		Processes:  []guestproto.ProcessStatus{{ID: "web", Generation: 1, State: guestproto.StateReady, Ready: true, Required: true}},
	}}
	service.guest = guest
	service.hostBridges = hostBridgeRuntime{
		routeSource: func(context.Context, netip.Addr) (netip.Addr, error) { return netip.MustParseAddr("192.168.64.1"), nil },
		startTCP: func(context.Context, bridge.TCPGrant) (hostTCPRelay, error) {
			return nil, errors.New("foreground relay must not start")
		},
		lease: time.Hour,
	}
	manager := &hostBridgeLeaseManager{environment: map[string]string{
		"DSX_BRIDGE_TEAM_DB_HOST": "192.168.64.1",
		"DSX_BRIDGE_TEAM_DB_PORT": "49152",
	}}
	manager.onEnsure = func() { fake.calls = append(fake.calls, "bridge:ensure") }
	manager.onStop = func() { fake.calls = append(fake.calls, "bridge:stop") }
	service.bridgeLeases = manager

	started, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash})
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.stopIdentities) != 0 {
		t.Fatalf("Start stopped persistent lease: %#v", manager.stopIdentities)
	}
	ensureIndex, guestIndex := -1, -1
	for index, call := range fake.calls {
		if call == "bridge:ensure" {
			ensureIndex = index
		}
		if call == "guest:start" {
			guestIndex = index
		}
	}
	if ensureIndex < 0 || guestIndex < 0 || ensureIndex > guestIndex {
		t.Fatalf("bridge/guest startup order = %#v", fake.calls)
	}
	if _, err := service.Stop(context.Background(), StopRequest{Root: root}); err != nil {
		t.Fatal(err)
	}
	wantIdentity := bridge.LeaseIdentity{ProjectID: started.ProjectID, Sandbox: started.Sandbox, RunID: started.RunID}
	if !reflect.DeepEqual(manager.stopIdentities, []bridge.LeaseIdentity{wantIdentity}) {
		t.Fatalf("Stop bridge identities = %#v", manager.stopIdentities)
	}
	stopBridge, stopRuntime := -1, -1
	for index, call := range fake.calls {
		if call == "bridge:stop" && stopBridge < 0 {
			stopBridge = index
		}
		if call == "stop" && stopRuntime < 0 {
			stopRuntime = index
		}
	}
	if stopBridge < 0 || stopRuntime < 0 || stopBridge > stopRuntime {
		t.Fatalf("bridge/runtime stop order = %#v", fake.calls)
	}
}

func TestLiveBridgeStartupRollbackStopsHelperAfterGuestFailure(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	writeLifecycleConfig(t, root, `,"network":{"internet":false,"hostGrants":[{"name":"team-db","destination":"10.40.0.9","port":5432}]},"processes":{"web":{"argv":["/bin/true"],"required":true}}`)
	hash := inspectLifecycleHash(t, service.inspection, root)
	service.guest = &lifecycleGuest{runtime: fake, startErr: errors.New("injected guest startup failure")}
	service.hostBridges = hostBridgeRuntime{
		routeSource: func(context.Context, netip.Addr) (netip.Addr, error) { return netip.MustParseAddr("192.168.64.1"), nil },
		startTCP: func(context.Context, bridge.TCPGrant) (hostTCPRelay, error) {
			return nil, errors.New("foreground relay must not start")
		},
		lease: time.Hour,
	}
	manager := &hostBridgeLeaseManager{environment: map[string]string{
		"DSX_BRIDGE_TEAM_DB_HOST": "192.168.64.1",
		"DSX_BRIDGE_TEAM_DB_PORT": "49152",
	}}
	service.bridgeLeases = manager

	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); err == nil {
		t.Fatal("Start succeeded despite injected guest failure")
	}
	if len(manager.ensureSpecs) != 1 || len(manager.stopIdentities) == 0 || manager.stopIdentities[0] != manager.ensureIdentity {
		t.Fatalf("bridge rollback ensure/stop = %#v %#v", manager.ensureIdentity, manager.stopIdentities)
	}
	projectID, _ := projectIDForRoot(root)
	if remaining, err := manifests.ListProjectManifests(context.Background(), projectID); err != nil || len(remaining) != 0 {
		t.Fatalf("startup rollback manifests = %#v, %v", remaining, err)
	}
}

func TestLivePublicationOnlyRestartFailureStopsPersistentLease(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	writeLifecycleConfig(t, root, `,"ports":[{"name":"web","guest":3000,"host":43101,"bind":"127.0.0.1","protocol":"tcp"}],"processes":{"web":{"argv":["/bin/true"],"required":true}}`)
	hash := inspectLifecycleHash(t, service.inspection, root)
	capabilities, err := fake.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	capabilities.FixedPublication = false
	fake.capabilities = &capabilities
	guest := &lifecycleGuest{runtime: fake, status: guestproto.StatusResult{
		Generation: 1,
		Processes:  []guestproto.ProcessStatus{{ID: "web", Generation: 1, State: guestproto.StateReady, Ready: true, Required: true}},
	}}
	service.guest = guest
	manager := &hostBridgeLeaseManager{}
	service.bridgeLeases = manager

	started, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash})
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.ensureSpecs) != 1 || manager.ensureSpecs[0].Mode != bridge.RelayModePublication {
		t.Fatalf("initial publication-only lease = %#v", manager.ensureSpecs)
	}
	if _, err := service.Stop(context.Background(), StopRequest{Root: root}); err != nil {
		t.Fatal(err)
	}
	manager.stopIdentities = nil
	guest.startErr = errors.New("injected publication-only resume failure")

	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash}); err == nil {
		t.Fatal("publication-only live restart succeeded despite guest failure")
	}
	identity := bridge.LeaseIdentity{ProjectID: started.ProjectID, Sandbox: started.Sandbox, RunID: started.RunID}
	if !reflect.DeepEqual(manager.stopIdentities, []bridge.LeaseIdentity{identity}) {
		t.Fatalf("publication-only restart lease stops = %#v", manager.stopIdentities)
	}
	if got := fake.resources[runtime.ResourceID(fake.workspaceSpec.Name)].State; got != "stopped" {
		t.Fatalf("publication-only restarted workspace state = %q", got)
	}
	stored, found, err := manifests.LoadManifest(context.Background(), started.ProjectID, started.Sandbox, started.RunID)
	if err != nil || !found || stored.State != model.StateStopped {
		t.Fatalf("publication-only rollback manifest: found=%t state=%q err=%v", found, stored.State, err)
	}
}

func TestManagedStandardImageSpecUsesGlobalReusableTag(t *testing.T) {
	projectRoot := t.TempDir()
	stageRoot, digest, err := stageStandardImage(context.Background(), projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stageRoot)
	execution := plan.ExecutionPlan{
		Project: plan.ProjectIdentity{ID: "abcdefghijklmnopqrst", CanonicalRoot: projectRoot},
		Image: plan.ResolvedImage{
			Context: "@dsx/standard", File: "Containerfile", InputDigest: digest, Standard: true,
		},
	}
	spec, err := imageSpecForPlan(execution, stageRoot)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Reference != "dsx.local/standard:"+digest[:12] || !spec.Reuse ||
		string(spec.Context) != stageRoot || string(spec.File) != filepath.Join(stageRoot, "Containerfile") {
		t.Fatalf("managed standard image spec = %#v", spec)
	}
	if len(spec.Labels) != 1 || spec.Labels[0].Key != "dev.dsx.standard-input" || spec.Labels[0].Value != digest {
		t.Fatalf("managed standard labels = %#v", spec.Labels)
	}
}

func lifecycleFixture(t *testing.T) (*LifecycleService, string, *statefs.ManifestRepository, *lifecycleRuntime, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeLifecycleConfig(t, root, "")
	inspection := NewInspectionService(plan.NewResolver())
	stateRoot := filepath.Join(t.TempDir(), "state")
	manifests, err := statefs.NewManifestRepository(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	fake := newLifecycleRuntime()
	helperDirectory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(helperDirectory, "dsx-guest")
	if err := os.WriteFile(helper, []byte("guest"), 0o700); err != nil {
		t.Fatal(err)
	}
	cacheParent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stagedHelper, err := StageGuestHelper(runtime.HostPath(helper), filepath.Join(cacheParent, "helper-cache"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	service := NewLifecycleService(LifecycleDependencies{
		Inspection: inspection,
		Manifests:  manifests,
		Locks:      manifests,
		Runtime:    fake,
		Now:        func() time.Time { return now },
		NewRunID: func(time.Time) (model.RunID, error) {
			return model.ParseRunID("01890f5c-7b00-7000-8000-000000000001")
		},
		User:              func() string { return "1000:1000" },
		GuestHelperSource: func() (runtime.HostPath, error) { return stagedHelper, nil },
	})
	service.cloneCleanupFetchedVerifier = func(context.Context, state.Manifest) error { return nil }
	service.cloneCleanupRecovery = func(context.Context, runtime.ResourceSnapshot, *state.Manifest) error { return nil }
	service.cloneCleanupIdentityValidator = func(context.Context, state.Manifest) error { return nil }
	return service, root, manifests, fake, inspectLifecycleHash(t, inspection, root)
}

func writeLifecycleConfig(t *testing.T, root, suffix string) {
	t.Helper()
	configuration := `{"schemaVersion":1,"workspace":{"root":"."},"image":{"ref":"ghcr.io/example/dev@sha256:` + strings.Repeat("a", 64) + `"}` + suffix + `}`
	directory := filepath.Join(root, ".dsx")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeLifecycleBuildConfig(t *testing.T, root string) {
	t.Helper()
	configuration := []byte(`{"schemaVersion":1,"workspace":{"root":"."},"image":{"build":{"context":".","file":"Dockerfile"}}}`)
	directory := filepath.Join(root, ".dsx")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config.jsonc"), configuration, 0o600); err != nil {
		t.Fatal(err)
	}
}

func inspectLifecycleHash(t *testing.T, inspection *InspectionService, root string) string {
	t.Helper()
	return inspectLifecyclePlan(t, inspection, root).ExecutableHash
}

func inspectLifecyclePlan(t *testing.T, inspection *InspectionService, root string) plan.ExecutionPlan {
	t.Helper()
	result, err := inspection.Inspect(context.Background(), InspectRequest{Root: root, SandboxName: "main", Mode: "live"})
	if err != nil {
		t.Fatal(err)
	}
	return result.Plan
}

func oneStoredManifest(t *testing.T, repository *statefs.ManifestRepository, projectID model.ProjectID) state.Manifest {
	t.Helper()
	manifests, err := repository.ListProjectManifests(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 {
		t.Fatalf("stored manifests = %#v", manifests)
	}
	return manifests[0]
}

func lifecycleCallKinds(calls []string) []string {
	result := make([]string, 0, len(calls))
	for _, call := range calls {
		if strings.HasPrefix(call, "exec:") {
			continue
		}
		result = append(result, call)
	}
	return result
}
