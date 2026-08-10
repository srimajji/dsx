package app

import (
	"context"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
)

func TestCloneHarnessRejectsWrongRuntimeImageBeforeInvocationState(t *testing.T) {
	harnessService := &HarnessService{
		adapters:            map[harness.Name]harness.Adapter{harness.Codex: fakeHarnessAdapter{}},
		agentImageReference: fixtureAgentImageReference,
	}
	service := &CloneService{harness: harnessService}
	execution := plan.ExecutionPlan{
		Agent: "codex",
		Image: plan.ResolvedImage{Reference: fixtureAgentImageReference, InputDigest: strings.Repeat("a", 64)},
		Auth:  []plan.ResolvedAuthGrant{{Harness: "codex", Profile: "default", Persistence: "global"}},
	}
	_, err := service.runHarness(
		context.Background(),
		runtime.ResourceSnapshot{ImageDigest: "sha256:" + strings.Repeat("b", 64)},
		execution,
		CloneRunRequest{Agent: "codex"},
		nil,
	)
	if model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatalf("runHarness() error = %v (code %q), want unavailable image", err, model.ErrorCodeOf(err))
	}
}
