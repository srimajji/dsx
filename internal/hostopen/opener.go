package hostopen

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
)

const macOSOpenExecutable = "/usr/bin/open"

// Runner executes one structured argv without a host shell.
type Runner interface {
	Run(context.Context, []string) error
}

// OSRunner invokes host commands directly.
type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return errors.New("host opener executable is required")
	}
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
}

// Opener leases the fixed macOS URL opener to a timeout-scoped caller.
type Opener struct {
	runner Runner
}

func New(runner Runner) (*Opener, error) {
	if runner == nil {
		return nil, errors.New("host opener runner is required")
	}
	return &Opener{runner: runner}, nil
}

func (opener *Opener) Open(ctx context.Context, providerURL string) error {
	if opener == nil || opener.runner == nil {
		return errors.New("host opener is unavailable")
	}
	if ctx == nil {
		return errors.New("host opener context is required")
	}
	parsed, err := url.Parse(providerURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("host opener requires an absolute HTTPS URL without user information or a fragment")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := opener.runner.Run(ctx, []string{macOSOpenExecutable, providerURL}); err != nil {
		return fmt.Errorf("run macOS URL opener: %w", err)
	}
	return nil
}
