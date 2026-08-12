package hostcmd

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
)

func TestSanitizeExplicitCLIRendering(t *testing.T) {
	hostile := "value\x1b[2J\x1b]0;title\a\x1b]52;c;Y2xpcA==\a\u202espoof"

	var inspect bytes.Buffer
	if err := renderInspect(&inspect, app.InspectResult{
		Facts: app.ProjectFacts{CanonicalRoot: hostile, ConfigPath: hostile, ConfigExists: true},
		Plan: plan.ExecutionPlan{
			ContractVersion: plan.ContractVersion,
			Agents:          plan.AgentPlan{Default: hostile},
			Image:           plan.ResolvedImage{Reference: hostile},
			ExecutableHash:  hostile,
		},
	}, "text"); err != nil {
		t.Fatalf("renderInspect() error = %v", err)
	}
	assertCLITerminalSafe(t, inspect.String())
	if !strings.Contains(inspect.String(), `\x1b[2J`) || !strings.Contains(inspect.String(), `\u202e`) {
		t.Fatalf("inspect did not visibly escape hostile fields: %q", inspect.String())
	}

	var doctor bytes.Buffer
	if err := renderDoctor(&doctor, app.DoctorResult{Capabilities: runtime.Capabilities{
		HostOS: hostile, HostVersion: hostile, HostArch: hostile, CLIVersion: hostile, ServerVersion: hostile, CompatibilityID: hostile,
	}}, "text"); err != nil {
		t.Fatalf("renderDoctor() error = %v", err)
	}
	assertCLITerminalSafe(t, doctor.String())

	var diagnostics bytes.Buffer
	if err := renderDiagnostics(&diagnostics, []config.Diagnostic{{Severity: hostile, Code: hostile, Path: hostile, Message: hostile}}); err != nil {
		t.Fatalf("renderDiagnostics() error = %v", err)
	}
	assertCLITerminalSafe(t, diagnostics.String())

	var stdout, stderr bytes.Buffer
	exit := Execute(context.Background(), []string{hostile}, &stdout, &stderr)
	if exit != 2 {
		t.Fatalf("hostile command exit = %d, want 2", exit)
	}
	assertCLITerminalSafe(t, stderr.String())
}

func TestSanitizeCompleteInspectPlanTail(t *testing.T) {
	const tailGrant = "tail-inspect-grant.example.internal"
	bridges := make([]plan.BridgeGrant, 0, 301)
	for index := range 300 {
		bridges = append(bridges, plan.BridgeGrant{
			Kind:        "tcp",
			Name:        fmt.Sprintf("grant-%03d-%s", index, strings.Repeat("x", 40)),
			Destination: fmt.Sprintf("service-%03d.example.internal", index),
			Port:        443,
		})
	}
	bridges = append(bridges, plan.BridgeGrant{Kind: "tcp", Name: "tail", Destination: tailGrant, Port: 8443})
	var output bytes.Buffer
	err := renderInspect(&output, app.InspectResult{Plan: plan.ExecutionPlan{
		ContractVersion: plan.ContractVersion,
		Bridges:         bridges,
	}}, "text")
	if err != nil {
		t.Fatalf("render complete inspect plan: %v", err)
	}
	if output.Len() <= 16*1024 || !strings.Contains(output.String(), tailGrant) {
		t.Fatalf("inspect output truncated tail grant: length=%d", output.Len())
	}
	assertCLITerminalSafe(t, output.String())
}

func assertCLITerminalSafe(t *testing.T, value string) {
	t.Helper()
	for _, forbidden := range []string{"\x1b", "\a", "\r", "\u202e", "\u2066", "\u2069"} {
		if strings.Contains(value, forbidden) {
			t.Fatalf("CLI output contains raw terminal control %q: %q", forbidden, value)
		}
	}
}
