package hostcmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	term "github.com/charmbracelet/x/term"
	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/buildinfo"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/state"
	"github.com/srimajji/dsx/internal/terminal"
	"github.com/srimajji/dsx/internal/tui"
)

const helpText = `DSX creates named, isolated Apple-container development workspaces.

Usage:
  dsx
  dsx init [--root PATH]
  dsx inspect [--format text|json] [--root PATH]
  dsx doctor [--format text|json] [--require-builder]
  dsx workspace create NAME [--root PATH] [--default-agent AGENT] [--approve-config HASH] [--open]
  dsx workspace list [--root PATH] [--format text|json]
  dsx workspace open NAME [--root PATH]
  dsx workspace start NAME [--root PATH]
  dsx workspace stop NAME [--root PATH]
  dsx workspace restart NAME [--root PATH]
  dsx workspace update NAME [--root PATH]
  dsx workspace remove NAME [--root PATH] [--force]
  dsx workspace remove --all|--legacy-resources|--all-projects [--root PATH] [--force]
  dsx agent WORKSPACE [--root PATH] [--agent omp|codex|claude|opencode] [--browser] [-- PROMPT]
  dsx auth status [--root PATH] [--format text|json]
  dsx auth import|login|refresh|purge --agent omp|codex|claude|opencode [--root PATH] [--force]
  dsx aws enable WORKSPACE [--root PATH]
  dsx aws disable WORKSPACE [--root PATH]
  dsx aws status WORKSPACE [--root PATH] [--format text|json]
  dsx git status|diff|fetch|apply WORKSPACE [--repo MEMBER] [--root PATH] [--format text|json]
  dsx version [--json]
  dsx --version [--json]
Run "dsx help" for this help.
`

type Inspector interface {
	Inspect(context.Context, app.InspectRequest) (app.InspectResult, error)
}

type Doctor interface {
	Doctor(context.Context, app.DoctorRequest) (app.DoctorResult, error)
}

type TUIRunner interface {
	Run(context.Context, tui.RunRequest) (tui.Intent, bool, error)
	RunProgress(context.Context, tui.ProgressRequest, tui.ProgressOperation) error
}

type WorkspaceLifecycle interface {
	Create(context.Context, app.WorkspaceCreateRequest) (app.WorkspaceResult, error)
	Open(context.Context, app.WorkspaceOpenRequest) (app.WorkspaceOpenResult, error)
	Start(context.Context, app.WorkspaceStartRequest) (app.WorkspaceResult, error)
	Stop(context.Context, app.WorkspaceStopRequest) (app.WorkspaceResult, error)
	Restart(context.Context, app.WorkspaceRestartRequest) (app.WorkspaceResult, error)
	Remove(context.Context, app.WorkspaceRemoveRequest) (app.WorkspaceRemoveResult, error)
	List(context.Context, app.WorkspaceListRequest) (app.WorkspaceListResult, error)
	AttachInfo(context.Context, app.WorkspaceAttachInfoRequest) (app.WorkspaceAttachInfo, error)
}

type WorkspaceGit interface {
	Update(context.Context, app.WorkspaceUpdateRequest) (app.WorkspaceResult, error)
	GitStatus(context.Context, app.GitStatusRequest) (app.GitStatusResult, error)
	GitDiff(context.Context, app.GitDiffRequest) (app.GitDiffResult, error)
	GitFetch(context.Context, app.GitFetchRequest) (app.GitFetchResult, error)
	GitApply(context.Context, app.GitApplyRequest) (app.GitApplyResult, error)
}

type AgentRunner interface {
	Run(context.Context, app.AgentRunRequest) (app.AgentRunResult, error)
}

type AuthManager interface {
	Status(context.Context, app.AuthStatusRequest) (app.AuthStatusResult, error)
	Import(context.Context, app.AuthImportRequest) (app.AuthImportResult, error)
	Login(context.Context, app.AuthLoginRequest) (app.AuthLoginResult, error)
	Refresh(context.Context, app.AuthRefreshRequest) (app.AuthImportResult, error)
	Purge(context.Context, app.AuthPurgeRequest) error
}

type AWSWorkspaceManager interface {
	Enable(context.Context, app.AWSWorkspaceRequest) (app.AWSWorkspaceResult, error)
	Disable(context.Context, app.AWSWorkspaceRequest) (app.AWSWorkspaceResult, error)
	Status(context.Context, app.AWSWorkspaceRequest) (app.AWSWorkspaceResult, error)
}
type WorkspaceInventory interface {
	ListAllManifests(context.Context) ([]state.Manifest, error)
}

type VSCodeLauncher interface {
	OpenSettings(context.Context) error
}

type Dependencies struct {
	Inspector     Inspector
	Doctor        Doctor
	Workspaces    WorkspaceLifecycle
	Git           WorkspaceGit
	Agents        AgentRunner
	Auth          AuthManager
	AWS           AWSWorkspaceManager
	Inventory     WorkspaceInventory
	TUI           TUIRunner
	VSCode        VSCodeLauncher
	Stdin         io.Reader
	IsTTY         func(io.Reader, io.Writer) bool
	TerminalState terminal.TerminalState
	Accessible    bool
}

type Dispatcher struct {
	dependencies Dependencies
}

func NewDispatcher(dependencies Dependencies) *Dispatcher {
	return &Dispatcher{dependencies: dependencies}
}

func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return NewDispatcher(Dependencies{}).Execute(ctx, args, stdout, stderr)
}

func (dispatcher *Dispatcher) Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if ctx == nil {
		return reportError(stderr, "dsx", model.NewError(model.CodeInternal, "internal error: nil context", nil))
	}
	if stdout == nil || stderr == nil {
		return model.ExitCode(model.NewError(model.CodeInternal, "command output is not configured", nil))
	}
	if len(args) == 0 {
		return dispatcher.executeTUI(ctx, tui.RunRequest{Root: "."}, stdout, stderr)
	}

	switch args[0] {
	case "help", "--help", "-h":
		if len(args) != 1 {
			return usageError(stderr, "dsx", "help does not accept arguments")
		}
		return writeHelp(stdout, stderr)
	case "version", "--version":
		return executeVersion(args[1:], stdout, stderr)
	case "inspect":
		return dispatcher.executeInspect(ctx, args[1:], stdout, stderr)
	case "doctor":
		return dispatcher.executeDoctor(ctx, args[1:], stdout, stderr)
	case "init":
		return dispatcher.executeInit(ctx, args[1:], stdout, stderr)
	case "workspace":
		return dispatcher.executeWorkspace(ctx, args[1:], stdout, stderr)
	case "agent":
		return dispatcher.executeAgent(ctx, args[1:], stdout, stderr)
	case "auth":
		return dispatcher.executeAuth(ctx, args[1:], stdout, stderr)
	case "aws":
		return dispatcher.executeAWS(ctx, args[1:], stdout, stderr)
	case "git":
		return dispatcher.executeGit(ctx, args[1:], stdout, stderr)
	default:
		return usageError(stderr, "dsx", fmt.Sprintf("unknown command %q", args[0]))
	}
}

func (dispatcher *Dispatcher) executeInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("init")
	root := flags.String("root", ".", "project root")
	if exit, done := parseFlags(flags, args, stdout, stderr, initHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx init", "init does not accept positional arguments")
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx init", err)
	}
	return dispatcher.executeTUI(ctx, tui.RunRequest{Root: *root, ForceSetup: true}, stdout, stderr)
}

func (dispatcher *Dispatcher) executeTUI(ctx context.Context, request tui.RunRequest, stdout, stderr io.Writer) int {
	request.Accessible = dispatcher.dependencies.Accessible
	if request.LoadDashboard == nil {
		request.LoadDashboard = dispatcher.loadDashboard
	}
	if !dispatcher.interactive(stdout) {
		if request.ForceSetup {
			return usageError(stderr, "dsx init", "init requires an interactive terminal")
		}
		return writeHelp(stdout, stderr)
	}
	if dispatcher == nil || dispatcher.dependencies.TUI == nil {
		return reportError(stderr, "dsx", model.NewError(model.CodeUnavailable, "terminal UI is unavailable", nil))
	}
	intent, found, err := dispatcher.dependencies.TUI.Run(ctx, request)
	if err != nil {
		return reportError(stderr, "dsx", err)
	}
	if !found {
		return 0
	}
	if intent.Action == "workspace-create" {
		return dispatcher.executeTUIWorkspaceCreate(ctx, request, intent, stdout, stderr)
	}
	return dispatcher.executeIntent(ctx, intent, stdout, stderr)
}

func (dispatcher *Dispatcher) interactive(stdout io.Writer) bool {
	if dispatcher == nil {
		return false
	}
	input := dispatcher.dependencies.Stdin
	if input == nil {
		return false
	}
	if dispatcher.dependencies.IsTTY != nil {
		return dispatcher.dependencies.IsTTY(input, stdout)
	}
	inputFD, inputOK := input.(interface{ Fd() uintptr })
	outputFD, outputOK := stdout.(interface{ Fd() uintptr })
	return inputOK && outputOK && term.IsTerminal(inputFD.Fd()) && term.IsTerminal(outputFD.Fd())
}

func executeVersion(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("version")
	jsonOutput := flags.Bool("json", false, "print machine-readable output")
	if exit, done := parseFlags(flags, args, stdout, stderr, versionHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx version", "version does not accept positional arguments")
	}
	return renderVersion(stdout, stderr, *jsonOutput)
}

func (dispatcher *Dispatcher) executeInspect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("inspect")
	format := flags.String("format", "text", "output format: text or json")
	root := flags.String("root", ".", "project root")
	if exit, done := parseFlags(flags, args, stdout, stderr, inspectHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx inspect", "inspect does not accept positional arguments")
	}
	if err := validateFormat(*format); err != nil {
		return reportError(stderr, "dsx inspect", err)
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx inspect", err)
	}
	if dispatcher == nil || dispatcher.dependencies.Inspector == nil {
		return reportError(stderr, "dsx inspect", model.NewError(model.CodeUnavailable, "inspection service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Inspector.Inspect(ctx, app.InspectRequest{Root: *root})
	if err != nil {
		_ = renderDiagnostics(stderr, result.Diagnostics)
		return reportError(stderr, "dsx inspect", err)
	}
	if renderErr := renderInspect(stdout, result, *format); renderErr != nil {
		return reportError(stderr, "dsx inspect", renderErr)
	}
	if *format == "text" {
		if renderErr := renderDiagnostics(stderr, result.Diagnostics); renderErr != nil {
			return reportError(stderr, "dsx inspect", renderErr)
		}
	}
	return 0
}

func (dispatcher *Dispatcher) executeDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("doctor")
	format := flags.String("format", "text", "output format: text or json")
	requireBuilder := flags.Bool("require-builder", false, "require a healthy image builder")
	if exit, done := parseFlags(flags, args, stdout, stderr, doctorHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx doctor", "doctor does not accept positional arguments")
	}
	if err := validateFormat(*format); err != nil {
		return reportError(stderr, "dsx doctor", err)
	}
	if dispatcher == nil || dispatcher.dependencies.Doctor == nil {
		return reportError(stderr, "dsx doctor", model.NewError(model.CodeUnavailable, "doctor service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Doctor.Doctor(ctx, app.DoctorRequest{RequireBuilder: *requireBuilder})
	if err != nil {
		_ = renderDiagnostics(stderr, result.Diagnostics)
		return reportError(stderr, "dsx doctor", err)
	}
	if renderErr := renderDoctor(stdout, result, *format); renderErr != nil {
		return reportError(stderr, "dsx doctor", renderErr)
	}
	if *format == "text" {
		if renderErr := renderDiagnostics(stderr, result.Diagnostics); renderErr != nil {
			return reportError(stderr, "dsx doctor", renderErr)
		}
	}
	return 0
}

func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	return flags
}

func parseFlags(flags *flag.FlagSet, args []string, stdout, stderr io.Writer, help string) (int, bool) {
	err := flags.Parse(args)
	if err == nil {
		return 0, false
	}
	if errors.Is(err, flag.ErrHelp) {
		if _, writeErr := io.WriteString(stdout, help); writeErr != nil {
			return reportError(stderr, "dsx "+flags.Name(), model.Wrap(model.CodeInternal, "write help", writeErr)), true
		}
		return 0, true
	}
	return usageError(stderr, "dsx "+flags.Name(), err.Error()), true
}

func validateFormat(format string) error {
	if format != "text" && format != "json" {
		return model.NewError(model.CodeUsage, fmt.Sprintf("format must be %q or %q, got %q", "text", "json", format), nil)
	}
	return nil
}

func usageError(stderr io.Writer, command, message string) int {
	return reportError(stderr, command, model.NewError(model.CodeUsage, message, nil))
}

func reportError(stderr io.Writer, command string, err error) int {
	if stderr != nil {
		message := ""
		if err != nil {
			message = terminal.SanitizeLine(err.Error())
		}
		_, _ = fmt.Fprintf(stderr, "%s: %s\n", terminal.SanitizeLine(command), message)
	}
	return model.ExitCode(err)
}

func writeHelp(stdout, stderr io.Writer) int {
	if _, err := io.WriteString(stdout, helpText); err != nil {
		return reportError(stderr, "dsx", model.Wrap(model.CodeInternal, "write help", err))
	}
	return 0
}

func renderVersion(stdout, stderr io.Writer, jsonOutput bool) int {
	info := buildinfo.Current()
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(info); err != nil {
			fmt.Fprintf(stderr, "dsx: write version: %s\n", terminal.SanitizeLine(err.Error()))
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "dsx %s (commit %s, built %s, %s)\n", terminal.SanitizeLine(info.Version), terminal.SanitizeLine(info.Commit), terminal.SanitizeLine(info.BuiltAt), terminal.SanitizeLine(info.GoVersion))
	return 0
}
