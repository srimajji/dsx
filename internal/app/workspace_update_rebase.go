package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

const maxWorkspaceGitOutput = 64 << 10

var errWorkspaceGitOutputLimit = errors.New("workspace Git output limit exceeded")

type hardLimitWriter struct {
	writer    io.Writer
	remaining int
}

func (writer *hardLimitWriter) Write(value []byte) (int, error) {
	if len(value) > writer.remaining {
		written := 0
		if writer.remaining > 0 {
			written, _ = writer.writer.Write(value[:writer.remaining])
			writer.remaining -= written
		}
		return written, errWorkspaceGitOutputLimit
	}
	written, err := writer.writer.Write(value)
	writer.remaining -= written
	return written, err
}

type workspaceGitExecutor func(context.Context, string, []string, io.Writer, io.Writer) (runtime.Exit, error)

type workspaceRebaseResult struct {
	BackupRef   string
	ResultCommit string
	Conflict    bool
}

type workspaceRebaseResolution struct {
	Pending bool
	Aborted bool
	Head    string
}

func inspectWorkspaceRebaseResolution(ctx context.Context, execute workspaceGitExecutor, record state.GitRecord) (workspaceRebaseResolution, error) {
	var checkedOut bytes.Buffer
	exit, err := execute(ctx, record.GuestPath, []string{"symbolic-ref", "--quiet", "--short", "HEAD"}, &checkedOut, io.Discard)
	if err != nil {
		return workspaceRebaseResolution{}, err
	}
	if exit.Code == nil || exit.Signal != "" {
		return workspaceRebaseResolution{}, model.NewError(model.CodeUnavailable, "inspect conflicted workspace branch failed", nil)
	}
	if *exit.Code != 0 {
		return workspaceRebaseResolution{Pending: true}, nil
	}
	if strings.TrimSpace(checkedOut.String()) != record.WorkspaceBranch {
		return workspaceRebaseResolution{}, model.NewError(model.CodeConflict, "resolved workspace checked out an unexpected branch", nil)
	}
	backupExists, err := gitRefExists(ctx, execute, record.GuestPath, record.BackupRef)
	if err != nil {
		return workspaceRebaseResolution{}, err
	}
	if !backupExists {
		return workspaceRebaseResolution{}, model.NewError(model.CodeAmbiguous, "recorded workspace backup ref is missing", nil)
	}
	head, err := resolveGuestRevision(ctx, execute, record.GuestPath, record.WorkspaceBranch)
	if err != nil {
		return workspaceRebaseResolution{}, err
	}
	backup, err := resolveGuestRevision(ctx, execute, record.GuestPath, record.BackupRef)
	if err != nil {
		return workspaceRebaseResolution{}, err
	}
	if head == backup {
		return workspaceRebaseResolution{Aborted: true, Head: head}, nil
	}
	exit, err = execute(ctx, record.GuestPath, []string{"merge-base", "--is-ancestor", record.ConflictSourceRevision, head}, nil, io.Discard)
	if err != nil || exit.Code == nil || *exit.Code != 0 || exit.Signal != "" {
		return workspaceRebaseResolution{}, model.NewError(model.CodeConflict, "resolved rebase head does not descend from the recorded update source", err)
	}
	return workspaceRebaseResolution{Head: head}, nil
}

func resolveGuestRevision(ctx context.Context, execute workspaceGitExecutor, directory, revision string) (string, error) {
	var output bytes.Buffer
	if err := executeGitCommand(ctx, execute, directory, []string{"rev-parse", "--verify", revision + "^{commit}"}, &output, io.Discard); err != nil {
		return "", err
	}
	return strings.TrimSpace(output.String()), nil
}

func performWorkspaceRebase(
	ctx context.Context,
	workspace model.WorkspaceName,
	record state.GitRecord,
	artifact gitx.SourceArtifact,
	guestBundle string,
	execute workspaceGitExecutor,
) (workspaceRebaseResult, error) {
	if ctx == nil || execute == nil {
		return workspaceRebaseResult{}, model.NewError(model.CodeInvalidInput, "workspace Git update dependencies are unavailable", nil)
	}
	workspaceBranch := "dsx/" + string(workspace)
	if record.WorkspaceBranch != workspaceBranch || artifact.SourceBranch != record.SourceBranch {
		return workspaceRebaseResult{}, model.NewError(model.CodeConflict, "workspace Git update contract changed", nil)
	}
	if guestBundle == "" || strings.ContainsAny(guestBundle, "\x00\r\n") {
		return workspaceRebaseResult{}, model.NewError(model.CodeInvalidInput, "workspace source bundle path is invalid", nil)
	}

	var status bytes.Buffer
	statusCapture := &hardLimitWriter{writer: &status, remaining: maxWorkspaceGitOutput}
	if err := executeGitCommand(ctx, execute, record.GuestPath, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}, statusCapture, nil); err != nil {
		return workspaceRebaseResult{}, fmt.Errorf("workspace status failed or exceeded its output limit: %w", err)
	}
	if status.Len() != 0 {
		return workspaceRebaseResult{}, model.NewError(model.CodeConflict, "workspace has uncommitted or untracked changes; update will not stash, discard, or commit them", nil)
	}
	var branch bytes.Buffer
	if err := executeGitCommand(ctx, execute, record.GuestPath, []string{"symbolic-ref", "--quiet", "--short", "HEAD"}, &branch, nil); err != nil {
		return workspaceRebaseResult{}, err
	}
	if strings.TrimSpace(branch.String()) != workspaceBranch {
		return workspaceRebaseResult{}, model.NewError(model.CodeConflict, "workspace branch is not checked out", nil)
	}
	if err := executeGitCommand(ctx, execute, record.GuestPath, []string{"bundle", "verify", guestBundle}, nil, nil); err != nil {
		return workspaceRebaseResult{}, fmt.Errorf("verify workspace source bundle: %w", err)
	}
	var heads bytes.Buffer
	if err := executeGitCommand(ctx, execute, record.GuestPath, []string{"bundle", "list-heads", guestBundle}, &heads, nil); err != nil {
		return workspaceRebaseResult{}, err
	}
	fields := strings.Fields(strings.TrimSpace(heads.String()))
	if len(fields) != 2 || fields[0] != artifact.SourceRevision || fields[1] != artifact.BundleRef {
		return workspaceRebaseResult{}, model.NewError(model.CodeConflict, "workspace source bundle advertised an unexpected revision or ref", nil)
	}

	sourceRef := "refs/dsx/source/" + string(workspace)
	refspec := artifact.BundleRef + ":" + sourceRef
	if err := executeGitCommand(ctx, execute, record.GuestPath, []string{"fetch", "--atomic", "--force", "--no-tags", "--no-write-fetch-head", guestBundle, refspec}, nil, nil); err != nil {
		return workspaceRebaseResult{}, fmt.Errorf("import workspace source bundle: %w", err)
	}
	var imported bytes.Buffer
	if err := executeGitCommand(ctx, execute, record.GuestPath, []string{"rev-parse", "--verify", sourceRef + "^{commit}"}, &imported, nil); err != nil {
		return workspaceRebaseResult{}, err
	}
	if strings.TrimSpace(imported.String()) != artifact.SourceRevision {
		return workspaceRebaseResult{}, model.NewError(model.CodeConflict, "imported workspace source revision changed after verification", nil)
	}
	if err := executeGitCommand(ctx, execute, record.GuestPath, []string{"merge-base", "--is-ancestor", record.SourceRevision, workspaceBranch}, nil, nil); err != nil {
		return workspaceRebaseResult{}, model.NewError(model.CodeConflict, "workspace branch no longer descends from its recorded source revision", err)
	}
	var before bytes.Buffer
	if err := executeGitCommand(ctx, execute, record.GuestPath, []string{"rev-parse", "--verify", workspaceBranch + "^{commit}"}, &before, nil); err != nil {
		return workspaceRebaseResult{}, err
	}
	beforeRevision := strings.TrimSpace(before.String())
	backupRef := "refs/dsx/backups/" + string(workspace)
	if err := executeGitCommand(ctx, execute, record.GuestPath, []string{"update-ref", backupRef, beforeRevision}, nil, nil); err != nil {
		return workspaceRebaseResult{}, fmt.Errorf("create workspace backup ref: %w", err)
	}

	rebaseArgs := []string{"-c", "rerere.enabled=false", "-c", "rerere.autoupdate=false", "rebase", "--onto", sourceRef, record.SourceRevision, workspaceBranch}
	rebaseErr := executeGitCommand(ctx, execute, record.GuestPath, rebaseArgs, nil, nil)
	if rebaseErr != nil {
		conflict, inspectErr := gitRefExists(ctx, execute, record.GuestPath, "REBASE_HEAD")
		if inspectErr != nil {
			return workspaceRebaseResult{}, errors.Join(rebaseErr, inspectErr)
		}
		if !conflict {
			return workspaceRebaseResult{}, fmt.Errorf("workspace rebase failed before a resolvable conflict was recorded: %w", rebaseErr)
		}
		return workspaceRebaseResult{BackupRef: backupRef, Conflict: true}, nil
	}
	var after bytes.Buffer
	if err := executeGitCommand(ctx, execute, record.GuestPath, []string{"rev-parse", "--verify", workspaceBranch + "^{commit}"}, &after, nil); err != nil {
		return workspaceRebaseResult{}, err
	}
	return workspaceRebaseResult{BackupRef: backupRef, ResultCommit: strings.TrimSpace(after.String())}, nil
}

func executeGitCommand(ctx context.Context, execute workspaceGitExecutor, directory string, arguments []string, stdout, stderr io.Writer) error {
	exit, err := execute(ctx, directory, arguments, stdout, stderr)
	if err != nil {
		return err
	}
	if exit.Code == nil || *exit.Code != 0 || exit.Signal != "" {
		return model.NewError(model.CodeUnavailable, fmt.Sprintf("guest Git %s failed", arguments[0]), nil)
	}
	return nil
}

func gitRefExists(ctx context.Context, execute workspaceGitExecutor, directory, revision string) (bool, error) {
	exit, err := execute(ctx, directory, []string{"rev-parse", "--verify", "--quiet", revision}, nil, nil)
	if err != nil {
		return false, err
	}
	if exit.Signal != "" || exit.Code == nil {
		return false, model.NewError(model.CodeUnavailable, "inspect workspace Git operation state failed", nil)
	}
	switch *exit.Code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, model.NewError(model.CodeUnavailable, "inspect workspace Git operation state failed", nil)
	}
}
