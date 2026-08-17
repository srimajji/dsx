package hostcmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/terminal"
)

const maxGitDiffBytes = 512 * 1024

const gitHelp = `Usage:
  dsx git status WORKSPACE [--repo MEMBER] [--root PATH] [--format text|json]
  dsx git diff WORKSPACE [--repo MEMBER] [--root PATH] [--format text|json]
  dsx git fetch WORKSPACE [--repo MEMBER] [--root PATH] [--format text|json]
  dsx git apply WORKSPACE [--repo MEMBER] [--root PATH] [--format text|json]
`

func runtimeExitCode(exit runtime.Exit, source string) (int, error) {
	if exit.Signal != "" {
		if exit.Code != nil {
			return 0, model.NewError(model.CodeInternal, source+" returned both an exit code and a signal", nil)
		}
		signal, found := interactiveSignals[strings.ToUpper(exit.Signal)]
		if !found {
			return 0, model.NewError(model.CodeInternal, fmt.Sprintf("%s returned unknown signal %q", source, exit.Signal), nil)
		}
		return 128 + int(signal), nil
	}
	if exit.Code == nil || *exit.Code < 0 || *exit.Code > 255 {
		return 0, model.NewError(model.CodeInternal, source+" returned no valid exit status", nil)
	}
	return *exit.Code, nil
}

func (dispatcher *Dispatcher) executeGit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "dsx git", "git requires status, diff, fetch, or apply")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if len(args) != 1 {
			return usageError(stderr, "dsx git", "git help does not accept arguments")
		}
		if _, err := io.WriteString(stdout, gitHelp); err != nil {
			return reportError(stderr, "dsx git", model.Wrap(model.CodeInternal, "write help", err))
		}
		return 0
	}
	operation := args[0]
	switch operation {
	case "status", "diff", "fetch", "apply":
	default:
		return usageError(stderr, "dsx git", fmt.Sprintf("unknown git command %q", operation))
	}
	if len(args) == 1 {
		return usageError(stderr, "dsx git "+operation, operation+" requires a workspace name")
	}
	if args[1] == "--help" || args[1] == "-h" {
		if len(args) != 2 {
			return usageError(stderr, "dsx git "+operation, "help does not accept arguments")
		}
		if _, err := io.WriteString(stdout, gitHelp); err != nil {
			return reportError(stderr, "dsx git "+operation, model.Wrap(model.CodeInternal, "write help", err))
		}
		return 0
	}
	workspace, err := model.ParseWorkspaceName(args[1])
	if err != nil {
		return usageError(stderr, "dsx git "+operation, err.Error())
	}
	flags := newFlagSet("git " + operation)
	root := flags.String("root", ".", "project root")
	format := flags.String("format", "text", "output format: text or json")
	repository := flags.String("repo", "", "exact repository member name")
	if exit, done := parseFlags(flags, args[2:], stdout, stderr, gitHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx git "+operation, operation+" does not accept extra arguments")
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx git "+operation, err)
	}
	if err := validateFormat(*format); err != nil {
		return reportError(stderr, "dsx git "+operation, err)
	}
	if dispatcher == nil || dispatcher.dependencies.Git == nil {
		return reportError(stderr, "dsx git "+operation, model.NewError(model.CodeUnavailable, "workspace Git service is unavailable", nil))
	}

	switch operation {
	case "status":
		result, err := dispatcher.dependencies.Git.GitStatus(ctx, app.GitStatusRequest{Root: *root, Workspace: workspace, Repository: *repository})
		if err != nil {
			return reportError(stderr, "dsx git status", err)
		}
		if err := renderGitStatus(stdout, result, *format); err != nil {
			return reportError(stderr, "dsx git status", err)
		}
	case "diff":
		result, err := dispatcher.dependencies.Git.GitDiff(ctx, app.GitDiffRequest{Root: *root, Workspace: workspace, Repository: *repository, MaxBytes: maxGitDiffBytes})
		if err != nil {
			return reportError(stderr, "dsx git diff", err)
		}
		if err := renderGitDiff(stdout, result, *format); err != nil {
			return reportError(stderr, "dsx git diff", err)
		}
	case "fetch":
		result, err := dispatcher.dependencies.Git.GitFetch(ctx, app.GitFetchRequest{Root: *root, Workspace: workspace, Repository: *repository})
		if err != nil {
			return reportError(stderr, "dsx git fetch", err)
		}
		if err := renderGitFetch(stdout, result, *format); err != nil {
			return reportError(stderr, "dsx git fetch", err)
		}
	case "apply":
		result, err := dispatcher.dependencies.Git.GitApply(ctx, app.GitApplyRequest{Root: *root, Workspace: workspace, Repository: *repository})
		if err != nil {
			return reportError(stderr, "dsx git apply", err)
		}
		if err := renderGitApply(stdout, result, *format); err != nil {
			return reportError(stderr, "dsx git apply", err)
		}
	}
	return 0
}

func renderGitStatus(writer io.Writer, result app.GitStatusResult, format string) error {
	result.Repositories = append([]gitx.Status(nil), result.Repositories...)
	sort.SliceStable(result.Repositories, func(i, j int) bool { return result.Repositories[i].Repository < result.Repositories[j].Repository })
	if format == "json" {
		return encodeJSON(writer, result)
	}
	if _, err := fmt.Fprintf(writer, "Project: %q\nWorkspace: %q\n", terminal.SanitizeLine(string(result.ProjectID)), terminal.SanitizeLine(string(result.Workspace))); err != nil {
		return model.Wrap(model.CodeInternal, "write git status output", err)
	}
	for _, repository := range result.Repositories {
		if _, err := fmt.Fprintf(writer, "Repository %q: workspace=%q source_branch=%q source_revision=%q source_snapshot=%t workspace_branch=%q result_commit=%q host_commit=%q host_tracked_fingerprint=%q host_tracked_clean=%t untracked=%t ignored=%t fetched=%t fetched_commit=%q\n", terminal.SanitizeLine(repository.Repository), terminal.SanitizeLine(repository.Workspace), terminal.SanitizeLine(repository.SourceBranch), terminal.SanitizeLine(repository.SourceRevision), repository.SourceSnapshot, terminal.SanitizeLine(repository.WorkspaceBranch), terminal.SanitizeLine(repository.ResultCommit), terminal.SanitizeLine(repository.HostCommit), terminal.SanitizeLine(repository.HostTrackedFingerprint), repository.HostTrackedClean, repository.WarnUntracked, repository.WarnIgnored, repository.Fetched, terminal.SanitizeLine(repository.FetchedCommit)); err != nil {
			return model.Wrap(model.CodeInternal, "write git status output", err)
		}
	}
	return nil
}

func renderGitDiff(writer io.Writer, result app.GitDiffResult, format string) error {
	result.Diffs = append([]app.RepositoryDiff(nil), result.Diffs...)
	sort.SliceStable(result.Diffs, func(i, j int) bool { return result.Diffs[i].Repository < result.Diffs[j].Repository })
	if format == "json" {
		return encodeJSON(writer, result)
	}
	if _, err := fmt.Fprintf(writer, "Project: %q\nWorkspace: %q\n", terminal.SanitizeLine(string(result.ProjectID)), terminal.SanitizeLine(string(result.Workspace))); err != nil {
		return model.Wrap(model.CodeInternal, "write git diff output", err)
	}
	for _, diff := range result.Diffs {
		if _, err := fmt.Fprintf(writer, "Repository %q:\n", terminal.SanitizeLine(diff.Repository)); err != nil {
			return model.Wrap(model.CodeInternal, "write git diff output", err)
		}
		switch {
		case len(diff.Patch) == 0:
			if _, err := io.WriteString(writer, "[no changes]\n"); err != nil {
				return model.Wrap(model.CodeInternal, "write git diff output", err)
			}
		case bytes.IndexByte(diff.Patch, 0) >= 0 || !utf8.Valid(diff.Patch):
			if _, err := fmt.Fprintf(writer, "[binary diff omitted: %d bytes]\n", len(diff.Patch)); err != nil {
				return model.Wrap(model.CodeInternal, "write git diff output", err)
			}
		default:
			builder := terminal.NewSanitizedBuilder(len(diff.Patch)*4 + 1)
			if !builder.WriteString(string(diff.Patch)) || !builder.Complete() {
				return model.NewError(model.CodeInternal, "git diff could not be rendered safely", nil)
			}
			patch := builder.String()
			if _, err := io.WriteString(writer, patch); err != nil {
				return model.Wrap(model.CodeInternal, "write git diff output", err)
			}
			if !strings.HasSuffix(patch, "\n") {
				if _, err := io.WriteString(writer, "\n"); err != nil {
					return model.Wrap(model.CodeInternal, "write git diff output", err)
				}
			}
		}
		if diff.Truncated {
			if _, err := io.WriteString(writer, "[diff truncated]\n"); err != nil {
				return model.Wrap(model.CodeInternal, "write git diff output", err)
			}
		}
	}
	return nil
}

func renderGitFetch(writer io.Writer, result app.GitFetchResult, format string) error {
	result.Repositories = append([]gitx.FetchResult(nil), result.Repositories...)
	sort.SliceStable(result.Repositories, func(i, j int) bool {
		left, right := result.Repositories[i], result.Repositories[j]
		if left.Repository != right.Repository {
			return left.Repository < right.Repository
		}
		if left.HostRef != right.HostRef {
			return left.HostRef < right.HostRef
		}
		return left.Commit < right.Commit
	})
	if format == "json" {
		return encodeJSON(writer, result)
	}
	if _, err := fmt.Fprintf(writer, "Project: %q\nWorkspace: %q\n", terminal.SanitizeLine(string(result.ProjectID)), terminal.SanitizeLine(string(result.Workspace))); err != nil {
		return model.Wrap(model.CodeInternal, "write git fetch output", err)
	}
	for _, repository := range result.Repositories {
		if _, err := fmt.Fprintf(writer, "Fetched repository %q ref %q at %q\n", terminal.SanitizeLine(repository.Repository), terminal.SanitizeLine(repository.HostRef), terminal.SanitizeLine(repository.Commit)); err != nil {
			return model.Wrap(model.CodeInternal, "write git fetch output", err)
		}
	}
	return nil
}

func renderGitApply(writer io.Writer, result app.GitApplyResult, format string) error {
	result.Repositories = append([]gitx.ApplyResult(nil), result.Repositories...)
	for i := range result.Repositories {
		result.Repositories[i].Paths = append([]string(nil), result.Repositories[i].Paths...)
		sort.Strings(result.Repositories[i].Paths)
	}
	sort.SliceStable(result.Repositories, func(i, j int) bool {
		left, right := result.Repositories[i], result.Repositories[j]
		if left.Repository != right.Repository {
			return left.Repository < right.Repository
		}
		if left.AppliedCommit != right.AppliedCommit {
			return left.AppliedCommit < right.AppliedCommit
		}
		return strings.Join(left.Paths, "\x00") < strings.Join(right.Paths, "\x00")
	})
	if format == "json" {
		return encodeJSON(writer, result)
	}
	if _, err := fmt.Fprintf(writer, "Project: %q\nWorkspace: %q\n", terminal.SanitizeLine(string(result.ProjectID)), terminal.SanitizeLine(string(result.Workspace))); err != nil {
		return model.Wrap(model.CodeInternal, "write git apply output", err)
	}
	for _, repository := range result.Repositories {
		if _, err := fmt.Fprintf(writer, "Applied repository %q commit %q\n", terminal.SanitizeLine(repository.Repository), terminal.SanitizeLine(repository.AppliedCommit)); err != nil {
			return model.Wrap(model.CodeInternal, "write git apply output", err)
		}
		for _, path := range repository.Paths {
			if _, err := fmt.Fprintf(writer, "  Path: %q\n", terminal.SanitizeLine(path)); err != nil {
				return model.Wrap(model.CodeInternal, "write git apply output", err)
			}
		}
	}
	return nil
}
