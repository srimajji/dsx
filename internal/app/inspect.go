package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srimajji/dsx/internal/config"
	projectinspect "github.com/srimajji/dsx/internal/inspect"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
)

const projectConfigPath = ".dsx/config.jsonc"

type HostMountResolver func(string) (plan.HostMountAuthority, error)

// InspectionDependencies makes the read-only project inspection path injectable.
// Nil functions use the production detector and parser.
type InspectionDependencies struct {
	InspectProject   func(string) (projectinspect.Facts, error)
	ParseConfig      func(string, string) (config.ValidatedConfig, []config.Diagnostic)
	ResolveHostMount HostMountResolver
	Resolver         plan.Resolver
	ConfigRoot       string
	TimingRecorder   PhaseTimingRecorder
	TimingClock      func() time.Time
}

// InspectionService canonicalizes, detects, parses, imports, resolves, and hashes
// a project plan without invoking a runtime or writing project state.
type InspectionService struct {
	inspectProject   func(string) (projectinspect.Facts, error)
	parseConfig      func(string, string) (config.ValidatedConfig, []config.Diagnostic)
	resolveHostMount HostMountResolver
	resolver         plan.Resolver
	configRoot       string
	timingRecorder   PhaseTimingRecorder
	timingClock      func() time.Time
}

func NewInspectionService(resolver plan.Resolver) *InspectionService {
	return NewInspectionServiceWithDependencies(InspectionDependencies{Resolver: resolver})
}

func NewInspectionServiceWithDependencies(dependencies InspectionDependencies) *InspectionService {
	if dependencies.InspectProject == nil {
		dependencies.InspectProject = projectinspect.Inspect
	}
	if dependencies.ParseConfig == nil {
		dependencies.ParseConfig = parseProjectConfig
	}
	if dependencies.Resolver == nil {
		dependencies.Resolver = plan.NewResolver()
	}
	if dependencies.ResolveHostMount == nil {
		dependencies.ResolveHostMount = resolveHostMount
	}
	return &InspectionService{
		inspectProject:   dependencies.InspectProject,
		parseConfig:      dependencies.ParseConfig,
		resolveHostMount: dependencies.ResolveHostMount,
		resolver:         dependencies.Resolver,
		configRoot:       dependencies.ConfigRoot,
		timingRecorder:   dependencies.TimingRecorder,
		timingClock:      dependencies.TimingClock,
	}
}

type configLocation struct {
	absolute string
	display  string
	shared   bool
}

func (service *InspectionService) configLocations(canonicalRoot string) (configLocation, configLocation, error) {
	shared := configLocation{
		absolute: filepath.Join(canonicalRoot, filepath.FromSlash(projectConfigPath)),
		display:  projectConfigPath,
		shared:   true,
	}
	if service.configRoot == "" {
		return configLocation{}, shared, nil
	}
	if !filepath.IsAbs(service.configRoot) || filepath.Clean(service.configRoot) != service.configRoot {
		return configLocation{}, configLocation{}, model.NewError(model.CodeInternal, "DSX config root must be a clean absolute path", nil)
	}
	projectID, err := model.NewProjectID(canonicalRoot)
	if err != nil {
		return configLocation{}, configLocation{}, model.Wrap(model.CodeInvalidInput, "derive project configuration namespace", err)
	}
	name := projectConfigNamespace(filepath.Base(canonicalRoot)) + "-" + string(projectID)
	absolute := filepath.Join(service.configRoot, "projects", name, "config.jsonc")
	return configLocation{absolute: absolute, display: filepath.ToSlash(absolute)}, shared, nil
}

func projectConfigNamespace(name string) string {
	name = strings.ToLower(name)
	var builder strings.Builder
	builder.Grow(len(name))
	separator := false
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			builder.WriteRune(character)
			separator = false
		} else if builder.Len() > 0 && !separator {
			builder.WriteByte('-')
			separator = true
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "project"
	}
	if len(result) > 48 {
		return result[:48]
	}
	return result
}

func (service *InspectionService) activeConfig(canonicalRoot string) (configLocation, bool, error) {
	local, shared, err := service.configLocations(canonicalRoot)
	if err != nil {
		return configLocation{}, false, err
	}
	localExists, err := validConfigFile(local)
	if err != nil {
		return configLocation{}, false, err
	}
	sharedExists, err := validConfigFile(shared)
	if err != nil {
		return configLocation{}, false, err
	}
	if localExists && sharedExists {
		return configLocation{}, false, model.NewError(model.CodeAmbiguous, "both home-local and shared project DSX configurations exist; remove one before continuing", nil)
	}
	if sharedExists {
		return shared, true, nil
	}
	if localExists {
		return local, true, nil
	}
	return configLocation{}, false, nil
}

func validConfigFile(location configLocation) (bool, error) {
	if location.absolute == "" {
		return false, nil
	}
	info, err := os.Lstat(location.absolute)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, model.Wrap(model.CodeInvalidInput, "inspect DSX configuration", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
		return false, model.NewError(model.CodeInvalidInput, fmt.Sprintf("DSX configuration %q must be a regular non-symlink file", location.display), nil)
	default:
		return true, nil
	}
}

func (service *InspectionService) Inspect(ctx context.Context, request InspectRequest) (InspectResult, error) {
	if ctx == nil {
		return InspectResult{}, model.NewError(model.CodeInvalidInput, "inspect: context is nil", nil)
	}
	if service == nil || service.inspectProject == nil || service.parseConfig == nil || service.resolver == nil {
		return InspectResult{}, model.NewError(model.CodeInternal, "inspect service is not configured", nil)
	}
	timing := beginPhase(service.timingRecorder, service.timingClock, PhaseInspect)
	defer timing.Stop()
	facts, err := service.inspectProject(request.Root)
	if err != nil {
		return InspectResult{}, model.Wrap(model.CodeInvalidInput, "inspect project", err)
	}
	result := InspectResult{Facts: mapProjectFacts(facts)}
	result.Diagnostics = append(result.Diagnostics, mapInspectDiagnostics(facts.Diagnostics)...)

	location, found, err := service.activeConfig(facts.WorkspaceRoot)
	if err != nil {
		return result, err
	}
	if !found {
		result.Diagnostics = append(result.Diagnostics, config.Diagnostic{
			Severity: "warning",
			Code:     "incomplete_plan",
			Path:     projectConfigPath,
			Message:  "no DSX configuration exists; detected declarations are shown, but no executable plan was invented",
		})
		sortDiagnostics(result.Diagnostics)
		if diagnosticsHaveErrors(result.Diagnostics) {
			return result, invalidDiagnosticsError(result.Diagnostics)
		}
		return result, nil
	}

	result.Facts.ConfigExists = true
	result.Facts.ConfigPath = location.display
	validated, parseDiagnostics := service.parseConfig(location.absolute, location.display)
	result.Diagnostics = append(result.Diagnostics, parseDiagnostics...)
	if diagnosticsHaveErrors(result.Diagnostics) {
		sortDiagnostics(result.Diagnostics)
		return result, invalidDiagnosticsError(result.Diagnostics)
	}

	authority, err := collectAuthorityInputs(facts.WorkspaceRoot, validated, nil, service.resolveHostMount)
	if err != nil {
		return result, model.Wrap(model.CodeInvalidInput, "resolve inspection authority inputs", err)
	}

	projectID, err := model.NewProjectID(facts.WorkspaceRoot)
	if err != nil {
		return result, model.Wrap(model.CodeInvalidInput, "derive project identity", err)
	}
	mode := model.ModeLive
	if request.Mode != "" {
		mode, err = model.ParseWorkspaceMode(request.Mode)
		if err != nil {
			return result, model.Wrap(model.CodeInvalidInput, "inspect mode", err)
		}
	}
	sandboxName := model.SandboxName("main")
	if request.SandboxName != "" {
		sandboxName, err = model.ParseSandboxName(request.SandboxName)
		if err != nil {
			return result, model.Wrap(model.CodeInvalidInput, "inspect sandbox", err)
		}
	}
	planning := beginPhase(service.timingRecorder, service.timingClock, PhasePlanning)
	resolved, resolveDiagnostics, err := service.resolver.Resolve(ctx, plan.ResolveInput{
		Config:  validated,
		Project: plan.ProjectIdentity{ID: projectID, CanonicalRoot: facts.WorkspaceRoot},
		Sandbox: plan.SandboxIdentity{Name: sandboxName},
		Mode:    mode,
		Ownership: plan.OwnershipPlan{
			Labels:       []plan.KeyValue{{Key: "dsx.project", Value: string(projectID)}, {Key: "dsx.sandbox", Value: string(sandboxName)}},
			ResourceName: "dsx-" + string(projectID) + "-" + string(sandboxName),
		},
		CLI:       plan.CLIOverrides{Agent: request.CLIOverrides.Agent, Browser: request.CLIOverrides.Browser, CPUs: request.CLIOverrides.CPUs, Memory: request.CLIOverrides.Memory},
		Defaults:  plan.DefaultValues{Agent: "codex", Internet: true, CPUs: DefaultWorkspaceCPUs, MemoryBytes: DefaultWorkspaceMemoryBytes, MaxConcurrentClones: 1},
		Authority: authority,
	})
	planning.Stop()
	result.Diagnostics = append(result.Diagnostics, resolveDiagnostics...)
	if err != nil {
		return result, model.Wrap(model.CodeInvalidInput, "resolve inspection plan", err)
	}
	result.Plan = resolved
	sortDiagnostics(result.Diagnostics)
	if diagnosticsHaveErrors(result.Diagnostics) {
		return result, invalidDiagnosticsError(result.Diagnostics)
	}
	return result, nil
}

func parseProjectConfig(absolutePath, displayPath string) (config.ValidatedConfig, []config.Diagnostic) {
	file, err := os.Open(absolutePath)
	if err != nil {
		return config.ValidatedConfig{SourcePath: displayPath}, []config.Diagnostic{{Severity: "error", Code: "read", Path: displayPath, Message: "cannot open configuration: " + err.Error()}}
	}
	defer file.Close()
	return config.Parse(displayPath, file)
}

func mapProjectFacts(facts projectinspect.Facts) ProjectFacts {
	result := ProjectFacts{
		CanonicalRoot: facts.WorkspaceRoot,
		GitRoots:      make([]DetectedPath, 0, len(facts.GitRoots)),
		Lockfiles:     make([]DetectedPath, 0, len(facts.Lockfiles)),
		Dockerfiles:   make([]DetectedPath, 0, len(facts.Containerfiles)),
		DevenvFiles:   make([]DetectedPath, 0, len(facts.Devenv)),
	}
	for _, path := range facts.GitRoots {
		result.GitRoots = append(result.GitRoots, DetectedPath{Path: path, Kind: "git"})
	}
	for _, lockfile := range facts.Lockfiles {
		result.Lockfiles = append(result.Lockfiles, DetectedPath{Path: lockfile.Path, Kind: lockfile.Ecosystem})
	}
	for _, path := range facts.Containerfiles {
		result.Dockerfiles = append(result.Dockerfiles, DetectedPath{Path: path, Kind: "containerfile"})
	}
	for _, devenv := range facts.Devenv {
		result.DevenvFiles = append(result.DevenvFiles, DetectedPath{Path: devenv.Path, Kind: "devenv"})
	}
	return result
}

func mapInspectDiagnostics(diagnostics []projectinspect.Diagnostic) []config.Diagnostic {
	result := make([]config.Diagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		message := diagnostic.Message
		if diagnostic.Field != "" {
			message += " at " + diagnostic.Field
		}
		result = append(result, config.Diagnostic{Severity: string(diagnostic.Severity), Code: diagnostic.Code, Path: diagnostic.Path, Message: message})
	}
	return result
}

func normalizeProjectPath(path string) string {
	path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return strings.TrimPrefix(path, "./")
}

func diagnosticsHaveErrors(diagnostics []config.Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" {
			return true
		}
	}
	return false
}

func invalidDiagnosticsError(diagnostics []config.Diagnostic) error {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" {
			return model.NewError(model.CodeInvalidInput, diagnostic.Message, nil)
		}
	}
	return nil
}

func sortDiagnostics(diagnostics []config.Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.Column != right.Column {
			return left.Column < right.Column
		}
		if left.Severity != right.Severity {
			return left.Severity < right.Severity
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		return left.Message < right.Message
	})
}
