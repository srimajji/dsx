package hostcmd

import (
	"context"
	"flag"
	"io"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
)

const loginHelp = `Usage: dsx login --agent omp|codex|claude|opencode --profile NAME --root PATH --approve-config HASH

Starts an explicit interactive provider login in the approved sandbox. DSX never
logs in implicitly or without an interactive terminal.
`

func (dispatcher *Dispatcher) executeLogin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("login")
	agent := flags.String("agent", "", "agent harness")
	profile := flags.String("profile", "", "declared authentication profile")
	root := flags.String("root", "", "project root")
	approval := flags.String("approve-config", "", "exact executable configuration hash")
	if exit, done := parseFlags(flags, args, stdout, stderr, loginHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx login", "login does not accept positional arguments")
	}
	provided := make(map[string]bool, 4)
	flags.Visit(func(item *flag.Flag) { provided[item.Name] = true })
	for _, name := range []string{"agent", "profile", "root", "approve-config"} {
		if !provided[name] {
			return usageError(stderr, "dsx login", "--"+name+" is required")
		}
	}
	if _, err := harness.ParseName(*agent); err != nil {
		return usageError(stderr, "dsx login", err.Error())
	}
	if _, err := model.ParseSandboxName(*profile); err != nil {
		return usageError(stderr, "dsx login", err.Error())
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx login", err)
	}
	if !validApprovalHash(*approval) {
		return usageError(stderr, "dsx login", "--approve-config must be exactly 64 lowercase hexadecimal characters")
	}
	if !dispatcher.interactive(stdout) {
		return usageError(stderr, "dsx login", "login requires an interactive terminal")
	}
	if dispatcher == nil || dispatcher.dependencies.Harness == nil {
		return reportError(stderr, "dsx login", model.NewError(model.CodeUnavailable, "harness service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Harness.Login(ctx, app.HarnessLoginRequest{
		Root:           *root,
		ApproveConfig:  *approval,
		Agent:          *agent,
		Profile:        *profile,
		Interactive:    true,
		Stdin:          dispatcher.dependencies.Stdin,
		Stdout:         stdout,
		Stderr:         stderr,
		RunInteractive: dispatcher.runInteractive,
		BeforeExec: func(result app.HarnessLoginResult) error {
			return renderHarnessIdentity(stdout, result.Agent, result.Version)
		},
		OpenBrowser: dispatcher.dependencies.LoginBrowser,
	})
	if err != nil {
		return reportError(stderr, "dsx login", err)
	}
	exit, err := runtimeExitCode(result.Exit, "harness login")
	if err != nil {
		return reportError(stderr, "dsx login", err)
	}
	return exit
}
