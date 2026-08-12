package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

func TestPerformWorkspaceRebaseSuccessAndConflictPreserveBackup(t *testing.T) {
	for _, test := range []struct {
		name     string
		conflict bool
	}{
		{name: "success"},
		{name: "conflict", conflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWorkspaceRebaseFixture(t, test.conflict)
			result, err := performWorkspaceRebase(
				context.Background(), model.WorkspaceName("alpha"), fixture.record, fixture.artifact,
				fixture.bundle, workspaceTestGitExecutor(t),
			)
			if err != nil {
				t.Fatalf("performWorkspaceRebase() error = %v", err)
			}
			if result.BackupRef != "refs/dsx/backups/alpha" || result.Conflict != test.conflict {
				t.Fatalf("rebase result = %#v", result)
			}
			if got := strings.TrimSpace(workspaceRunGit(t, fixture.guest, "rev-parse", result.BackupRef)); got != fixture.before {
				t.Fatalf("backup ref = %s, want %s", got, fixture.before)
			}
			if test.conflict {
				if strings.TrimSpace(workspaceRunGit(t, fixture.guest, "rev-parse", "REBASE_HEAD")) == "" {
					t.Fatal("conflicted rebase was not preserved")
				}
				return
			}
			if result.ResultCommit == "" || result.ResultCommit == fixture.before {
				t.Fatalf("successful result commit = %q", result.ResultCommit)
			}
			workspaceRunGit(t, fixture.guest, "merge-base", "--is-ancestor", fixture.artifact.SourceRevision, result.ResultCommit)
		})
	}
}

func TestInspectWorkspaceRebaseResolutionContinueAbortAndMissingBackup(t *testing.T) {
	for _, test := range []struct {
		name   string
		action func(*testing.T, workspaceRebaseFixture, string)
		check  func(*testing.T, workspaceRebaseResolution)
	}{
		{
			name: "abort",
			action: func(t *testing.T, fixture workspaceRebaseFixture, _ string) {
				workspaceRunGit(t, fixture.guest, "rebase", "--abort")
			},
			check: func(t *testing.T, resolution workspaceRebaseResolution) {
				if !resolution.Aborted || resolution.Pending {
					t.Fatalf("abort resolution = %#v", resolution)
				}
			},
		},
		{
			name: "continue",
			action: func(t *testing.T, fixture workspaceRebaseFixture, _ string) {
				if err := os.WriteFile(filepath.Join(fixture.guest, "shared.txt"), []byte("resolved\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				workspaceRunGit(t, fixture.guest, "add", "shared.txt")
				workspaceRunGit(t, fixture.guest, "-c", "core.editor=true", "rebase", "--continue")
			},
			check: func(t *testing.T, resolution workspaceRebaseResolution) {
				if resolution.Aborted || resolution.Pending || resolution.Head == "" {
					t.Fatalf("continue resolution = %#v", resolution)
				}
			},
		},
		{
			name: "missing backup",
			action: func(t *testing.T, fixture workspaceRebaseFixture, backup string) {
				workspaceRunGit(t, fixture.guest, "rebase", "--abort")
				workspaceRunGit(t, fixture.guest, "update-ref", "-d", backup)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWorkspaceRebaseFixture(t, true)
			rebase, err := performWorkspaceRebase(context.Background(), "alpha", fixture.record, fixture.artifact, fixture.bundle, workspaceTestGitExecutor(t))
			if err != nil || !rebase.Conflict {
				t.Fatalf("prepare conflict = %#v, %v", rebase, err)
			}
			fixture.record.BackupRef = rebase.BackupRef
			fixture.record.Conflict = true
			fixture.record.ConflictSourceRevision = fixture.artifact.SourceRevision
			test.action(t, fixture, rebase.BackupRef)
			resolution, err := inspectWorkspaceRebaseResolution(context.Background(), workspaceTestGitExecutor(t), fixture.record)
			if test.name == "missing backup" {
				if err == nil || !strings.Contains(err.Error(), "backup ref is missing") {
					t.Fatalf("missing backup error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, resolution)
		})
	}
}

func TestPerformWorkspaceRebaseRejectsDirtyAndMalformedBundleWithoutMutation(t *testing.T) {
	t.Run("dirty workspace", func(t *testing.T) {
		fixture := newWorkspaceRebaseFixture(t, false)
		if err := os.WriteFile(filepath.Join(fixture.guest, "workspace.txt"), []byte("dirty\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := performWorkspaceRebase(context.Background(), "alpha", fixture.record, fixture.artifact, fixture.bundle, workspaceTestGitExecutor(t))
		if err == nil || !strings.Contains(err.Error(), "will not stash, discard, or commit") {
			t.Fatalf("dirty update error = %v", err)
		}
		assertWorkspaceUpdateUnchanged(t, fixture)
	})

	t.Run("malformed bundle", func(t *testing.T) {
		fixture := newWorkspaceRebaseFixture(t, false)
		malformed := filepath.Join(t.TempDir(), "malformed.bundle")
		if err := os.WriteFile(malformed, []byte("not a bundle"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := performWorkspaceRebase(context.Background(), "alpha", fixture.record, fixture.artifact, malformed, workspaceTestGitExecutor(t))
		if err == nil || !strings.Contains(err.Error(), "verify workspace source bundle") {
			t.Fatalf("malformed update error = %v", err)
		}
		assertWorkspaceUpdateUnchanged(t, fixture)
	})

	t.Run("oversized workspace status", func(t *testing.T) {
		fixture := newWorkspaceRebaseFixture(t, false)
		execute := func(_ context.Context, _ string, arguments []string, stdout, _ io.Writer) (runtime.Exit, error) {
			if len(arguments) != 0 && arguments[0] == "status" {
				_, err := stdout.Write(bytes.Repeat([]byte{'x'}, maxWorkspaceGitOutput+1))
				return runtime.Exit{}, err
			}
			t.Fatalf("unexpected command after oversized status: %v", arguments)
			return runtime.Exit{}, nil
		}
		_, err := performWorkspaceRebase(context.Background(), "alpha", fixture.record, fixture.artifact, fixture.bundle, execute)
		if !errors.Is(err, errWorkspaceGitOutputLimit) {
			t.Fatalf("oversized status error = %v", err)
		}
		assertWorkspaceUpdateUnchanged(t, fixture)
	})
}

func TestHardLimitWriterStopsGrowingBundleAtConfiguredLimit(t *testing.T) {
	var destination bytes.Buffer
	writer := &hardLimitWriter{writer: &destination, remaining: 1024}
	chunk := bytes.Repeat([]byte{'x'}, 257)
	var err error
	for err == nil {
		_, err = writer.Write(chunk)
	}
	if !errors.Is(err, errWorkspaceGitOutputLimit) {
		t.Fatalf("growing producer error = %v", err)
	}
	if destination.Len() != 1024 {
		t.Fatalf("bounded destination size = %d, want 1024", destination.Len())
	}
}

type workspaceRebaseFixture struct {
	guest    string
	bundle   string
	before   string
	record   state.GitRecord
	artifact gitx.SourceArtifact
}

func newWorkspaceRebaseFixture(t *testing.T, conflict bool) workspaceRebaseFixture {
	t.Helper()
	host := filepath.Join(t.TempDir(), "host")
	workspaceRunGit(t, "", "init", "--quiet", "--initial-branch=main", host)
	workspaceRunGit(t, host, "config", "user.name", "DSX Test")
	workspaceRunGit(t, host, "config", "user.email", "dsx@example.invalid")
	if err := os.WriteFile(filepath.Join(host, "shared.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceRunGit(t, host, "add", "shared.txt")
	workspaceRunGit(t, host, "commit", "-m", "source")
	source := strings.TrimSpace(workspaceRunGit(t, host, "rev-parse", "HEAD"))

	guest := filepath.Join(t.TempDir(), "guest")
	workspaceRunGit(t, "", "clone", "--quiet", "--no-hardlinks", host, guest)
	workspaceRunGit(t, guest, "config", "user.name", "DSX Test")
	workspaceRunGit(t, guest, "config", "user.email", "dsx@example.invalid")
	workspaceRunGit(t, guest, "switch", "--quiet", "-c", "dsx/alpha")
	workspaceFile := filepath.Join(guest, "workspace.txt")
	if err := os.WriteFile(workspaceFile, []byte("workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if conflict {
		if err := os.WriteFile(filepath.Join(guest, "shared.txt"), []byte("workspace\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	workspaceRunGit(t, guest, "add", "-A")
	workspaceRunGit(t, guest, "commit", "-m", "workspace change")
	before := strings.TrimSpace(workspaceRunGit(t, guest, "rev-parse", "HEAD"))

	if conflict {
		if err := os.WriteFile(filepath.Join(host, "shared.txt"), []byte("local\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(filepath.Join(host, "local.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceRunGit(t, host, "add", "-A")
	workspaceRunGit(t, host, "commit", "-m", "new local source")
	updated := strings.TrimSpace(workspaceRunGit(t, host, "rev-parse", "HEAD"))
	privateRef := "refs/dsx/private/source/00000000000000000000000000000000"
	workspaceRunGit(t, host, "update-ref", privateRef, updated)
	bundle := filepath.Join(t.TempDir(), "source.bundle")
	workspaceRunGit(t, host, "bundle", "create", bundle, privateRef)
	if err := os.Chmod(bundle, gitx.SourceBundleMode); err != nil {
		t.Fatal(err)
	}
	return workspaceRebaseFixture{
		guest: guest, bundle: bundle, before: before,
		record:   state.GitRecord{GuestPath: guest, SourceBranch: "main", SourceRevision: source, WorkspaceBranch: "dsx/alpha"},
		artifact: gitx.SourceArtifact{SourceBranch: "main", SourceRevision: updated, BundleRef: privateRef},
	}
}

func assertWorkspaceUpdateUnchanged(t *testing.T, fixture workspaceRebaseFixture) {
	t.Helper()
	if got := strings.TrimSpace(workspaceRunGit(t, fixture.guest, "rev-parse", "dsx/alpha")); got != fixture.before {
		t.Fatalf("workspace branch changed from %s to %s", fixture.before, got)
	}
	command := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/dsx/backups/alpha")
	command.Dir = fixture.guest
	if err := command.Run(); err == nil {
		t.Fatal("backup ref was created before update preconditions passed")
	}
}

func workspaceTestGitExecutor(t *testing.T) workspaceGitExecutor {
	t.Helper()
	return func(ctx context.Context, directory string, arguments []string, stdout, stderr io.Writer) (runtime.Exit, error) {
		argv := []string{"--no-pager", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false"}
		argv = append(argv, arguments...)
		command := exec.CommandContext(ctx, "git", argv...)
		command.Dir = directory
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
		command.Stdout, command.Stderr = stdout, stderr
		err := command.Run()
		if err == nil {
			code := 0
			return runtime.Exit{Code: &code}, nil
		}
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return runtime.Exit{}, err
		}
		code := exitError.ExitCode()
		return runtime.Exit{Code: &code}, nil
	}
}

func workspaceRunGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}
