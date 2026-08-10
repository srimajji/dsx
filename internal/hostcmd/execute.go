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
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/terminal"
	"github.com/srimajji/dsx/internal/tui"
)

const helpText = `DSX creates isolated Apple-container development sandboxes.

Usage:
  dsx
  dsx init [--root PATH]
  dsx inspect [--format text|json] [--root PATH] [--mode live|clone] [--sandbox NAME] [--agent NAME]
  dsx doctor [--format text|json] [--require-builder]
  dsx start [--root PATH] --approve-config HASH
  dsx stop [--root PATH] [--name NAME]
  dsx list|ls [--root PATH] [--format text|json]
  dsx clean [--root PATH] [--name NAME] [--force] [--discard-unfetched] [--purge-auth --agent NAME [--profile NAME]]
  dsx clean --all [--force] [--discard-unfetched] [--purge-auth --agent NAME [--profile NAME]]
  dsx shell [--root PATH] [--approve-config HASH] [--agent omp|codex|claude|opencode] [--profile NAME] [-- command args...]
  dsx run --name NAME --agent omp|codex|claude|opencode [--profile NAME] [--browser] --approve-config HASH -- PROMPT
  dsx login --agent omp|codex|claude|opencode --profile NAME --root PATH --approve-config HASH
  dsx git status|diff|fetch|apply NAME [--repo MEMBER] [--root PATH] [--format text|json]
  dsx status [--root PATH] [--format text|json]
  dsx logs [--root PATH] [--format text|json] PROCESS
  dsx version [--json]
  dsx --version [--json]
Run "dsx help" for this help.
`

// Inspector is the read-only application boundary used by the inspect command.
type Inspector interface {
	Inspect(context.Context, app.InspectRequest) (app.InspectResult, error)
}

// Doctor is the read-only application boundary used by the doctor command.
type Doctor interface {
	Doctor(context.Context, app.DoctorRequest) (app.DoctorResult, error)
}

type TUIRunner interface {
	Run(context.Context, tui.RunRequest) (tui.Intent, bool, error)
}

type Lifecycle interface {
	Start(context.Context, app.StartRequest) (app.StartResult, error)
	Stop(context.Context, app.StopRequest) (app.StopResult, error)
	Clean(context.Context, app.CleanRequest) (app.CleanResult, error)
	List(context.Context, app.ListRequest) (app.ListResult, error)
	Shell(context.Context, app.ShellRequest) (app.ShellResult, error)
}

type HarnessRunner interface {
	Run(context.Context, app.HarnessRunRequest) (app.HarnessRunResult, error)
	Login(context.Context, app.HarnessLoginRequest) (app.HarnessLoginResult, error)
	PurgeAuth(context.Context, app.PurgeAuthRequest) error
}

type ProcessStatus interface {
	ProcessStatus(context.Context, app.ProcessStatusRequest) (app.ProcessStatusResult, error)
}

type ProcessLogs interface {
	ProcessLogs(context.Context, app.ProcessLogsRequest) (app.ProcessLogsResult, error)
}

// Dependencies makes application services injectable without exposing command
// parsing or rendering to the application layer.
type Dependencies struct {
	Inspector     Inspector
	Doctor        Doctor
	Lifecycle     Lifecycle
	Harness       HarnessRunner
	Clones        app.CloneManager
	TUI           TUIRunner
	Stdin         io.Reader
	IsTTY         func(io.Reader, io.Writer) bool
	TerminalState terminal.TerminalState
	LoginBrowser  app.LoginBrowserOpener
	Accessible    bool
}

// Dispatcher parses explicit, non-interactive host commands.
type Dispatcher struct {
	dependencies Dependencies
}

func NewDispatcher(dependencies Dependencies) *Dispatcher {
	return &Dispatcher{dependencies: dependencies}
}

// Execute preserves the simple entry point used by callers that only need
// dependency-free help and version commands.
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
	case "version":
		return executeVersion(args[1:], stdout, stderr)
	case "--version":
		return executeVersion(args[1:], stdout, stderr)
	case "inspect":
		return dispatcher.executeInspect(ctx, args[1:], stdout, stderr)
	case "doctor":
		return dispatcher.executeDoctor(ctx, args[1:], stdout, stderr)
	case "init":
		return dispatcher.executeInit(ctx, args[1:], stdout, stderr)
	case "start":
		return dispatcher.executeStart(ctx, args[1:], stdout, stderr)
	case "stop":
		return dispatcher.executeStop(ctx, args[1:], stdout, stderr)
	case "list", "ls":
		return dispatcher.executeList(ctx, args[1:], stdout, stderr)
	case "clean":
		return dispatcher.executeClean(ctx, args[1:], stdout, stderr)
	case "shell":
		return dispatcher.executeShell(ctx, args[1:], stdout, stderr)
	case "login":
		return dispatcher.executeLogin(ctx, args[1:], stdout, stderr)
	case "run":
		return dispatcher.executeRun(ctx, args[1:], stdout, stderr)
	case "git":
		return dispatcher.executeGit(ctx, args[1:], stdout, stderr)
	case "status":
		return dispatcher.executeProcessStatus(ctx, args[1:], stdout, stderr)
	case "logs":
		return dispatcher.executeProcessLogs(ctx, args[1:], stdout, stderr)
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
	return dispatcher.executeTUI(ctx, tui.RunRequest{Root: *root, ForceSetup: true}, stdout, stderr)
}

func (dispatcher *Dispatcher) executeTUI(ctx context.Context, request tui.RunRequest, stdout, stderr io.Writer) int {
	request.Accessible = dispatcher.dependencies.Accessible
	if !dispatcher.interactive(stdout) {
		if request.ForceSetup {
			return usageError(stderr, "dsx init", "init requires an interactive terminal")
		}
		return writeHelp(stdout, stderr)
	}
	if dispatcher == nil || dispatcher.dependencies.TUI == nil {
		return reportError(stderr, "dsx", model.NewError(model.CodeUnavailable, "terminal UI is unavailable", nil))
	}
	if !request.ForceSetup && dispatcher.dependencies.Lifecycle != nil {
		listed, err := dispatcher.dependencies.Lifecycle.List(ctx, app.ListRequest{Root: request.Root})
		if err == nil {
			request.Sandboxes = append([]app.SandboxSummary(nil), listed.Sandboxes...)
		}
	}
	intent, found, err := dispatcher.dependencies.TUI.Run(ctx, request)
	if err != nil {
		return reportError(stderr, "dsx", err)
	}
	if found {
		return dispatcher.executeIntent(ctx, intent, stdout, stderr)
	}
	return 0
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
	mode := flags.String("mode", string(model.ModeLive), "workspace mode: live or clone")
	sandbox := flags.String("sandbox", "", "named clone sandbox")
	agent := flags.String("agent", "", "agent harness")
	browser := flags.Bool("browser", false, "enable isolated Playwright browser in the inspected plan")
	if exit, done := parseFlags(flags, args, stdout, stderr, inspectHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx inspect", "inspect does not accept positional arguments")
	}
	if err := validateFormat(*format); err != nil {
		return reportError(stderr, "dsx inspect", err)
	}
	parsedMode, err := model.ParseWorkspaceMode(*mode)
	if err != nil {
		return usageError(stderr, "dsx inspect", err.Error())
	}
	if *agent != "" {
		if _, err := harness.ParseName(*agent); err != nil {
			return usageError(stderr, "dsx inspect", err.Error())
		}
	}
	if parsedMode == model.ModeClone {
		parsedSandbox, err := model.ParseSandboxName(*sandbox)
		if err != nil || parsedSandbox == model.SandboxName("main") {
			return usageError(stderr, "dsx inspect", "--mode clone requires a named --sandbox other than main")
		}
	} else if *sandbox != "" && *sandbox != "main" {
		return usageError(stderr, "dsx inspect", "--sandbox is only available with --mode clone")
	}
	if dispatcher == nil || dispatcher.dependencies.Inspector == nil {
		return reportError(stderr, "dsx inspect", model.NewError(model.CodeUnavailable, "inspection service is unavailable", nil))
	}

	var browserOverride *bool
	if *browser {
		enabled := true
		browserOverride = &enabled
	}
	result, err := dispatcher.dependencies.Inspector.Inspect(ctx, app.InspectRequest{
		Root:         *root,
		Mode:         string(parsedMode),
		SandboxName:  *sandbox,
		CLIOverrides: app.CLIOverrides{Agent: *agent, Browser: browserOverride},
	})
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
