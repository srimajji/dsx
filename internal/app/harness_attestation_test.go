package app

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
)

func TestAgentRejectsWrongRuntimeImageBeforeReadingAttestation(t *testing.T) {
	service := &AgentService{agentImageReference: fixtureAgentImageReference}
	execution := plan.ExecutionPlan{
		Agents: plan.AgentPlan{Allowed: []string{"codex"}, Default: "codex"},
		Image:  plan.ResolvedImage{Reference: fixtureAgentImageReference, InputDigest: strings.Repeat("a", 64)},
	}
	read := false
	err := service.verifyHarnessBuildAttestation(
		context.Background(),
		runtime.ResourceSnapshot{ImageDigest: "sha256:" + strings.Repeat("b", 64)},
		execution,
		fakeHarnessAdapter{},
		func(io.Writer, io.Writer) (runtime.Exit, error) {
			read = true
			return runtime.Exit{}, nil
		},
	)
	if model.ErrorCodeOf(err) != model.CodeUnavailable || read {
		t.Fatalf("verifyHarnessBuildAttestation() error = %v, read=%t; want unavailable before read", err, read)
	}
}
