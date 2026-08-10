package gitx

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOSRunnerUsesExactArgvAndBoundsOutput(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "shell-expanded")
	var output bytes.Buffer
	exit, err := (OSRunner{MaxOutputBytes: 8}).Run(context.Background(), Command{
		Argv:   []string{"/bin/echo", "$(touch " + marker + ")", strings.Repeat("x", 32)},
		Env:    []string{"PATH=/usr/bin:/bin"},
		Stdout: &output,
	})
	if err != nil || exit.Code != 0 {
		t.Fatalf("Run() exit=%#v err=%v", exit, err)
	}
	if output.Len() != 8 {
		t.Fatalf("bounded output length = %d, want 8", output.Len())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("shell expression was evaluated: %v", err)
	}
}

func TestOSRunnerHardStdoutLimitTerminatesProducer(t *testing.T) {
	executable, err := exec.LookPath("yes")
	if err != nil {
		t.Skip("yes is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output bytes.Buffer
	exit, err := (OSRunner{}).Run(ctx, Command{
		Argv:           []string{executable, "bundle"},
		Env:            []string{"PATH=/usr/bin:/bin"},
		Stdout:         &output,
		StdoutMaxBytes: 32,
	})
	if err == nil {
		t.Fatalf("Run() accepted producer beyond hard cap, exit=%#v", exit)
	}
	if ctx.Err() != nil {
		t.Fatalf("producer was not terminated promptly: %v", ctx.Err())
	}
	if output.Len() != 32 {
		t.Fatalf("hard-capped output length = %d, want 32", output.Len())
	}
}

func TestProduceSourceBundleRemovesPartialOutputOnHardCap(t *testing.T) {
	executable, err := exec.LookPath("yes")
	if err != nil {
		t.Skip("yes is unavailable")
	}
	root := t.TempDir()
	service := &Service{
		runner:        OSRunner{},
		gitExecutable: executable,
		environment:   []string{"PATH=/usr/bin:/bin"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := service.produceSourceBundle(ctx, root, root, "refs/dsx/private/source/test", 32); err == nil {
		t.Fatal("produceSourceBundle accepted producer beyond hard cap")
	}
	if ctx.Err() != nil {
		t.Fatalf("producer was not terminated promptly: %v", ctx.Err())
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial source artifacts remain: %#v", entries)
	}
}

func TestOSRunnerRejectsUnsafeExecutablePath(t *testing.T) {
	t.Parallel()
	for _, executable := range []string{"./git", "git"} {
		if _, err := (OSRunner{}).Run(context.Background(), Command{Argv: []string{executable, "status"}}); err == nil {
			t.Fatalf("Run accepted non-absolute executable %q", executable)
		}
	}
}
