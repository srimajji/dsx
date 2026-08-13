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
		"dsx auth status", "dsx auth import|login|refresh|purge", "dsx aws enable WORKSPACE",
		"dsx aws disable WORKSPACE", "dsx aws status WORKSPACE", "dsx git status|diff|fetch|apply WORKSPACE",
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

type awsWorkspaceManagerStub struct {
	calls  []awsWorkspaceCall
	result AWSWorkspaceResult
	err    error
}

type awsWorkspaceCall struct {
	operation string
	request   AWSWorkspaceRequest
}

func (stub *awsWorkspaceManagerStub) Enable(_ context.Context, request AWSWorkspaceRequest) (AWSWorkspaceResult, error) {
	stub.calls = append(stub.calls, awsWorkspaceCall{operation: "enable", request: request})
	return stub.result, stub.err
}

func (stub *awsWorkspaceManagerStub) Disable(_ context.Context, request AWSWorkspaceRequest) (AWSWorkspaceResult, error) {
	stub.calls = append(stub.calls, awsWorkspaceCall{operation: "disable", request: request})
	result := stub.result
	result.Enabled = false
	return result, stub.err
}

func (stub *awsWorkspaceManagerStub) Status(_ context.Context, request AWSWorkspaceRequest) (AWSWorkspaceResult, error) {
	stub.calls = append(stub.calls, awsWorkspaceCall{operation: "status", request: request})
	return stub.result, stub.err
}

func TestAWSWorkspaceRoutes(t *testing.T) {
	manager := &awsWorkspaceManagerStub{result: AWSWorkspaceResult{
		Workspace:        "feature-a",
		Enabled:          true,
		HostAvailability: "available",
		MirrorHealth:     "healthy",
	}}
	dispatcher := NewDispatcher(Dependencies{AWS: manager})
	cases := []struct {
		name      string
		args      []string
		operation string
		want      string
	}{
		{
			name:      "enable",
			args:      []string{"aws", "enable", "feature-a", "--root", "/project"},
			operation: "enable",
			want:      "Workspace: \"feature-a\"\nEnabled: true\nHost availability: \"available\"\nMirror health: \"healthy\"\nFailure code: \"\"\n",
		},
		{
			name:      "disable",
			args:      []string{"aws", "disable", "feature-a", "--root", "/project"},
			operation: "disable",
			want:      "Workspace: \"feature-a\"\nEnabled: false\nHost availability: \"available\"\nMirror health: \"healthy\"\nFailure code: \"\"\n",
		},
		{
			name:      "status text",
			args:      []string{"aws", "status", "feature-a", "--root", "/project", "--format", "text"},
			operation: "status",
			want:      "Workspace: \"feature-a\"\nEnabled: true\nHost availability: \"available\"\nMirror health: \"healthy\"\nFailure code: \"\"\n",
		},
		{
			name:      "status json",
			args:      []string{"aws", "status", "feature-a", "--root", "/project", "--format", "json"},
			operation: "status",
			want:      "{\"workspace\":\"feature-a\",\"enabled\":true,\"host_availability\":\"available\",\"mirror_health\":\"healthy\",\"failure_code\":\"\"}\n",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exit := dispatcher.Execute(context.Background(), test.args, &stdout, &stderr); exit != 0 {
				t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
			}
			if stdout.String() != test.want || stderr.Len() != 0 {
				t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
			call := manager.calls[len(manager.calls)-1]
			if call.operation != test.operation || call.request.Root != "/project" || call.request.Workspace != "feature-a" {
				t.Fatalf("call = %#v", call)
			}
		})
	}
}

func TestAWSWorkspaceParserRejectsInvalidCommandsBeforeCallingManager(t *testing.T) {
	manager := &awsWorkspaceManagerStub{}
	dispatcher := NewDispatcher(Dependencies{AWS: manager})
	cases := [][]string{
		{"aws"},
		{"aws", "missing"},
		{"aws", "enable"},
		{"aws", "disable", "feature-a", "extra"},
		{"aws", "enable", "feature-a", "--format", "json"},
		{"aws", "status", "feature-a", "--format", "yaml"},
		{"aws", "status", "INVALID"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 2 {
			t.Fatalf("Execute(%q) exit = %d, stderr = %q", args, exit, stderr.String())
		}
		if stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("Execute(%q) stdout = %q, stderr = %q", args, stdout.String(), stderr.String())
		}
	}
	if len(manager.calls) != 0 {
		t.Fatalf("manager calls = %#v", manager.calls)
	}
}

func TestAWSWorkspaceExecutionWithoutManagerIsUnavailable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := NewDispatcher(Dependencies{}).Execute(
		context.Background(),
		[]string{"aws", "status", "feature-a"},
		&stdout,
		&stderr,
	)
	if exit != 4 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "AWS workspace service is unavailable") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}
