package hostcmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/terminal"
	"github.com/srimajji/dsx/internal/tui"
)

func (dispatcher *Dispatcher) executeStart(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("start")
	root := flags.String("root", ".", "project root")
	approval := flags.String("approve-config", "", "approved executable configuration hash")
	if exit, done := parseFlags(flags, args, stdout, stderr, startHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx start", "start does not accept positional arguments")
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx start", err)
	}
	if !validApprovalHash(*approval) {
		return usageError(stderr, "dsx start", "--approve-config must be exactly 64 lowercase hexadecimal characters")
	}
	if dispatcher == nil || dispatcher.dependencies.Lifecycle == nil {
		return reportError(stderr, "dsx start", model.NewError(model.CodeUnavailable, "lifecycle service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Lifecycle.Start(ctx, app.StartRequest{Root: *root, ApproveConfig: *approval})
	if err != nil {
		return reportError(stderr, "dsx start", err)
	}
	if err := renderStart(stdout, result); err != nil {
		return reportError(stderr, "dsx start", err)
	}
	return 0
}

func (dispatcher *Dispatcher) executeStop(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("stop")
	root := flags.String("root", ".", "project root")
	name := flags.String("name", "", "named clone sandbox")
	if exit, done := parseFlags(flags, args, stdout, stderr, stopHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx stop", "stop does not accept positional arguments")
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx stop", err)
	}
	if err := validateNamedLifecycleSelector(*name); err != nil {
		return usageError(stderr, "dsx stop", err.Error())
	}
	if dispatcher == nil || dispatcher.dependencies.Lifecycle == nil {
		return reportError(stderr, "dsx stop", model.NewError(model.CodeUnavailable, "lifecycle service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Lifecycle.Stop(ctx, app.StopRequest{Root: *root, Sandbox: *name})
	if err != nil {
		return reportError(stderr, "dsx stop", err)
	}
	if err := renderStop(stdout, result); err != nil {
		return reportError(stderr, "dsx stop", err)
	}
	return 0
}

func (dispatcher *Dispatcher) executeClean(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("clean")
	root := flags.String("root", ".", "project root")
	name := flags.String("name", "", "named clone sandbox")
	all := flags.Bool("all", false, "remove DSX-owned resources for every project")
	force := flags.Bool("force", false, "bypass interactive cleanup confirmation")
	discardUnfetched := flags.Bool("discard-unfetched", false, "permit deletion of unfetched or uncaptured clone work")
	purgeAuth := flags.Bool("purge-auth", false, "remove the selected persistent harness authentication profile")
	agent := flags.String("agent", "", "harness owning the authentication profile")
	profile := flags.String("profile", "default", "persistent authentication profile")
	if exit, done := parseFlags(flags, args, stdout, stderr, cleanHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx clean", "clean does not accept positional arguments")
	}
	if *all && *root != "." {
		return usageError(stderr, "dsx clean", "--root cannot be combined with --all")
	}
	if *all && *name != "" {
		return usageError(stderr, "dsx clean", "--name cannot be combined with --all")
	}
	if err := validateNamedLifecycleSelector(*name); err != nil {
		return usageError(stderr, "dsx clean", err.Error())
	}
	if !*all {
		if err := validateRoot(*root); err != nil {
			return reportError(stderr, "dsx clean", err)
		}
	}
	if *purgeAuth {
		if _, err := harness.ParseName(*agent); err != nil {
			return usageError(stderr, "dsx clean", "--purge-auth requires --agent omp|codex|claude|opencode")
		}
		if _, err := model.ParseSandboxName(*profile); err != nil {
			return usageError(stderr, "dsx clean", err.Error())
		}
		if dispatcher == nil || dispatcher.dependencies.Harness == nil {
			return reportError(stderr, "dsx clean", model.NewError(model.CodeUnavailable, "authentication service is unavailable", nil))
		}
	}
	if dispatcher == nil || dispatcher.dependencies.Lifecycle == nil {
		return reportError(stderr, "dsx clean", model.NewError(model.CodeUnavailable, "lifecycle service is unavailable", nil))
	}
	confirmed, confirmationExit := dispatcher.confirmCleanup(*all, *name, *force, stdout, stderr)
	if confirmationExit != 0 || !confirmed {
		return confirmationExit
	}
	result, err := dispatcher.dependencies.Lifecycle.Clean(ctx, app.CleanRequest{
		Root: *root, Sandbox: *name, All: *all, Confirmed: true, DiscardUnfetched: *discardUnfetched,
	})
	if err != nil {
		return reportError(stderr, "dsx clean", err)
	}
	if *purgeAuth {
		if err := dispatcher.dependencies.Harness.PurgeAuth(ctx, app.PurgeAuthRequest{Agent: *agent, Profile: *profile}); err != nil {
			return reportError(stderr, "dsx clean", err)
		}
	}
	if err := renderClean(stdout, result); err != nil {
		return reportError(stderr, "dsx clean", err)
	}
	return 0
}

func (dispatcher *Dispatcher) confirmCleanup(all bool, name string, force bool, stdout, stderr io.Writer) (bool, int) {
	if force {
		return true, 0
	}
	if !dispatcher.interactive(stdout) {
		return false, usageError(stderr, "dsx clean", "cleanup requires an interactive confirmation or --force")
	}
	scope := "this project"
	if all {
		scope = "every DSX project"
	} else if name != "" {
		scope = fmt.Sprintf("sandbox %q", terminal.SanitizeLine(name))
	}
	if _, err := fmt.Fprintf(stdout, "Remove all DSX-owned resources for %s? [y/N] ", scope); err != nil {
		return false, reportError(stderr, "dsx clean", err)
	}
	line, err := bufio.NewReader(io.LimitReader(dispatcher.dependencies.Stdin, 32)).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, reportError(stderr, "dsx clean", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "y" && answer != "yes" {
		_, _ = fmt.Fprintln(stdout, "Cleanup cancelled.")
		return false, 0
	}
	return true, 0
}

func validateNamedLifecycleSelector(value string) error {
	if value == "" {
		return nil
	}
	sandbox, err := model.ParseSandboxName(value)
	if err != nil {
		return err
	}
	if sandbox == model.SandboxName("main") {
		return fmt.Errorf("main is the live workspace and cannot be selected with --name")
	}
	return nil
}

func (dispatcher *Dispatcher) executeList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("list")
	root := flags.String("root", ".", "project root")
	format := flags.String("format", "text", "output format: text or json")
	if exit, done := parseFlags(flags, args, stdout, stderr, listHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx list", "list does not accept positional arguments")
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx list", err)
	}
	if err := validateFormat(*format); err != nil {
		return reportError(stderr, "dsx list", err)
	}
	if dispatcher == nil || dispatcher.dependencies.Lifecycle == nil {
		return reportError(stderr, "dsx list", model.NewError(model.CodeUnavailable, "lifecycle service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Lifecycle.List(ctx, app.ListRequest{Root: *root})
	if err != nil {
		return reportError(stderr, "dsx list", err)
	}
	if err := renderList(stdout, result, *format); err != nil {
		return reportError(stderr, "dsx list", err)
	}
	return 0
}

func (dispatcher *Dispatcher) executeShell(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("shell")
	root := flags.String("root", ".", "project root")
	approval := flags.String("approve-config", "", "exact executable configuration hash for start-or-attach")
	agent := flags.String("agent", "", "agent harness")
	profile := flags.String("profile", "default", "persistent authentication profile")
	if exit, done := parseFlags(flags, args, stdout, stderr, shellHelp); done {
		return exit
	}
	if flags.NArg() != 0 && !argumentsFollowSeparator(args, flags.Args()) {
		return usageError(stderr, "dsx shell", "shell command arguments must follow --")
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx shell", err)
	}
	if *approval != "" && !validApprovalHash(*approval) {
		return usageError(stderr, "dsx shell", "--approve-config must be exactly 64 lowercase hexadecimal characters")
	}
	if *agent != "" {
		if _, err := harness.ParseName(*agent); err != nil {
			return usageError(stderr, "dsx shell", err.Error())
		}
	}
	if _, err := model.ParseSandboxName(*profile); err != nil {
		return usageError(stderr, "dsx shell", err.Error())
	}
	if *agent != "" {
		if flags.NArg() != 0 {
			return usageError(stderr, "dsx shell", "--agent does not accept shell command arguments")
		}
		if !dispatcher.interactive(stdout) {
			return usageError(stderr, "dsx shell", "--agent requires an interactive terminal; use dsx run for one-shot execution")
		}
		return dispatcher.runHarness(ctx, app.HarnessRunRequest{
			Root: *root, ApproveConfig: *approval, Agent: *agent, Profile: *profile, Interactive: true,
			Stdin: dispatcher.dependencies.Stdin, Stdout: stdout, Stderr: stderr,
		}, stderr)
	}
	return dispatcher.runShell(ctx, app.ShellRequest{
		ApproveConfig: *approval,
		Root:          *root,
		Agent:         *agent,
		Argv:          append([]string(nil), flags.Args()...),
		Terminal:      dispatcher.interactive(stdout),
		Stdin:         dispatcher.dependencies.Stdin,
		Stdout:        stdout,
		Stderr:        stderr,
		BeforeExec: func(ready app.ShellReady) error {
			return renderProcessStatus(stdout, app.ProcessStatusResult{URLs: ready.URLs, Processes: ready.Processes}, "text")
		},
	}, stderr)
}

func (dispatcher *Dispatcher) runShell(ctx context.Context, request app.ShellRequest, stderr io.Writer) int {
	if dispatcher == nil || dispatcher.dependencies.Lifecycle == nil {
		return reportError(stderr, "dsx shell", model.NewError(model.CodeUnavailable, "lifecycle service is unavailable", nil))
	}
	if request.Terminal {
		request.RunInteractive = dispatcher.runInteractive
	}
	result, err := dispatcher.dependencies.Lifecycle.Shell(ctx, request)
	if err != nil {
		return reportError(stderr, "dsx shell", err)
	}
	exit, err := shellExitCode(result)
	if err != nil {
		return reportError(stderr, "dsx shell", err)
	}
	return exit
}

func (dispatcher *Dispatcher) runHarness(ctx context.Context, request app.HarnessRunRequest, stderr io.Writer) int {
	if dispatcher == nil || dispatcher.dependencies.Harness == nil {
		return reportError(stderr, "dsx shell", model.NewError(model.CodeUnavailable, "harness service is unavailable", nil))
	}
	if request.Interactive {
		request.RunInteractive = dispatcher.runInteractive
		beforeExec := request.BeforeExec
		if beforeExec == nil {
			beforeExec = func(result app.HarnessRunResult) error {
				return renderHarnessIdentity(request.Stdout, result.Agent, result.Version)
			}
		}
		request.BeforeExec = func(result app.HarnessRunResult) error {
			warningOutput := request.Stderr
			if warningOutput == nil {
				warningOutput = request.Stdout
			}
			if _, err := fmt.Fprintln(warningOutput, terminal.SanitizeLine("Warning: concurrent editing harnesses in one live workspace may conflict or corrupt work.")); err != nil {
				return model.Wrap(model.CodeInternal, "write live harness warning", err)
			}
			return beforeExec(result)
		}
	} else if request.BeforeExec == nil {
		request.BeforeExec = func(result app.HarnessRunResult) error {
			return renderHarnessIdentity(request.Stdout, result.Agent, result.Version)
		}
	}
	result, err := dispatcher.dependencies.Harness.Run(ctx, request)
	if err != nil {
		return reportError(stderr, "dsx shell", err)
	}
	exit, err := shellExitCode(app.ShellResult{Exit: result.Exit})
	if err != nil {
		return reportError(stderr, "dsx shell", err)
	}
	return exit
}
func renderHarnessIdentity(writer io.Writer, agent harness.Name, version string) error {
	_, err := fmt.Fprintf(writer, "Agent: %q\nVersion: %q\n", terminal.SanitizeLine(string(agent)), terminal.SanitizeLine(version))
	if err != nil {
		return model.Wrap(model.CodeInternal, "write harness status", err)
	}
	return nil
}

func claimInteractiveSignals(ctx context.Context) (context.Context, <-chan os.Signal, func()) {
	if claimedCtx, signals, claimed := terminal.ClaimInteractiveSignalOwnership(ctx); claimed {
		return claimedCtx, signals, func() {}
	}
	signals, stopSignals := terminal.WatchSignals()
	return ctx, signals, stopSignals
}

func (dispatcher *Dispatcher) runInteractive(ctx context.Context, child app.InteractiveChild) (runtime.Exit, error) {
	if len(child.Argv) == 0 || child.Argv[0] == "" {
		return runtime.Exit{}, model.NewError(model.CodeInternal, "interactive child executable is missing", nil)
	}
	command := exec.Command(child.Argv[0], child.Argv[1:]...)
	command.Env = make([]string, len(child.Env))
	copy(command.Env, child.Env)
	command.Dir = child.Dir
	output := child.Stdout
	if output == nil {
		output = child.Stderr
	}
	initial, resize, stopResize := terminal.WatchResize(child.Stdin, output)
	defer stopResize()
	handoffCtx, signals, stopSignals := claimInteractiveSignals(ctx)
	defer stopSignals()
	exit, err := (terminal.Handoff{
		Input:       child.Stdin,
		Output:      output,
		State:       dispatcher.dependencies.TerminalState,
		InitialSize: initial,
		Resize:      resize,
		Signals:     signals,
	}).Run(handoffCtx, command)
	result := runtime.Exit{}
	if exit.Signal != 0 {
		result.Signal = signalName(exit.Signal)
	} else if exit.ExitCode >= 0 {
		code := exit.ExitCode
		result.Code = &code
	}
	return result, err
}

func (dispatcher *Dispatcher) executeIntent(ctx context.Context, intent tui.Intent, stdout, stderr io.Writer) int {
	root := intent.Project
	if err := validateRoot(root); err != nil {
		return reportError(stderr, "dsx "+intent.Action, err)
	}
	if strings.HasPrefix(intent.Action, "git-") {
		return dispatcher.executeGitIntent(ctx, intent, stdout, stderr)
	}
	if intent.Action == "clone-run" {
		return dispatcher.executeCloneRunIntent(ctx, intent, stdout, stderr)
	}
	if dispatcher == nil || dispatcher.dependencies.Lifecycle == nil {
		return reportError(stderr, "dsx "+intent.Action, model.NewError(model.CodeUnavailable, fmt.Sprintf("%s is not available in this build", intent.Action), nil))
	}
	switch intent.Action {
	case "create", "start", "attach":
		return dispatcher.runShell(ctx, app.ShellRequest{
			Root: root, Terminal: true, Stdin: dispatcher.dependencies.Stdin, Stdout: stdout, Stderr: stderr,
		}, stderr)
	case "stop":
		result, err := dispatcher.dependencies.Lifecycle.Stop(ctx, app.StopRequest{Root: root, Sandbox: intent.Sandbox})
		if err != nil {
			return reportError(stderr, "dsx stop", err)
		}
		if err := renderStop(stdout, result); err != nil {
			return reportError(stderr, "dsx stop", err)
		}
		return 0
	case "clean":
		result, err := dispatcher.dependencies.Lifecycle.Clean(ctx, app.CleanRequest{Root: root, Confirmed: true})
		if err != nil {
			return reportError(stderr, "dsx clean", err)
		}
		if err := renderClean(stdout, result); err != nil {
			return reportError(stderr, "dsx clean", err)
		}
		return 0
	default:
		return reportError(stderr, "dsx", model.NewError(model.CodeUnavailable, fmt.Sprintf("%s is not available in this build", intent.Action), nil))
	}
}

func (dispatcher *Dispatcher) executeGitIntent(ctx context.Context, intent tui.Intent, stdout, stderr io.Writer) int {
	operation := strings.TrimPrefix(intent.Action, "git-")
	command := "dsx git " + operation
	sandbox, err := model.ParseSandboxName(intent.Sandbox)
	if err != nil {
		return reportError(stderr, command, model.NewError(model.CodeUsage, "select a valid named clone sandbox", err))
	}
	if sandbox == "main" {
		return reportError(stderr, command, model.NewError(model.CodeUnavailable, "Git "+operation+" is unavailable for the live sandbox; select a named clone sandbox", nil))
	}
	if dispatcher == nil || dispatcher.dependencies.Clones == nil {
		return reportError(stderr, command, model.NewError(model.CodeUnavailable, "clone service is unavailable", nil))
	}
	switch operation {
	case "status":
		result, operationErr := dispatcher.dependencies.Clones.GitStatus(ctx, app.GitStatusRequest{
			Root: intent.Project, Sandbox: intent.Sandbox, Repository: intent.Repository,
		})
		if operationErr != nil {
			return reportError(stderr, command, operationErr)
		}
		if renderErr := renderGitStatus(stdout, result, "text"); renderErr != nil {
			return reportError(stderr, command, renderErr)
		}
	case "diff":
		result, operationErr := dispatcher.dependencies.Clones.GitDiff(ctx, app.GitDiffRequest{
			Root: intent.Project, Sandbox: intent.Sandbox, Repository: intent.Repository, MaxBytes: maxGitDiffBytes,
		})
		if operationErr != nil {
			return reportError(stderr, command, operationErr)
		}
		if renderErr := renderGitDiff(stdout, result, "text"); renderErr != nil {
			return reportError(stderr, command, renderErr)
		}
	case "fetch":
		result, operationErr := dispatcher.dependencies.Clones.GitFetch(ctx, app.GitFetchRequest{
			Root: intent.Project, Sandbox: intent.Sandbox, Repository: intent.Repository,
		})
		if operationErr != nil {
			return reportError(stderr, command, operationErr)
		}
		if renderErr := renderGitFetch(stdout, result, "text"); renderErr != nil {
			return reportError(stderr, command, renderErr)
		}
	default:
		return reportError(stderr, command, model.NewError(model.CodeUnavailable, fmt.Sprintf("%s is not available in this build", intent.Action), nil))
	}
	return 0
}

func (dispatcher *Dispatcher) executeCloneRunIntent(ctx context.Context, intent tui.Intent, stdout, stderr io.Writer) int {
	const command = "dsx run"
	if dispatcher == nil || dispatcher.dependencies.Clones == nil {
		return reportError(stderr, command, model.NewError(model.CodeUnavailable, "clone service is unavailable", nil))
	}
	if !validApprovalHash(intent.ApproveConfig) {
		return reportError(stderr, command, model.NewError(model.CodeUsage, "reviewed clone plan has an invalid approval hash", nil))
	}
	result, err := dispatcher.dependencies.Clones.RunClone(ctx, app.CloneRunRequest{
		Root: intent.Project, ApproveConfig: intent.ApproveConfig, Sandbox: intent.Sandbox,
		Agent: intent.Agent, Profile: intent.Profile, Prompt: intent.Prompt, Browser: intent.Browser,
		Stdin: dispatcher.dependencies.Stdin, Stdout: stdout, Stderr: stderr,
	})
	if err != nil {
		return reportError(stderr, command, err)
	}
	exit, err := runtimeExitCode(result.Exit, "clone run")
	if err != nil {
		return reportError(stderr, command, err)
	}
	return exit
}

func validApprovalHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := range value {
		if (value[index] < '0' || value[index] > '9') && (value[index] < 'a' || value[index] > 'f') {
			return false
		}
	}
	return true
}

func validateRoot(root string) error {
	if strings.TrimSpace(root) == "" {
		return model.NewError(model.CodeUsage, "--root must not be empty", nil)
	}
	return nil
}

func argumentsFollowSeparator(args, positional []string) bool {
	separator := len(args) - len(positional) - 1
	if separator < 0 || separator >= len(args) || args[separator] != "--" {
		return false
	}
	for index := range positional {
		if positional[index] != args[separator+1+index] {
			return false
		}
	}
	return true
}

func shellExitCode(result app.ShellResult) (int, error) {
	if result.Exit.Signal != "" {
		if result.Exit.Code != nil {
			return 0, model.NewError(model.CodeInternal, "shell returned both an exit code and a signal", nil)
		}
		signal, found := shellSignals[strings.ToUpper(result.Exit.Signal)]
		if !found {
			return 0, model.NewError(model.CodeInternal, fmt.Sprintf("shell returned unknown signal %q", result.Exit.Signal), nil)
		}
		return 128 + int(signal), nil
	}
	if result.Exit.Code == nil || *result.Exit.Code < 0 || *result.Exit.Code > 255 {
		return 0, model.NewError(model.CodeInternal, "shell returned no valid exit status", nil)
	}
	return *result.Exit.Code, nil
}

var shellSignals = map[string]syscall.Signal{
	"SIGHUP":    syscall.SIGHUP,
	"SIGINT":    syscall.SIGINT,
	"SIGQUIT":   syscall.SIGQUIT,
	"SIGILL":    syscall.SIGILL,
	"SIGTRAP":   syscall.SIGTRAP,
	"SIGABRT":   syscall.SIGABRT,
	"SIGBUS":    syscall.SIGBUS,
	"SIGFPE":    syscall.SIGFPE,
	"SIGKILL":   syscall.SIGKILL,
	"SIGUSR1":   syscall.SIGUSR1,
	"SIGSEGV":   syscall.SIGSEGV,
	"SIGUSR2":   syscall.SIGUSR2,
	"SIGPIPE":   syscall.SIGPIPE,
	"SIGALRM":   syscall.SIGALRM,
	"SIGTERM":   syscall.SIGTERM,
	"SIGCHLD":   syscall.SIGCHLD,
	"SIGCONT":   syscall.SIGCONT,
	"SIGSTOP":   syscall.SIGSTOP,
	"SIGTSTP":   syscall.SIGTSTP,
	"SIGTTIN":   syscall.SIGTTIN,
	"SIGTTOU":   syscall.SIGTTOU,
	"SIGURG":    syscall.SIGURG,
	"SIGXCPU":   syscall.SIGXCPU,
	"SIGXFSZ":   syscall.SIGXFSZ,
	"SIGVTALRM": syscall.SIGVTALRM,
	"SIGPROF":   syscall.SIGPROF,
	"SIGWINCH":  syscall.SIGWINCH,
	"SIGIO":     syscall.SIGIO,
	"SIGSYS":    syscall.SIGSYS,
}

func signalName(signal syscall.Signal) string {
	for name, candidate := range shellSignals {
		if candidate == signal {
			return name
		}
	}
	return fmt.Sprintf("SIG%d", signal)
}
