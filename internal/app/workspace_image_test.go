package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentimage "github.com/srimajji/dsx/images/agent"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

func TestPrepareStandardImageBuildsApprovedEmbeddedContextAndCleansStage(t *testing.T) {
	fixture := newWorkspaceFixture(t)
	capture := &standardImageRuntime{recordingWorkspaceRuntime: fixture.runtime}
	fixture.service.runtime = capture
	fixture.execution.Image.Reference = ""
	fixture.execution.Image.Standard = true
	fixture.execution.Image.Context = "@dsx/standard"
	fixture.execution.Image.File = agentimage.BuildFile
	fixture.execution.Image.InputDigest = agentimage.InputDigest()

	if err := fixture.service.PrepareStandardImage(context.Background(), *fixture.execution); err != nil {
		t.Fatal(err)
	}
	if len(capture.specs) != 1 {
		t.Fatalf("image specs = %d, want 1", len(capture.specs))
	}
	spec := capture.specs[0]
	if want := "dsx.local/standard:" + agentimage.InputDigest()[:12]; spec.Reference != want {
		t.Fatalf("image reference = %q, want %q", spec.Reference, want)
	}
	if spec.Context == "" || spec.File != runtime.HostPath(filepath.Join(string(spec.Context), agentimage.BuildFile)) {
		t.Fatalf("staged build paths = context %q file %q", spec.Context, spec.File)
	}
	if strings.HasPrefix(string(spec.Context), fixture.root+string(os.PathSeparator)) {
		t.Fatalf("standard image staged inside project: %q", spec.Context)
	}
	if len(spec.Labels) != 1 || spec.Labels[0].Key != "dev.dsx.standard-input" || spec.Labels[0].Value != agentimage.InputDigest() {
		t.Fatalf("standard image labels = %#v", spec.Labels)
	}
	if _, err := os.Stat(string(spec.Context)); !os.IsNotExist(err) {
		t.Fatalf("build stage was not removed: %v", err)
	}

	fixture.execution.Image.InputDigest = strings.Repeat("0", 64)
	if err := fixture.service.PrepareStandardImage(context.Background(), *fixture.execution); model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("mismatched authority error = %v", err)
	}
	if len(capture.specs) != 1 {
		t.Fatalf("mismatched authority reached runtime: %d specs", len(capture.specs))
	}
}

type standardImageRuntime struct {
	*recordingWorkspaceRuntime
	specs []runtime.ImageSpec
}

func (runtimeAdapter *standardImageRuntime) EnsureImage(_ context.Context, spec runtime.ImageSpec) (runtime.Image, error) {
	runtimeAdapter.specs = append(runtimeAdapter.specs, spec)
	return runtime.Image{Reference: spec.Reference, Digest: strings.Repeat("a", 64), Local: true}, nil
}
