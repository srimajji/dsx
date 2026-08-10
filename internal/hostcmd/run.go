package hostcmd

import (
	"context"
	"io"
	"strings"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
)

const runHelp = `Usage: dsx run --name NAME --agent omp|codex|claude|opencode [--profile NAME] [--browser] --approve-config HASH -- PROMPT
`

func (dispatcher *Dispatcher) executeRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("run")
	name := flags.String("name", "", "named clone sandbox")
	agent := flags.String("agent", "", "agent harness")
	profile := flags.String("profile", "default", "persistent authentication profile")
	browser := flags.Bool("browser", false, "enable isolated Playwright browser")
	approval := flags.String("approve-config", "", "exact executable configuration hash")
	if exit, done := parseFlags(flags, args, stdout, stderr, runHelp); done {
		return exit
	}
	if flags.NArg() != 1 || !argumentsFollowSeparator(args, flags.Args()) {
		return usageError(stderr, "dsx run", "run requires exactly one prompt after --")
	}
	if _, err := model.ParseSandboxName(*name); err != nil {
		return usageError(stderr, "dsx run", err.Error())
	}
	if _, err := harness.ParseName(*agent); err != nil {
		return usageError(stderr, "dsx run", err.Error())
	}
	if _, err := model.ParseSandboxName(*profile); err != nil {
		return usageError(stderr, "dsx run", err.Error())
	}
	if !validApprovalHash(*approval) {
		return usageError(stderr, "dsx run", "--approve-config must be exactly 64 lowercase hexadecimal characters")
	}
	if strings.TrimSpace(flags.Arg(0)) == "" {
		return usageError(stderr, "dsx run", "prompt must not be empty")
	}
	if dispatcher == nil || dispatcher.dependencies.Clones == nil {
		return reportError(stderr, "dsx run", model.NewError(model.CodeUnavailable, "clone service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Clones.RunClone(ctx, app.CloneRunRequest{
		Root:          ".",
		ApproveConfig: *approval,
		Sandbox:       *name,
		Agent:         *agent,
		Profile:       *profile,
		Prompt:        flags.Arg(0),
		Browser:       *browser,
		Stdin:         dispatcher.dependencies.Stdin,
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err != nil {
		return reportError(stderr, "dsx run", err)
	}
	exit, err := runtimeExitCode(result.Exit, "clone run")
	if err != nil {
		return reportError(stderr, "dsx run", err)
	}
	return exit
}
