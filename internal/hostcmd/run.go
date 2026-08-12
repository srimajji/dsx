package hostcmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/terminal"
)

const agentHelp = `Usage: dsx agent WORKSPACE [--root PATH] [--agent omp|codex|claude|opencode] [--browser] [-- PROMPT]
`

func (dispatcher *Dispatcher) executeAgent(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "dsx agent", "agent requires a workspace name")
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		if len(args) != 1 {
			return usageError(stderr, "dsx agent", "agent help does not accept arguments")
		}
		if _, err := io.WriteString(stdout, agentHelp); err != nil {
			return reportError(stderr, "dsx agent", model.Wrap(model.CodeInternal, "write help", err))
		}
		return 0
	}
	workspace, err := model.ParseWorkspaceName(args[0])
	if err != nil {
		return usageError(stderr, "dsx agent", err.Error())
	}
	flags := newFlagSet("agent")
	root := flags.String("root", ".", "project root")
	agent := flags.String("agent", "", "approved agent override")
	browser := flags.Bool("browser", false, "enable a disposable isolated browser for this session")
	remaining := args[1:]
	if exit, done := parseFlags(flags, remaining, stdout, stderr, agentHelp); done {
		return exit
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx agent", err)
	}
	if *agent != "" {
		if _, err := harness.ParseName(*agent); err != nil {
			return usageError(stderr, "dsx agent", err.Error())
		}
	}
	prompt := ""
	switch flags.NArg() {
	case 0:
		if containsSeparator(remaining) {
			return usageError(stderr, "dsx agent", "-- must be followed by exactly one prompt")
		}
		if !dispatcher.interactive(stdout) {
			return usageError(stderr, "dsx agent", "an agent without a prompt requires an interactive terminal")
		}
	case 1:
		if !argumentsFollowSeparator(remaining, flags.Args()) {
			return usageError(stderr, "dsx agent", "prompt must follow --")
		}
		prompt = flags.Arg(0)
		if strings.TrimSpace(prompt) == "" {
			return usageError(stderr, "dsx agent", "prompt must not be empty")
		}
	default:
		return usageError(stderr, "dsx agent", "agent accepts exactly one prompt after --")
	}
	if dispatcher == nil || dispatcher.dependencies.Agents == nil {
		return reportError(stderr, "dsx agent", model.NewError(model.CodeUnavailable, "agent service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Agents.Run(ctx, app.AgentRunRequest{
		Root:           *root,
		Workspace:      string(workspace),
		Agent:          *agent,
		Browser:        *browser,
		Prompt:         prompt,
		Stdin:          dispatcher.dependencies.Stdin,
		Stdout:         stdout,
		Stderr:         stderr,
		RunInteractive: dispatcher.runInteractive,
		BeforeExec: func(result app.AgentRunResult) error {
			return renderAgentIdentity(stdout, result.Agent, result.Version)
		},
	})
	if err != nil {
		return reportError(stderr, "dsx agent", err)
	}
	exit, err := runtimeExitCode(result.Exit, "agent")
	if err != nil {
		return reportError(stderr, "dsx agent", err)
	}
	return exit
}

func containsSeparator(args []string) bool {
	for _, argument := range args {
		if argument == "--" {
			return true
		}
	}
	return false
}

func renderAgentIdentity(writer io.Writer, agent harness.Name, version string) error {
	if _, err := fmt.Fprintf(writer, "Agent: %q\nVersion: %q\n", terminal.SanitizeLine(string(agent)), terminal.SanitizeLine(version)); err != nil {
		return model.Wrap(model.CodeInternal, "write agent status", err)
	}
	return nil
}
