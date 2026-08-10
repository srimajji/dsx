package hostcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/terminal"
)

const processStatusHelp = `Usage: dsx status [--root PATH] [--format text|json]
`

const processLogsHelp = `Usage: dsx logs [--root PATH] [--format text|json] PROCESS
`

func (dispatcher *Dispatcher) executeProcessStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("status")
	root := flags.String("root", ".", "project root")
	format := flags.String("format", "text", "output format: text or json")
	if exit, done := parseFlags(flags, args, stdout, stderr, processStatusHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx status", "status does not accept positional arguments")
	}
	if err := validateFormat(*format); err != nil {
		return reportError(stderr, "dsx status", err)
	}
	service, ok := dispatcher.dependencies.Lifecycle.(ProcessStatus)
	if !ok {
		return reportError(stderr, "dsx status", model.NewError(model.CodeUnavailable, "guest process status is unavailable", nil))
	}
	result, err := service.ProcessStatus(ctx, app.ProcessStatusRequest{Root: *root})
	if err != nil {
		return reportError(stderr, "dsx status", err)
	}
	if err := renderProcessStatus(stdout, result, *format); err != nil {
		return reportError(stderr, "dsx status", err)
	}
	return 0
}

func (dispatcher *Dispatcher) executeProcessLogs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("logs")
	root := flags.String("root", ".", "project root")
	format := flags.String("format", "text", "output format: text or json")
	if exit, done := parseFlags(flags, args, stdout, stderr, processLogsHelp); done {
		return exit
	}
	if flags.NArg() != 1 {
		return usageError(stderr, "dsx logs", "logs requires exactly one configured process")
	}
	if err := validateFormat(*format); err != nil {
		return reportError(stderr, "dsx logs", err)
	}
	service, ok := dispatcher.dependencies.Lifecycle.(ProcessLogs)
	if !ok {
		return reportError(stderr, "dsx logs", model.NewError(model.CodeUnavailable, "guest process logs are unavailable", nil))
	}
	result, err := service.ProcessLogs(ctx, app.ProcessLogsRequest{Root: *root, Target: flags.Arg(0)})
	if err != nil {
		return reportError(stderr, "dsx logs", err)
	}
	if *format == "json" {
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			return reportError(stderr, "dsx logs", model.Wrap(model.CodeInternal, "write logs JSON", err))
		}
		return 0
	}
	if _, err := fmt.Fprint(stdout, terminal.Sanitize(result.Log)); err != nil {
		return reportError(stderr, "dsx logs", model.Wrap(model.CodeInternal, "write logs", err))
	}
	return 0
}

func renderProcessStatus(writer io.Writer, result app.ProcessStatusResult, format string) error {
	if format == "json" {
		return json.NewEncoder(writer).Encode(result)
	}
	urls := append([]string(nil), result.URLs...)
	sort.Strings(urls)
	for _, url := range urls {
		if _, err := fmt.Fprintf(writer, "URL %s\n", terminal.SanitizeLine(url)); err != nil {
			return err
		}
	}
	warnings := append([]string(nil), result.Warnings...)
	sort.Strings(warnings)
	for _, warning := range warnings {
		if _, err := fmt.Fprintf(writer, "Warning: %s\n", terminal.SanitizeLine(warning)); err != nil {
			return err
		}
	}
	processes := append([]guestproto.ProcessStatus(nil), result.Processes...)
	sort.Slice(processes, func(i, j int) bool { return processes[i].ID < processes[j].ID })
	for _, process := range processes {
		if _, err := fmt.Fprintf(writer, "%s state=%s ready=%t required=%t", terminal.SanitizeLine(process.ID), process.State, process.Ready, process.Required); err != nil {
			return err
		}
		if process.Exit != nil {
			switch {
			case process.Exit.Code != nil:
				if _, err := fmt.Fprintf(writer, " exit_code=%d", *process.Exit.Code); err != nil {
					return err
				}
			case process.Exit.Signal != "":
				if _, err := fmt.Fprintf(writer, " signal=%s", terminal.SanitizeLine(process.Exit.Signal)); err != nil {
					return err
				}
			}
		}
		if process.Failure != "" {
			if _, err := fmt.Fprintf(writer, " failure=%s", terminal.SanitizeLine(process.Failure)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(writer); err != nil {
			return err
		}
	}
	return nil
}
