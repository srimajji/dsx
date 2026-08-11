package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

const (
	swVersExecutable = "/usr/bin/sw_vers"
	unameExecutable  = "/usr/bin/uname"

	minimumDarwinMajor = 25
	minimumMacOSMajor  = 26
	minimumVersion     = "1.2.2"
	maximumVersion     = "1.3.0"
	compatibilityID    = "apple-container/cli-1.2.2/server-1.2.2"
)

var probeEnvironment = []string{"LANG=C", "LC_ALL=C"}

type ProbeErrorKind string

const (
	ProbeInvalidExecutable  ProbeErrorKind = "invalid_executable"
	ProbeCommandFailed      ProbeErrorKind = "command_failed"
	ProbeInvalidOutput      ProbeErrorKind = "invalid_output"
	ProbeUnsupportedHost    ProbeErrorKind = "unsupported_host"
	ProbeUnsupportedArch    ProbeErrorKind = "unsupported_arch"
	ProbeUnsupportedVersion ProbeErrorKind = "unsupported_version"
	ProbeUnsupportedPatch   ProbeErrorKind = "unsupported_patch"
	ProbeVersionMismatch    ProbeErrorKind = "version_mismatch"
	ProbeServiceUnhealthy   ProbeErrorKind = "service_unhealthy"
)

type ProbeError struct {
	Kind        ProbeErrorKind
	Component   string
	Observed    string
	Required    string
	Remediation string
	Cause       error
}

func (err *ProbeError) Error() string {
	if err == nil {
		return "<nil>"
	}
	message := fmt.Sprintf("Apple runtime probe %s: %s", err.Component, strings.ReplaceAll(string(err.Kind), "_", " "))
	if err.Observed != "" {
		message += fmt.Sprintf(" (observed %q)", err.Observed)
	}
	if err.Required != "" {
		message += fmt.Sprintf("; require %s", err.Required)
	}
	if err.Remediation != "" {
		message += "; " + err.Remediation
	}
	return message
}

func (err *ProbeError) Unwrap() error { return err.Cause }

type Adapter struct {
	runner              Runner
	containerExecutable string
}

func NewAdapter(runner Runner, containerExecutable string) (*Adapter, error) {
	if runner == nil {
		return nil, invalidProbeConfiguration("runner", "", "provide an Apple command runner", errors.New("runner is nil"))
	}
	if containerExecutable == "" || !filepath.IsAbs(containerExecutable) || filepath.Clean(containerExecutable) != containerExecutable {
		return nil, invalidProbeConfiguration("container CLI", containerExecutable, "provide a clean absolute path to the container executable", nil)
	}
	return &Adapter{runner: runner, containerExecutable: containerExecutable}, nil
}

func (adapter *Adapter) Probe(ctx context.Context) (runtime.Capabilities, error) {
	var capabilities runtime.Capabilities
	if adapter == nil || adapter.runner == nil {
		return capabilities, invalidProbeConfiguration("adapter", "", "construct the probe with NewAdapter", errors.New("adapter is nil or incomplete"))
	}
	if ctx == nil {
		return capabilities, invalidProbeConfiguration("context", "", "provide a non-nil context", errors.New("context is nil"))
	}

	hostOS, err := adapter.scalar(ctx, unameExecutable, "host OS", "-s")
	if err != nil {
		return capabilities, err
	}
	capabilities.HostOS = hostOS
	if hostOS != "Darwin" {
		return capabilities, unavailableProbeError(&ProbeError{
			Kind: ProbeUnsupportedHost, Component: "host OS", Observed: hostOS,
			Required: "Darwin", Remediation: "run DSX on a supported Apple silicon Mac",
		})
	}

	darwinRelease, err := adapter.scalar(ctx, unameExecutable, "Darwin release", "-r")
	if err != nil {
		return capabilities, err
	}
	darwinMajor, err := majorVersion(darwinRelease)
	if err != nil {
		return capabilities, invalidOutput("Darwin release", darwinRelease, "uname -r must return a dotted numeric release", err)
	}
	if darwinMajor < minimumDarwinMajor {
		return capabilities, unavailableProbeError(&ProbeError{
			Kind: ProbeUnsupportedHost, Component: "Darwin release", Observed: darwinRelease,
			Required: "Darwin 25 or newer", Remediation: "upgrade macOS before using DSX",
		})
	}

	hostVersion, err := adapter.scalar(ctx, swVersExecutable, "macOS version", "-productVersion")
	if err != nil {
		return capabilities, err
	}
	if err := validateProductVersion(hostVersion); err != nil {
		return capabilities, invalidOutput("macOS version", hostVersion, "sw_vers must return a numeric product version", err)
	}
	capabilities.HostVersion = hostVersion
	macOSMajor, err := majorVersion(hostVersion)
	if err != nil {
		return capabilities, invalidOutput("macOS version", hostVersion, "sw_vers must return a numeric product version", err)
	}
	if macOSMajor < minimumMacOSMajor {
		return capabilities, unavailableProbeError(&ProbeError{
			Kind: ProbeUnsupportedHost, Component: "macOS version", Observed: hostVersion,
			Required: "macOS 26 or newer", Remediation: "upgrade macOS before using DSX",
		})
	}

	hostArch, err := adapter.scalar(ctx, unameExecutable, "host architecture", "-m")
	if err != nil {
		return capabilities, err
	}
	capabilities.HostArch = hostArch
	if hostArch != "arm64" {
		return capabilities, unavailableProbeError(&ProbeError{
			Kind: ProbeUnsupportedArch, Component: "host architecture", Observed: hostArch,
			Required: "arm64", Remediation: "run DSX natively on Apple silicon without architecture translation",
		})
	}

	versionResult, err := adapter.run(ctx, adapter.containerExecutable, "container system version", "system", "version", "--format", "json")
	if err != nil {
		return capabilities, err
	}
	cliVersion, serverVersion, err := decodeSystemVersion(versionResult.Stdout)
	if err != nil {
		return capabilities, invalidOutput("container system version", string(versionResult.Stdout), "install container 1.2.2 and ensure its system version command emits JSON", err)
	}
	capabilities.CLIVersion = cliVersion
	capabilities.ServerVersion = serverVersion
	if err := validateVersionPair(cliVersion, serverVersion); err != nil {
		return capabilities, err
	}
	capabilities.CompatibilityID = compatibilityID

	_, statusServerVersion, err := adapter.systemStatus(ctx)
	if err != nil {
		return capabilities, err
	}
	if statusServerVersion != serverVersion {
		return capabilities, versionMismatch("container system status", statusServerVersion, serverVersion)
	}
	capabilities.ServiceHealthy = true
	setAllowlistedCapabilities(&capabilities)

	builderResult, err := adapter.run(ctx, adapter.containerExecutable, "container builder status", "builder", "status", "--format", "json")
	if err != nil {
		return capabilities, err
	}
	builderHealthy, err := decodeBuilderStatus(builderResult.Stdout)
	if err != nil {
		return capabilities, invalidOutput("container builder status", string(builderResult.Stdout), "repair the Apple container builder and retry", err)
	}
	capabilities.BuilderHealthy = builderHealthy
	return capabilities, nil
}

// Status reports whether the installed Apple container system service is
// running without requiring the API server to be available.
func (adapter *Adapter) Status(ctx context.Context) (runtime.SystemStatus, error) {
	if err := adapter.ready(ctx); err != nil {
		return runtime.SystemStatus{}, err
	}
	command := Command{
		Executable: adapter.containerExecutable,
		Args:       []string{"system", "status", "--format", "json"},
		Env:        append([]string(nil), probeEnvironment...),
	}
	result, runErr := adapter.runner.Run(ctx, command)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return runtime.SystemStatus{}, ctxErr
	}
	var output systemStatus
	if result.StdoutTruncated || decodeJSON(result.Stdout, &output, true) != nil {
		return runtime.SystemStatus{}, unavailableProbeError(&ProbeError{
			Kind: ProbeInvalidOutput, Component: "container system status", Observed: "unreadable status",
			Required: "bounded JSON status", Remediation: "run `container system start` and retry", Cause: runErr,
		})
	}
	switch output.Status {
	case "running":
		if runErr != nil || result.ExitCode != 0 || result.Signal != "" {
			return runtime.SystemStatus{}, unavailableProbeError(&ProbeError{
				Kind: ProbeCommandFailed, Component: "container system status", Observed: fmt.Sprintf("exit %d", result.ExitCode),
				Required: "exit 0", Remediation: commandRemediation("container system status"), Cause: runErr,
			})
		}
		return runtime.SystemStatus{State: runtime.SystemStateRunning}, nil
	case "stopped", "unregistered":
		return runtime.SystemStatus{
			State: runtime.SystemStateStopped, Remediation: "Run `container system start` to continue.",
		}, nil
	default:
		return runtime.SystemStatus{}, unavailableProbeError(&ProbeError{
			Kind: ProbeInvalidOutput, Component: "container system status", Observed: output.Status,
			Required: `status "running", "stopped", or "unregistered"`, Remediation: "run `dsx doctor` and repair Apple Container",
		})
	}
}

func (adapter *Adapter) StartSystem(ctx context.Context) error {
	if err := adapter.ready(ctx); err != nil {
		return err
	}
	_, err := adapter.command(ctx, "start container system", Command{Args: []string{"system", "start"}})
	return err
}

// CheckSystemStatus gates setup persistence on a running container system.
func (adapter *Adapter) CheckSystemStatus(ctx context.Context) error {
	status, err := adapter.Status(ctx)
	if err != nil {
		return err
	}
	if status.State != runtime.SystemStateRunning {
		return unavailableProbeError(&ProbeError{
			Kind: ProbeServiceUnhealthy, Component: "container API service", Observed: string(status.State),
			Required: `status "running"`, Remediation: "run `container system start` and retry",
		})
	}
	return nil
}

func (adapter *Adapter) systemStatus(ctx context.Context) (string, string, error) {
	result, err := adapter.run(ctx, adapter.containerExecutable, "container system status", "system", "status", "--format", "json")
	if err != nil {
		return "", "", err
	}
	status, serverVersion, err := decodeSystemStatus(result.Stdout)
	if err != nil {
		return "", "", invalidOutput("container system status", string(result.Stdout), "start the Apple container service and retry", err)
	}
	if status != "running" {
		return "", "", unavailableProbeError(&ProbeError{
			Kind: ProbeServiceUnhealthy, Component: "container API service", Observed: status,
			Required: `JSON status "running"`, Remediation: "run `container system start` and retry",
		})
	}
	return status, serverVersion, nil
}

func (adapter *Adapter) scalar(ctx context.Context, executable, component string, args ...string) (string, error) {
	result, err := adapter.run(ctx, executable, component, args...)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(result.Stdout))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", invalidOutput(component, value, "command must emit exactly one non-empty line", nil)
	}
	return value, nil
}

func (adapter *Adapter) run(ctx context.Context, executable, component string, args ...string) (Result, error) {
	command := Command{
		Executable: executable,
		Args:       append([]string(nil), args...),
		Env:        append([]string(nil), probeEnvironment...),
	}
	result, err := adapter.runner.Run(ctx, command)
	if err != nil {
		observed := fmt.Sprintf("exit %d", result.ExitCode)
		if result.ExitCode == -1 {
			observed = "executable unavailable"
		}
		return result, unavailableProbeError(&ProbeError{
			Kind: ProbeCommandFailed, Component: component, Observed: observed,
			Required: "a successful read-only command", Remediation: commandRemediation(component), Cause: err,
		})
	}
	if result.ExitCode != 0 {
		return result, unavailableProbeError(&ProbeError{
			Kind: ProbeCommandFailed, Component: component, Observed: fmt.Sprintf("exit %d", result.ExitCode),
			Required: "exit 0", Remediation: commandRemediation(component),
		})
	}
	return result, nil
}

func commandRemediation(component string) string {
	if component == "container system status" {
		return "run `container system start` and retry; if the command is unavailable, install container 1.2.2"
	}
	if strings.HasPrefix(component, "container") {
		return "install container 1.2.2, start its system service, and retry"
	}
	return "verify the macOS host tools and retry"
}

type systemVersionEntry struct {
	AppName   string `json:"appName"`
	BuildType string `json:"buildType"`
	Commit    string `json:"commit"`
	Version   string `json:"version"`
}

func decodeSystemVersion(data []byte) (string, string, error) {
	var entries []systemVersionEntry
	if err := decodeJSON(data, &entries, true); err != nil {
		return "", "", err
	}
	if len(entries) != 2 {
		return "", "", fmt.Errorf("expected exactly two version entries, got %d", len(entries))
	}
	var cliVersion, serverVersion string
	for _, entry := range entries {
		switch entry.AppName {
		case "container":
			if cliVersion != "" {
				return "", "", errors.New("duplicate container version entry")
			}
			if _, err := parseVersion(entry.Version); err != nil {
				return "", "", fmt.Errorf("invalid container version: %w", err)
			}
			cliVersion = entry.Version
		case "container-apiserver":
			if serverVersion != "" {
				return "", "", errors.New("duplicate container-apiserver version entry")
			}
			version, err := parseServerVersion(entry.Version)
			if err != nil {
				return "", "", err
			}
			serverVersion = version
		default:
			return "", "", fmt.Errorf("unexpected version entry %q", entry.AppName)
		}
	}
	if cliVersion == "" || serverVersion == "" {
		return "", "", errors.New("version output is missing container or container-apiserver")
	}
	return cliVersion, serverVersion, nil
}

type systemStatus struct {
	APIServerAppName string `json:"apiServerAppName"`
	APIServerBuild   string `json:"apiServerBuild"`
	APIServerCommit  string `json:"apiServerCommit"`
	APIServerVersion string `json:"apiServerVersion"`
	AppRoot          string `json:"appRoot"`
	InstallRoot      string `json:"installRoot"`
	Status           string `json:"status"`
}

func decodeSystemStatus(data []byte) (string, string, error) {
	var output systemStatus
	if err := decodeJSON(data, &output, true); err != nil {
		return "", "", err
	}
	if output.APIServerAppName != "container-apiserver" {
		return "", "", fmt.Errorf("unexpected API server app name %q", output.APIServerAppName)
	}
	if output.Status == "" {
		return "", "", errors.New("system status is empty")
	}
	version, err := parseServerVersion(output.APIServerVersion)
	if err != nil {
		return "", "", err
	}
	return output.Status, version, nil
}

type builderStatus struct {
	Configuration struct {
		ID string `json:"id"`
	} `json:"configuration"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

func decodeBuilderStatus(data []byte) (bool, error) {
	var entries []builderStatus
	if err := decodeJSON(data, &entries, false); err != nil {
		return false, err
	}
	seen := false
	healthy := false
	for _, entry := range entries {
		if entry.Configuration.ID != "buildkit" {
			continue
		}
		if seen {
			return false, errors.New("duplicate buildkit builder entries")
		}
		seen = true
		if entry.Status.State == "" {
			return false, errors.New("buildkit builder state is empty")
		}
		healthy = entry.Status.State == "running"
	}
	return seen && healthy, nil
}

func decodeJSON(data []byte, destination any, disallowUnknown bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("JSON output contains trailing values")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("read JSON terminator: %w", err)
	}
	return nil
}

type semanticVersion struct {
	major int
	minor int
	patch int
}

func parseVersion(value string) (semanticVersion, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return semanticVersion{}, fmt.Errorf("version %q must contain major.minor.patch", value)
	}
	values := [3]int{}
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semanticVersion{}, fmt.Errorf("version %q contains a non-canonical numeric component", value)
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return semanticVersion{}, fmt.Errorf("version %q contains a non-numeric component", value)
		}
		values[index] = number
	}
	return semanticVersion{major: values[0], minor: values[1], patch: values[2]}, nil
}

func validateProductVersion(value string) error {
	switch strings.Count(value, ".") {
	case 1:
		_, err := parseVersion(value + ".0")
		return err
	case 2:
		_, err := parseVersion(value)
		return err
	default:
		return fmt.Errorf("product version %q must contain major.minor or major.minor.patch", value)
	}
}

func majorVersion(value string) (int, error) {
	part, _, _ := strings.Cut(value, ".")
	major, err := strconv.Atoi(part)
	if err != nil || major < 0 {
		return 0, fmt.Errorf("invalid major version %q", part)
	}
	return major, nil
}

func parseServerVersion(value string) (string, error) {
	const prefix = "container-apiserver version "
	if !strings.HasPrefix(value, prefix) {
		return "", fmt.Errorf("API server version does not start with %q", prefix)
	}
	remainder := strings.TrimPrefix(value, prefix)
	version, details, found := strings.Cut(remainder, " (")
	if !found || version == "" || !strings.HasSuffix(details, ")") {
		return "", errors.New("API server version has an unexpected machine-field format")
	}
	if _, err := parseVersion(version); err != nil {
		return "", fmt.Errorf("invalid API server version: %w", err)
	}
	return version, nil
}

func validateVersionPair(cliVersion, serverVersion string) error {
	cli, err := parseVersion(cliVersion)
	if err != nil {
		return invalidOutput("container CLI version", cliVersion, "install container 1.2.2", err)
	}
	server, err := parseVersion(serverVersion)
	if err != nil {
		return invalidOutput("container API server version", serverVersion, "install container 1.2.2", err)
	}
	if compareVersion(cli, mustVersion(minimumVersion)) < 0 || compareVersion(cli, mustVersion(maximumVersion)) >= 0 {
		return unsupportedVersion("container CLI", cliVersion)
	}
	if compareVersion(server, mustVersion(minimumVersion)) < 0 || compareVersion(server, mustVersion(maximumVersion)) >= 0 {
		return unsupportedVersion("container API server", serverVersion)
	}
	if cliVersion != serverVersion {
		return versionMismatch("container CLI/API server", cliVersion, serverVersion)
	}
	if cliVersion != minimumVersion {
		return unavailableProbeError(&ProbeError{
			Kind: ProbeUnsupportedPatch, Component: "container CLI/API server", Observed: cliVersion,
			Required: "the tested 1.2.2/1.2.2 pair", Remediation: "install container 1.2.2; later 1.2.x patches require an explicit DSX compatibility allowlist entry",
		})
	}
	return nil
}

func compareVersion(left, right semanticVersion) int {
	if left.major != right.major {
		return left.major - right.major
	}
	if left.minor != right.minor {
		return left.minor - right.minor
	}
	return left.patch - right.patch
}

func mustVersion(value string) semanticVersion {
	version, err := parseVersion(value)
	if err != nil {
		panic(err)
	}
	return version
}

func unsupportedVersion(component, observed string) error {
	return unavailableProbeError(&ProbeError{
		Kind: ProbeUnsupportedVersion, Component: component, Observed: observed,
		Required: ">=1.2.2 and <1.3.0", Remediation: "install the supported Apple container 1.2.2 release",
	})
}

func versionMismatch(component, observed, required string) error {
	return unavailableProbeError(&ProbeError{
		Kind: ProbeVersionMismatch, Component: component, Observed: observed,
		Required: required, Remediation: "install matching container CLI and API server versions, restart the service, and retry",
	})
}

func invalidOutput(component, observed, remediation string, cause error) error {
	return unavailableProbeError(&ProbeError{
		Kind: ProbeInvalidOutput, Component: component, Observed: observed,
		Required: "valid machine-readable output", Remediation: remediation, Cause: cause,
	})
}

func unavailableProbeError(err *ProbeError) error {
	return model.NewError(model.CodeUnavailable, err.Error(), err)
}

func invalidProbeConfiguration(component, observed, remediation string, cause error) error {
	err := &ProbeError{
		Kind: ProbeInvalidExecutable, Component: component, Observed: observed,
		Required: "valid probe configuration", Remediation: remediation, Cause: cause,
	}
	return model.NewError(model.CodeInvalidInput, err.Error(), err)
}

func setAllowlistedCapabilities(capabilities *runtime.Capabilities) {
	capabilities.MachineReadableInspection = true
	capabilities.Labels = true
	capabilities.Networks = true
	capabilities.Volumes = true
	capabilities.Copy = true
	capabilities.FixedPublication = false
	capabilities.DynamicPublication = false
	capabilities.PTY = true
	capabilities.Resize = true
}
