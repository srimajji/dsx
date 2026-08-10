package apple

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRunnerStructuredArguments(t *testing.T) {
	result, err := (OSRunner{}).Run(context.Background(), Command{
		Executable: "/bin/echo",
		Args:       []string{"$(touch /tmp/dsx-runner-must-not-exist)", "a b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(result.Stdout), "$(touch /tmp/dsx-runner-must-not-exist) a b\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d", result.ExitCode)
	}
}
func TestRunnerDeniesAmbientEnvironment(t *testing.T) {
	t.Setenv("DSX_PARENT_SECRET", "must-not-leak")
	for _, environment := range [][]string{nil, {}} {
		result, err := (OSRunner{}).Run(context.Background(), Command{
			Executable: "/usr/bin/env",
			Env:        environment,
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(result.Stdout), "DSX_PARENT_SECRET") {
			t.Fatalf("ambient environment leaked: %q", result.Stdout)
		}
	}

	result, err := (OSRunner{}).Run(context.Background(), Command{
		Executable: "/usr/bin/env",
		Env:        []string{"DSX_EXPLICIT=value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(result.Stdout), "DSX_EXPLICIT=value\n"; got != want {
		t.Fatalf("environment = %q, want %q", got, want)
	}
}

func TestRunnerCapturesFailure(t *testing.T) {
	result, err := (OSRunner{}).Run(context.Background(), Command{
		Executable: "/bin/sh",
		Args:       []string{"-c", "printf failure >&2; exit 7"},
	})
	if err == nil {
		t.Fatal("expected process failure")
	}
	if result.ExitCode != 7 || string(result.Stderr) != "failure" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunnerHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := (OSRunner{}).Run(ctx, Command{Executable: "/bin/sleep", Args: []string{"5"}})
	if err == nil || result.ExitCode == 0 {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if !strings.Contains(err.Error(), "signal: killed") && !strings.Contains(err.Error(), "context deadline") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunnerStreaming(t *testing.T) {
	var stdout, stderr bytes.Buffer
	result, err := (OSRunner{}).Run(context.Background(), Command{
		Executable: "/bin/sh",
		Args:       []string{"-c", "printf streamed; printf diagnostic >&2"},
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "streamed" || stderr.String() != "diagnostic" ||
		string(result.Stdout) != "streamed" || string(result.Stderr) != "diagnostic" {
		t.Fatalf("stream/capture mismatch: %#v %q %q", result, stdout.String(), stderr.String())
	}
}

func TestRunnerBoundsCapturedOutputWhileStreamingAllBytes(t *testing.T) {
	var streamed countingWriter
	result, err := (OSRunner{}).Run(context.Background(), Command{
		Executable: "/bin/dd",
		Args:       []string{"if=/dev/zero", "bs=1048576", "count=5"},
		Stdout:     &streamed,
		Stderr:     io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if streamed.bytes != 5*1024*1024 {
		t.Fatalf("streamed bytes = %d", streamed.bytes)
	}
	if len(result.Stdout) != maxCapturedOutputBytes || !result.StdoutTruncated {
		t.Fatalf("capture bytes = %d, truncated = %v", len(result.Stdout), result.StdoutTruncated)
	}
}

type countingWriter struct{ bytes int }

func (writer *countingWriter) Write(value []byte) (int, error) {
	writer.bytes += len(value)
	return len(value), nil
}
