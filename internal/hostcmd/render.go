package hostcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/terminal"
)

const maxInspectPlanBytes = 512 * 1024

const versionHelp = `Usage: dsx version [--json]
`

const initHelp = `Usage: dsx init [--root PATH]

Opens the same reviewable setup flow used by bare dsx.
`

const inspectHelp = `Usage: dsx inspect [--format text|json] [--root PATH]

Reads project declarations and prints reusable project defaults without mutation.
`

const doctorHelp = `Usage: dsx doctor [--format text|json] [--require-builder]

Checks the supported macOS and Apple container runtime without mutation.
`

func renderInspect(writer io.Writer, result app.InspectResult, format string) error {
	if format == "json" {
		return encodeJSON(writer, result)
	}
	if _, err := fmt.Fprintf(writer, "Project: %q\nConfiguration: %q\nConfigured: %t\n", terminal.SanitizeLine(result.Facts.CanonicalRoot), terminal.SanitizeLine(result.Facts.ConfigPath), result.Facts.ConfigExists); err != nil {
		return model.Wrap(model.CodeInternal, "write inspect output", err)
	}
	if result.Plan.ContractVersion == "" {
		_, err := io.WriteString(writer, "Executable plan: unavailable until configuration is complete\n")
		return err
	}
	image := result.Plan.Image.Reference
	if result.Plan.Image.Standard {
		image = "DSX Standard — Ubuntu (local build)"
	} else if image == "" && result.Plan.Image.File != "" {
		image = "project build " + result.Plan.Image.File
	}
	if _, err := fmt.Fprintf(writer, "Default agent: %q\nImage: %q\nExecutable hash: %s\n", terminal.SanitizeLine(result.Plan.Agents.Default), terminal.SanitizeLine(image), terminal.SanitizeLine(result.Plan.ExecutableHash)); err != nil {
		return model.Wrap(model.CodeInternal, "write inspect output", err)
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("  ", "  ")
	if err := encoder.Encode(result.Plan); err != nil {
		return model.Wrap(model.CodeInternal, "write inspect plan", err)
	}
	sanitized := terminal.NewSanitizedBuilder(maxInspectPlanBytes)
	if !sanitized.WriteString(encoded.String()) || !sanitized.Complete() {
		return model.NewError(model.CodeInvalidInput, "complete inspect plan exceeds the safe display bound; output was refused rather than truncated", nil)
	}
	if _, err := io.WriteString(writer, "Plan:\n"); err != nil {
		return model.Wrap(model.CodeInternal, "write inspect output", err)
	}
	if _, err := io.WriteString(writer, sanitized.String()); err != nil {
		return model.Wrap(model.CodeInternal, "write inspect plan", err)
	}
	return nil
}

func renderDoctor(writer io.Writer, result app.DoctorResult, format string) error {
	if format == "json" {
		return encodeJSON(writer, result)
	}
	capabilities := result.Capabilities
	_, err := fmt.Fprintf(writer, "Host: %q %q %q\nApple container CLI: %q\nApple container server: %q\nCompatibility: %q\nService healthy: %t\nBuilder healthy: %t\n", terminal.SanitizeLine(capabilities.HostOS), terminal.SanitizeLine(capabilities.HostVersion), terminal.SanitizeLine(capabilities.HostArch), terminal.SanitizeLine(capabilities.CLIVersion), terminal.SanitizeLine(capabilities.ServerVersion), terminal.SanitizeLine(capabilities.CompatibilityID), capabilities.ServiceHealthy, capabilities.BuilderHealthy)
	if err != nil {
		return model.Wrap(model.CodeInternal, "write doctor output", err)
	}
	return nil
}

func renderDiagnostics(writer io.Writer, diagnostics []config.Diagnostic) error {
	ordered := append([]config.Diagnostic(nil), diagnostics...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.Severity != right.Severity {
			return left.Severity < right.Severity
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.Column != right.Column {
			return left.Column < right.Column
		}
		return left.Code < right.Code
	})
	for _, diagnostic := range ordered {
		if _, err := fmt.Fprintf(writer, "diagnostic severity=%q code=%q path=%q line=%d column=%d message=%q\n", terminal.SanitizeLine(diagnostic.Severity), terminal.SanitizeLine(diagnostic.Code), terminal.SanitizeLine(diagnostic.Path), diagnostic.Line, diagnostic.Column, terminal.SanitizeLine(diagnostic.Message)); err != nil {
			return model.Wrap(model.CodeInternal, "write diagnostics", err)
		}
	}
	return nil
}

func encodeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return model.Wrap(model.CodeInternal, "write JSON output", err)
	}
	return nil
}
