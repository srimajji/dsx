package hostcmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/terminal"
)

const authHelp = `Usage:
  dsx auth status [--root PATH] [--format text|json]
  dsx auth import --agent omp|codex|opencode [--root PATH]
  dsx auth login --agent omp|codex|claude|opencode [--root PATH]
  dsx auth refresh --agent omp|codex|opencode [--root PATH]
  dsx auth purge --agent omp|codex|claude|opencode [--root PATH] [--force]
`

func (dispatcher *Dispatcher) executeAuth(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "dsx auth", "auth requires status, import, login, refresh, or purge")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if len(args) != 1 {
			return usageError(stderr, "dsx auth", "auth help does not accept arguments")
		}
		if _, err := io.WriteString(stdout, authHelp); err != nil {
			return reportError(stderr, "dsx auth", model.Wrap(model.CodeInternal, "write help", err))
		}
		return 0
	}
	operation := args[0]
	switch operation {
	case "status":
		return dispatcher.executeAuthStatus(ctx, args[1:], stdout, stderr)
	case "import", "login", "refresh", "purge":
		return dispatcher.executeAuthMutation(ctx, operation, args[1:], stdout, stderr)
	default:
		return usageError(stderr, "dsx auth", fmt.Sprintf("unknown auth command %q", operation))
	}
}

func (dispatcher *Dispatcher) executeAuthStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("auth status")
	root := flags.String("root", ".", "project root")
	format := flags.String("format", "text", "output format: text or json")
	if exit, done := parseFlags(flags, args, stdout, stderr, authHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx auth status", "status does not accept positional arguments")
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx auth status", err)
	}
	if err := validateFormat(*format); err != nil {
		return reportError(stderr, "dsx auth status", err)
	}
	if dispatcher == nil || dispatcher.dependencies.Auth == nil {
		return reportError(stderr, "dsx auth status", model.NewError(model.CodeUnavailable, "authentication service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Auth.Status(ctx, app.AuthStatusRequest{Root: *root})
	if err != nil {
		return reportError(stderr, "dsx auth status", err)
	}
	if err := renderAuthStatus(stdout, result, *format); err != nil {
		return reportError(stderr, "dsx auth status", err)
	}
	return 0
}

func (dispatcher *Dispatcher) executeAuthMutation(ctx context.Context, operation string, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("auth " + operation)
	root := flags.String("root", ".", "project root")
	agent := flags.String("agent", "", "agent harness")
	var force *bool
	if operation == "purge" {
		force = flags.Bool("force", false, "confirm destructive credential removal")
	}
	if exit, done := parseFlags(flags, args, stdout, stderr, authHelp); done {
		return exit
	}
	command := "dsx auth " + operation
	if flags.NArg() != 0 {
		return usageError(stderr, command, operation+" does not accept positional arguments")
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, command, err)
	}
	parsedAgent, err := harness.ParseName(*agent)
	if err != nil {
		return usageError(stderr, command, err.Error())
	}
	if dispatcher == nil || dispatcher.dependencies.Auth == nil {
		return reportError(stderr, command, model.NewError(model.CodeUnavailable, "authentication service is unavailable", nil))
	}

	switch operation {
	case "import":
		result, err := dispatcher.dependencies.Auth.Import(ctx, app.AuthImportRequest{Root: *root, Agent: string(parsedAgent), Approved: true})
		if err != nil {
			return reportError(stderr, command, err)
		}
		return renderAuthMutation(stdout, stderr, command, result.Agent, result.Digest)
	case "refresh":
		result, err := dispatcher.dependencies.Auth.Refresh(ctx, app.AuthRefreshRequest{Root: *root, Agent: string(parsedAgent), Approved: true})
		if err != nil {
			return reportError(stderr, command, err)
		}
		return renderAuthMutation(stdout, stderr, command, result.Agent, result.Digest)
	case "login":
		if !dispatcher.interactive(stdout) {
			return usageError(stderr, command, "login requires an interactive terminal")
		}
		result, err := dispatcher.dependencies.Auth.Login(ctx, app.AuthLoginRequest{
			Root: *root, Agent: string(parsedAgent), Approved: true, Interactive: true,
			Stdin: dispatcher.dependencies.Stdin, Stdout: stdout, Stderr: stderr,
			RunInteractive: dispatcher.runInteractive,
		})
		if err != nil {
			return reportError(stderr, command, err)
		}
		if exit := renderAuthMutation(stdout, stderr, command, result.Agent, ""); exit != 0 {
			return exit
		}
		exit, err := runtimeExitCode(result.Exit, "authentication login")
		if err != nil {
			return reportError(stderr, command, err)
		}
		return exit
	case "purge":
		approved, exit := dispatcher.confirmDestructive(fmt.Sprintf("Purge %s project authentication? [y/N] ", parsedAgent), *force, stdout, stderr)
		if !approved {
			return exit
		}
		if err := dispatcher.dependencies.Auth.Purge(ctx, app.AuthPurgeRequest{Root: *root, Agent: string(parsedAgent), Approved: true}); err != nil {
			return reportError(stderr, command, err)
		}
		if _, err := fmt.Fprintf(stdout, "Purged authentication for %q.\n", terminal.SanitizeLine(string(parsedAgent))); err != nil {
			return reportError(stderr, command, model.Wrap(model.CodeInternal, "write authentication result", err))
		}
		return 0
	}
	return reportError(stderr, command, model.NewError(model.CodeInternal, "unreachable authentication operation", nil))
}

func (dispatcher *Dispatcher) confirmDestructive(prompt string, force bool, stdout, stderr io.Writer) (bool, int) {
	if force {
		return true, 0
	}
	if !dispatcher.interactive(stdout) {
		return false, usageError(stderr, "dsx", "destructive operation requires an interactive confirmation or --force")
	}
	if _, err := io.WriteString(stdout, prompt); err != nil {
		return false, reportError(stderr, "dsx", model.Wrap(model.CodeInternal, "write confirmation", err))
	}
	input := dispatcher.dependencies.Stdin
	if input == nil {
		return false, usageError(stderr, "dsx", "confirmation input is unavailable")
	}
	line, err := bufio.NewReader(io.LimitReader(input, 32)).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, reportError(stderr, "dsx", model.Wrap(model.CodeInternal, "read confirmation", err))
	}
	if strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes") {
		return true, 0
	}
	if _, err := io.WriteString(stdout, "Cancelled.\n"); err != nil {
		return false, reportError(stderr, "dsx", model.Wrap(model.CodeInternal, "write cancellation", err))
	}
	return false, 0
}

func renderAuthStatus(writer io.Writer, result app.AuthStatusResult, format string) error {
	result.Agents = append([]app.AuthAgentStatus(nil), result.Agents...)
	sort.SliceStable(result.Agents, func(i, j int) bool { return result.Agents[i].Agent < result.Agents[j].Agent })
	if format == "json" {
		return encodeJSON(writer, result)
	}
	if _, err := fmt.Fprintf(writer, "Project: %q\n", terminal.SanitizeLine(string(result.ProjectID))); err != nil {
		return model.Wrap(model.CodeInternal, "write authentication status", err)
	}
	for _, status := range result.Agents {
		if _, err := fmt.Fprintf(writer, "Agent %q: configured=%t host_import=%q\n", terminal.SanitizeLine(string(status.Agent)), status.Configured, terminal.SanitizeLine(fmt.Sprint(status.HostImport))); err != nil {
			return model.Wrap(model.CodeInternal, "write authentication status", err)
		}
	}
	return nil
}

func renderAuthMutation(stdout, stderr io.Writer, command string, agent harness.Name, digest string) int {
	if digest == "" {
		if _, err := fmt.Fprintf(stdout, "Authentication ready for %q.\n", terminal.SanitizeLine(string(agent))); err != nil {
			return reportError(stderr, command, model.Wrap(model.CodeInternal, "write authentication result", err))
		}
		return 0
	}
	if _, err := fmt.Fprintf(stdout, "Authentication ready for %q at digest %q.\n", terminal.SanitizeLine(string(agent)), terminal.SanitizeLine(digest)); err != nil {
		return reportError(stderr, command, model.Wrap(model.CodeInternal, "write authentication result", err))
	}
	return 0
}
