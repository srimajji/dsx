package hostcmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/buildinfo"
)

func TestVersion(t *testing.T) {
	previousVersion, previousCommit, previousBuiltAt := buildinfo.Version, buildinfo.Commit, buildinfo.BuiltAt
	buildinfo.Version, buildinfo.Commit, buildinfo.BuiltAt = "1.2.3", "abc123", "2026-08-09T00:00:00Z"
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.BuiltAt = previousVersion, previousCommit, previousBuiltAt
	})
	var stdout, stderr bytes.Buffer
	if exit := Execute(context.Background(), []string{"version"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dsx 1.2.3 (commit abc123") || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestHelpContainsOnlyCleanCutoverRoutes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := Execute(context.Background(), []string{"help"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	for _, command := range []string{
		"dsx workspace create NAME", "dsx workspace list", "dsx workspace open NAME",
		"dsx workspace start NAME", "dsx workspace stop NAME", "dsx workspace restart NAME",
		"dsx workspace update NAME", "dsx workspace remove NAME", "dsx agent WORKSPACE",
		"dsx auth status", "dsx auth import|login|refresh|purge", "dsx git status|diff|fetch|apply WORKSPACE",
	} {
		if !strings.Contains(stdout.String(), command) {
			t.Errorf("help missing %q: %q", command, stdout.String())
		}
	}
	for _, removed := range []string{"dsx shell", "dsx run", "\n  dsx start ", "\n  dsx stop ", "\n  dsx clean ", "\n  dsx list", "\n  dsx ls", "--mode live|clone", "--sandbox"} {
		if strings.Contains(stdout.String(), removed) {
			t.Errorf("help retains removed route %q: %q", removed, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRemovedTopLevelCommandsFailUsage(t *testing.T) {
	for _, command := range []string{"shell", "run", "start", "stop", "clean", "list", "ls", "login", "status", "logs"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exit := Execute(context.Background(), []string{command}, &stdout, &stderr); exit != 2 {
				t.Fatalf("exit = %d, want 2", exit)
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), "unknown command") {
				t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestUnknownCommandUsesUsageExitAndStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := Execute(context.Background(), []string{"missing"}, &stdout, &stderr); exit != 2 {
		t.Fatalf("exit = %d", exit)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), `dsx: unknown command "missing"`) {
		t.Fatalf("stdout = %q stderr = %q", stdout.String(), stderr.String())
	}
}
