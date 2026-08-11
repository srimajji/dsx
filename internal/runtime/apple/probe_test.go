package apple

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

const testContainerExecutable = "/opt/test/bin/container"

type fakeResponse struct {
	result Result
	err    error
}

type fakeProbeRunner struct {
	responses map[string]fakeResponse
	calls     []Command
}

func (runner *fakeProbeRunner) Run(_ context.Context, command Command) (Result, error) {
	copied := command
	copied.Args = append([]string(nil), command.Args...)
	copied.Env = append([]string(nil), command.Env...)
	runner.calls = append(runner.calls, copied)
	response, exists := runner.responses[commandKey(command.Executable, command.Args...)]
	if !exists {
		return Result{ExitCode: -1}, fmt.Errorf("unexpected command: %s %q", command.Executable, command.Args)
	}
	return response.result, response.err
}

func TestProbeHealthy(t *testing.T) {
	runner := healthyProbeRunner(t)
	adapter := newTestAdapter(t, runner)

	capabilities, err := adapter.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.HostOS != "Darwin" || capabilities.HostVersion != "27.0.0" || capabilities.HostArch != "arm64" {
		t.Fatalf("host capabilities = %#v", capabilities)
	}
	if capabilities.CLIVersion != "1.2.2" || capabilities.ServerVersion != "1.2.2" {
		t.Fatalf("runtime versions = %#v", capabilities)
	}
	if capabilities.CompatibilityID != compatibilityID || !capabilities.ServiceHealthy || !capabilities.BuilderHealthy {
		t.Fatalf("runtime health = %#v", capabilities)
	}
	if !capabilities.MachineReadableInspection || !capabilities.Labels || !capabilities.Networks || !capabilities.Volumes || !capabilities.Copy || !capabilities.PTY || !capabilities.Resize {
		t.Fatalf("allowlisted capabilities incomplete: %#v", capabilities)
	}
	if capabilities.FixedPublication || capabilities.DynamicPublication {
		t.Fatal("port publication must remain false until the runtime experiment proves forwarding")
	}
	assertReadOnlyProbeCalls(t, runner.calls)
}
func TestCheckSystemStatusQueriesOnlyServiceStatus(t *testing.T) {
	runner := healthyProbeRunner(t)
	adapter := newTestAdapter(t, runner)
	if err := adapter.CheckSystemStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []Command{{
		Executable: testContainerExecutable,
		Args:       []string{"system", "status", "--format", "json"},
		Env:        append([]string(nil), probeEnvironment...),
	}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("status calls = %#v, want %#v", runner.calls, want)
	}
}

func TestCheckSystemStatusRejectsStoppedService(t *testing.T) {
	runner := healthyProbeRunner(t)
	runner.responses[commandKey(testContainerExecutable, "system", "status", "--format", "json")] =
		stdoutResponse(systemStatusJSON("stopped", "1.2.2"))
	adapter := newTestAdapter(t, runner)

	err := adapter.CheckSystemStatus(context.Background())
	assertProbeError(t, err, ProbeServiceUnhealthy)
	if !strings.Contains(err.Error(), "container system start") {
		t.Fatalf("status error lacks start command: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("status calls = %#v, want one", runner.calls)
	}
	assertReadOnlyProbeCalls(t, runner.calls)
}
func TestStatusRecognizesStoppedUnregisteredService(t *testing.T) {
	runner := healthyProbeRunner(t)
	runner.responses[commandKey(testContainerExecutable, "system", "status", "--format", "json")] = fakeResponse{
		result: Result{
			Stdout:   []byte(`{"apiServerAppName":"","apiServerBuild":"","apiServerCommit":"","apiServerVersion":"","appRoot":"","installRoot":"","status":"unregistered"}`),
			ExitCode: 1,
		},
		err: errors.New("exit status 1"),
	}
	adapter := newTestAdapter(t, runner)

	status, err := adapter.Status(context.Background())
	if err != nil || status.State != runtime.SystemStateStopped || !strings.Contains(status.Remediation, "container system start") {
		t.Fatalf("Status() = %#v, %v", status, err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("status calls = %#v, want one", runner.calls)
	}
	assertReadOnlyProbeCalls(t, runner.calls)
}

func TestStartSystemUsesExactCommand(t *testing.T) {
	runner := &fakeProbeRunner{responses: map[string]fakeResponse{
		commandKey(testContainerExecutable, "system", "start"): stdoutResponse(""),
	}}
	adapter := newTestAdapter(t, runner)
	if err := adapter.StartSystem(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []Command{{
		Executable: testContainerExecutable,
		Args:       []string{"system", "start"},
		Env:        append([]string(nil), probeEnvironment...),
	}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("start calls = %#v, want %#v", runner.calls, want)
	}
}

func TestProbeHostGates(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		output    string
		wantKind  ProbeErrorKind
		wantCalls int
	}{
		{name: "not Darwin", key: commandKey(unameExecutable, "-s"), output: "Linux\n", wantKind: ProbeUnsupportedHost, wantCalls: 1},
		{name: "old Darwin", key: commandKey(unameExecutable, "-r"), output: "24.6.0\n", wantKind: ProbeUnsupportedHost, wantCalls: 2},
		{name: "malformed Darwin", key: commandKey(unameExecutable, "-r"), output: "current\n", wantKind: ProbeInvalidOutput, wantCalls: 2},
		{name: "malformed macOS", key: commandKey(swVersExecutable, "-productVersion"), output: "Tahoe\n", wantKind: ProbeInvalidOutput, wantCalls: 3},
		{name: "wrong architecture", key: commandKey(unameExecutable, "-m"), output: "x86_64\n", wantKind: ProbeUnsupportedArch, wantCalls: 4},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := healthyProbeRunner(t)
			runner.responses[test.key] = stdoutResponse(test.output)
			adapter := newTestAdapter(t, runner)
			capabilities, err := adapter.Probe(context.Background())
			assertProbeError(t, err, test.wantKind)
			if model.ErrorCodeOf(err) != model.CodeUnavailable {
				t.Fatalf("error code = %q", model.ErrorCodeOf(err))
			}
			if len(runner.calls) != test.wantCalls {
				t.Fatalf("calls = %d, want %d", len(runner.calls), test.wantCalls)
			}
			if capabilities.ServiceHealthy || capabilities.BuilderHealthy || capabilities.CompatibilityID != "" {
				t.Fatalf("unproven capabilities leaked through host gate: %#v", capabilities)
			}
			assertReadOnlyProbeCalls(t, runner.calls)
		})
	}
}
func TestVersionGateMacOSProducts(t *testing.T) {
	tests := []struct {
		productVersion string
		wantKind       ProbeErrorKind
	}{
		{productVersion: "25.6.0", wantKind: ProbeUnsupportedHost},
		{productVersion: "26.0", wantKind: ""},
		{productVersion: "27.1.2", wantKind: ""},
	}

	for _, test := range tests {
		t.Run(test.productVersion, func(t *testing.T) {
			runner := healthyProbeRunner(t)
			runner.responses[commandKey(unameExecutable, "-r")] = stdoutResponse("25.0.0\n")
			runner.responses[commandKey(swVersExecutable, "-productVersion")] = stdoutResponse(test.productVersion + "\n")
			adapter := newTestAdapter(t, runner)

			capabilities, err := adapter.Probe(context.Background())
			if test.wantKind != "" {
				assertProbeError(t, err, test.wantKind)
				if len(runner.calls) != 3 {
					t.Fatalf("runtime or architecture queried after macOS gate: %#v", runner.calls)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if capabilities.HostVersion != test.productVersion || !capabilities.ServiceHealthy {
					t.Fatalf("capabilities = %#v", capabilities)
				}
			}
			assertReadOnlyProbeCalls(t, runner.calls)
		})
	}
}

func TestProbeRuntimeOutputFailures(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		response fakeResponse
		wantKind ProbeErrorKind
	}{
		{
			name: "missing CLI", key: commandKey(testContainerExecutable, "system", "version", "--format", "json"),
			response: fakeResponse{result: Result{ExitCode: -1}, err: errors.New("executable not found")}, wantKind: ProbeCommandFailed,
		},
		{
			name: "malformed version JSON", key: commandKey(testContainerExecutable, "system", "version", "--format", "json"),
			response: stdoutResponse("{not-json"), wantKind: ProbeInvalidOutput,
		},
		{
			name: "version human text", key: commandKey(testContainerExecutable, "system", "version", "--format", "json"),
			response: stdoutResponse("container version 1.2.2\n"), wantKind: ProbeInvalidOutput,
		},
		{
			name: "malformed status JSON", key: commandKey(testContainerExecutable, "system", "status", "--format", "json"),
			response: stdoutResponse("[]"), wantKind: ProbeInvalidOutput,
		},
		{
			name: "status human text", key: commandKey(testContainerExecutable, "system", "status", "--format", "json"),
			response: stdoutResponse("container service is running\n"), wantKind: ProbeInvalidOutput,
		},
		{
			name: "malformed builder JSON", key: commandKey(testContainerExecutable, "builder", "status", "--format", "json"),
			response: stdoutResponse("{not-json"), wantKind: ProbeInvalidOutput,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := healthyProbeRunner(t)
			runner.responses[test.key] = test.response
			adapter := newTestAdapter(t, runner)
			capabilities, err := adapter.Probe(context.Background())
			assertProbeError(t, err, test.wantKind)
			if capabilities.BuilderHealthy {
				t.Fatalf("builder health inferred on failure: %#v", capabilities)
			}
			assertReadOnlyProbeCalls(t, runner.calls)
		})
	}
}

func TestProbeServiceGate(t *testing.T) {
	tests := []struct {
		name          string
		status        string
		serverVersion string
		wantKind      ProbeErrorKind
	}{
		{name: "service stopped", status: "stopped", serverVersion: "1.2.2", wantKind: ProbeServiceUnhealthy},
		{name: "service starting", status: "starting", serverVersion: "1.2.2", wantKind: ProbeServiceUnhealthy},
		{name: "status server mismatch", status: "running", serverVersion: "1.2.3", wantKind: ProbeVersionMismatch},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := healthyProbeRunner(t)
			runner.responses[commandKey(testContainerExecutable, "system", "status", "--format", "json")] = stdoutResponse(systemStatusJSON(test.status, test.serverVersion))
			adapter := newTestAdapter(t, runner)
			capabilities, err := adapter.Probe(context.Background())
			assertProbeError(t, err, test.wantKind)
			if capabilities.ServiceHealthy || capabilities.BuilderHealthy {
				t.Fatalf("health inferred before service gate: %#v", capabilities)
			}
			if len(runner.calls) != 6 {
				t.Fatalf("builder must not be probed after service gate; calls = %#v", runner.calls)
			}
			assertReadOnlyProbeCalls(t, runner.calls)
		})
	}
}

func TestProbeBuilderCapability(t *testing.T) {
	runner := healthyProbeRunner(t)
	runner.responses[commandKey(testContainerExecutable, "builder", "status", "--format", "json")] = stdoutResponse(`[{"configuration":{"id":"buildkit"},"status":{"state":"stopped"}}]`)
	adapter := newTestAdapter(t, runner)

	capabilities, err := adapter.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.ServiceHealthy || capabilities.BuilderHealthy {
		t.Fatalf("health = %#v", capabilities)
	}
	assertReadOnlyProbeCalls(t, runner.calls)
}

func TestProbeRejectsInvalidExecutable(t *testing.T) {
	for _, executable := range []string{"", "container", "/opt/test/../bin/container"} {
		_, err := NewAdapter(&fakeProbeRunner{}, executable)
		assertProbeError(t, err, ProbeInvalidExecutable)
		if model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatalf("error code = %q", model.ErrorCodeOf(err))
		}
	}
}

func TestVersionGate(t *testing.T) {
	tests := []struct {
		name     string
		cli      string
		server   string
		wantKind ProbeErrorKind
	}{
		{name: "CLI below range", cli: "1.2.1", server: "1.2.1", wantKind: ProbeUnsupportedVersion},
		{name: "server below range", cli: "1.2.2", server: "1.2.1", wantKind: ProbeUnsupportedVersion},
		{name: "CLI at upper bound", cli: "1.3.0", server: "1.3.0", wantKind: ProbeUnsupportedVersion},
		{name: "server above range", cli: "1.2.2", server: "2.0.0", wantKind: ProbeUnsupportedVersion},
		{name: "versions mismatch", cli: "1.2.2", server: "1.2.3", wantKind: ProbeVersionMismatch},
		{name: "unsupported patch", cli: "1.2.3", server: "1.2.3", wantKind: ProbeUnsupportedPatch},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := healthyProbeRunner(t)
			runner.responses[commandKey(testContainerExecutable, "system", "version", "--format", "json")] = stdoutResponse(systemVersionJSON(test.cli, test.server))
			adapter := newTestAdapter(t, runner)
			capabilities, err := adapter.Probe(context.Background())
			assertProbeError(t, err, test.wantKind)
			if capabilities.CompatibilityID != "" || capabilities.ServiceHealthy || capabilities.MachineReadableInspection {
				t.Fatalf("capability success inferred before version gate: %#v", capabilities)
			}
			if len(runner.calls) != 5 {
				t.Fatalf("service must not be queried after version gate; calls = %#v", runner.calls)
			}
			assertReadOnlyProbeCalls(t, runner.calls)
		})
	}
}

func healthyProbeRunner(t *testing.T) *fakeProbeRunner {
	t.Helper()
	return &fakeProbeRunner{responses: map[string]fakeResponse{
		commandKey(unameExecutable, "-s"):                                            stdoutResponse("Darwin\n"),
		commandKey(unameExecutable, "-r"):                                            stdoutResponse("27.0.0\n"),
		commandKey(swVersExecutable, "-productVersion"):                              stdoutResponse("27.0.0\n"),
		commandKey(unameExecutable, "-m"):                                            stdoutResponse("arm64\n"),
		commandKey(testContainerExecutable, "system", "version", "--format", "json"): stdoutResponse(readFixture(t, "system-version-1.2.2.json")),
		commandKey(testContainerExecutable, "system", "status", "--format", "json"):  stdoutResponse(readFixture(t, "system-status-running.json")),
		commandKey(testContainerExecutable, "builder", "status", "--format", "json"): stdoutResponse(readFixture(t, "builder-status-running.json")),
	}}
}

func newTestAdapter(t *testing.T, runner Runner) *Adapter {
	t.Helper()
	adapter, err := NewAdapter(runner, testContainerExecutable)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func stdoutResponse(output string) fakeResponse {
	return fakeResponse{result: Result{Stdout: []byte(output), ExitCode: 0}}
}

func commandKey(executable string, args ...string) string {
	return executable + "\x00" + strings.Join(args, "\x00")
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func systemVersionJSON(cliVersion, serverVersion string) string {
	return fmt.Sprintf(`[{"appName":"container","buildType":"release","commit":"test","version":%q},{"appName":"container-apiserver","buildType":"release","commit":"test","version":%q}]`, cliVersion, "container-apiserver version "+serverVersion+" (build: release, commit: test)")
}

func systemStatusJSON(status, serverVersion string) string {
	return fmt.Sprintf(`{"apiServerAppName":"container-apiserver","apiServerBuild":"release","apiServerCommit":"test","apiServerVersion":%q,"appRoot":"/tmp/container","installRoot":"/opt/container","status":%q}`, "container-apiserver version "+serverVersion+" (build: release, commit: test)", status)
}

func assertProbeError(t *testing.T, err error, kind ProbeErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", kind)
	}
	var probeError *ProbeError
	if !errors.As(err, &probeError) {
		t.Fatalf("error type = %T, want *ProbeError: %v", err, err)
	}
	if probeError.Kind != kind {
		t.Fatalf("error kind = %q, want %q: %v", probeError.Kind, kind, err)
	}
	if probeError.Component == "" || probeError.Remediation == "" {
		t.Fatalf("error is not actionable: %#v", probeError)
	}
}

func assertReadOnlyProbeCalls(t *testing.T, calls []Command) {
	t.Helper()
	allowed := map[string]bool{
		commandKey(unameExecutable, "-s"):                                            true,
		commandKey(unameExecutable, "-r"):                                            true,
		commandKey(swVersExecutable, "-productVersion"):                              true,
		commandKey(unameExecutable, "-m"):                                            true,
		commandKey(testContainerExecutable, "system", "version", "--format", "json"): true,
		commandKey(testContainerExecutable, "system", "status", "--format", "json"):  true,
		commandKey(testContainerExecutable, "builder", "status", "--format", "json"): true,
	}
	for _, call := range calls {
		if !allowed[commandKey(call.Executable, call.Args...)] {
			t.Fatalf("mutating or unexpected call: %#v", call)
		}
		if !reflect.DeepEqual(call.Env, probeEnvironment) {
			t.Fatalf("environment = %#v, want %#v", call.Env, probeEnvironment)
		}
		if !filepathIsAbsoluteAndClean(call.Executable) {
			t.Fatalf("executable is not absolute and clean: %q", call.Executable)
		}
		if call.Dir != "" || call.Stdin != nil {
			t.Fatalf("probe command has unexpected process inputs: %#v", call)
		}
	}
}

func filepathIsAbsoluteAndClean(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.Contains(path, "/../") && !strings.Contains(path, "/./")
}
