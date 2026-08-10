package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (service *Service) FetchResult(ctx context.Context, request FetchRequest) (FetchResult, error) {
	if err := service.validateRepositoryIdentity(ctx, request.Repository); err != nil {
		return FetchResult{}, err
	}
	if err := service.validateLocalConfiguration(ctx, request.Repository.HostPath); err != nil {
		return FetchResult{}, err
	}
	if err := validateSandbox(request.Sandbox); err != nil {
		return FetchResult{}, err
	}
	if err := validateFullOID(request.ExpectedCommit, "expected result commit"); err != nil {
		return FetchResult{}, err
	}
	if err := service.validateWorkTree(ctx, request.Repository.HostPath); err != nil {
		return FetchResult{}, err
	}
	privatePath, cleanup, err := copyValidatedBundle(request.BundlePath, request.Digest)
	if err != nil {
		return FetchResult{}, err
	}
	defer cleanup()
	if err := service.runGit(ctx, request.Repository.HostPath, nil, "bundle", "verify", privatePath); err != nil {
		return FetchResult{}, fmt.Errorf("verify result bundle: %w", err)
	}

	expectedBundleRef := "refs/heads/dsx/" + request.Sandbox
	commit, err := service.singleBundleHead(ctx, request.Repository.HostPath, privatePath, expectedBundleRef)
	if err != nil {
		return FetchResult{}, err
	}
	if commit != request.ExpectedCommit {
		return FetchResult{}, fmt.Errorf("result bundle commit %s does not match expected commit %s", commit, request.ExpectedCommit)
	}
	hostRef := RefNamespace + request.Sandbox
	refspec := expectedBundleRef + ":" + hostRef
	if err := service.validateRepositoryIdentity(ctx, request.Repository); err != nil {
		return FetchResult{}, fmt.Errorf("revalidate repository before result fetch: %w", err)
	}
	if err := service.runGit(ctx, request.Repository.HostPath, nil,
		"fetch", "--atomic", "--force", "--no-tags", "--no-write-fetch-head", privatePath, refspec,
	); err != nil {
		return FetchResult{}, fmt.Errorf("fetch verified result bundle: %w", err)
	}
	observed, err := service.resolveCommit(ctx, request.Repository.HostPath, hostRef)
	if err != nil {
		return FetchResult{}, fmt.Errorf("resolve fetched result: %w", err)
	}
	if observed != commit {
		return FetchResult{}, fmt.Errorf("fetched ref resolved to %s, want %s", observed, commit)
	}
	return FetchResult{Repository: request.Repository.Name, HostRef: hostRef, Commit: commit}, nil
}

func (service *Service) singleBundleHead(ctx context.Context, repositoryPath, bundlePath, expectedRef string) (string, error) {
	output, err := service.gitOutput(ctx, repositoryPath, "bundle", "list-heads", bundlePath)
	if err != nil {
		return "", fmt.Errorf("list result bundle heads: %w", err)
	}
	lines := bytes.Split(bytes.TrimSuffix(output, []byte{'\n'}), []byte{'\n'})
	if len(lines) != 1 {
		return "", errors.New("result bundle must contain exactly one advertised ref")
	}
	fields := bytes.Fields(lines[0])
	if len(fields) != 2 {
		return "", errors.New("result bundle advertised a malformed ref")
	}
	commit, ref := string(fields[0]), string(fields[1])
	if err := validateFullOID(commit, "bundle commit"); err != nil {
		return "", err
	}
	if ref != expectedRef {
		return "", fmt.Errorf("result bundle ref %q does not match required %q", ref, expectedRef)
	}
	if err := service.runGit(ctx, repositoryPath, nil, "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("result bundle contains an invalid ref: %w", err)
	}
	return commit, nil
}

func (service *Service) Status(ctx context.Context, request StatusRequest) (Status, error) {
	if err := service.validateRepositoryIdentity(ctx, request.Repository); err != nil {
		return Status{}, err
	}
	if err := service.validateLocalConfiguration(ctx, request.Repository.HostPath); err != nil {
		return Status{}, err
	}
	if err := validateSandbox(request.Sandbox); err != nil {
		return Status{}, err
	}
	if request.SourceRef == "" || !strings.HasPrefix(request.SourceRef, "refs/heads/") || strings.ContainsAny(request.SourceRef, "\x00\r\n") {
		return Status{}, errors.New("source ref must be a local branch ref")
	}
	if err := service.runGit(ctx, request.Repository.HostPath, nil, "check-ref-format", request.SourceRef); err != nil {
		return Status{}, fmt.Errorf("validate source ref: %w", err)
	}
	if request.ResultBranch != "dsx/"+request.Sandbox {
		return Status{}, errors.New("result branch does not match sandbox")
	}
	if err := service.runGit(ctx, request.Repository.HostPath, nil, "check-ref-format", "refs/heads/"+request.ResultBranch); err != nil {
		return Status{}, fmt.Errorf("validate result branch: %w", err)
	}
	if err := validateFullOID(request.SourceCommit, "source commit"); err != nil {
		return Status{}, err
	}
	if request.ResultCommit != "" {
		if err := validateFullOID(request.ResultCommit, "result commit"); err != nil {
			return Status{}, err
		}
	}
	if err := validateDigest(request.TrackedFingerprint); err != nil {
		return Status{}, fmt.Errorf("tracked fingerprint: %w", err)
	}
	if request.FetchedCommit != "" {
		if err := validateFullOID(request.FetchedCommit, "fetched commit"); err != nil {
			return Status{}, err
		}
	}
	if err := service.validateWorkTree(ctx, request.Repository.HostPath); err != nil {
		return Status{}, err
	}
	current, err := service.resolveCommit(ctx, request.Repository.HostPath, "HEAD")
	if err != nil {
		return Status{}, fmt.Errorf("resolve current commit: %w", err)
	}
	state, err := service.inspectRepositoryState(ctx, request.Repository.HostPath)
	if err != nil {
		return Status{}, err
	}
	fingerprint, err := service.trackedFingerprint(ctx, request.Repository.HostPath)
	if err != nil {
		return Status{}, err
	}
	observedFetched, found, err := service.optionalRefCommit(ctx, request.Repository.HostPath, RefNamespace+request.Sandbox)
	if err != nil {
		return Status{}, err
	}
	fetched := found && (request.FetchedCommit == "" || observedFetched == request.FetchedCommit)
	if err := service.validateRepositoryIdentity(ctx, request.Repository); err != nil {
		return Status{}, fmt.Errorf("repository identity changed during status: %w", err)
	}
	return Status{
		Repository:             request.Repository.Name,
		Sandbox:                request.Sandbox,
		SourceRef:              request.SourceRef,
		SourceCommit:           request.SourceCommit,
		ResultBranch:           request.ResultBranch,
		ResultCommit:           request.ResultCommit,
		HostCommit:             current,
		HostTrackedFingerprint: fingerprint,
		HostTrackedClean:       !state.trackedDirty,
		WarnUntracked:          state.warnUntracked,
		WarnIgnored:            state.warnIgnored,
		Fetched:                fetched,
		FetchedCommit:          observedFetched,
	}, nil
}

func (service *Service) optionalRefCommit(ctx context.Context, repositoryPath, ref string) (string, bool, error) {
	capture := cappedCapture{limit: 256}
	var stderr cappedCapture
	stderr.limit = maxGitErrorOutput
	exit, runErr := service.runner.Run(ctx, Command{
		Argv:   service.gitArgv("rev-parse", "--verify", "--quiet", ref+"^{commit}"),
		Dir:    repositoryPath,
		Env:    append([]string(nil), service.environment...),
		Stdout: &capture,
		Stderr: &stderr,
	})
	if exit.Code == 1 {
		return "", false, nil
	}
	if runErr != nil || exit.Code != 0 || exit.Signal != "" {
		return "", false, fmt.Errorf("inspect fetched ref %q: exit=%d signal=%q: %s: %w", ref, exit.Code, exit.Signal, strings.TrimSpace(string(stderr.Bytes())), runErr)
	}
	commit := strings.TrimSpace(string(capture.Bytes()))
	if err := validateFullOID(commit, "fetched ref commit"); err != nil {
		return "", false, err
	}
	return commit, true, nil
}

func (service *Service) Diff(ctx context.Context, request DiffRequest) (result DiffResult, returnErr error) {
	if err := service.validateRepositoryIdentity(ctx, request.Repository); err != nil {
		return DiffResult{}, err
	}
	if err := validateFullOID(request.BaseCommit, "base commit"); err != nil {
		return DiffResult{}, err
	}
	if err := validateFullOID(request.HeadCommit, "head commit"); err != nil {
		return DiffResult{}, err
	}
	if len(request.BaseCommit) != len(request.HeadCommit) {
		return DiffResult{}, errors.New("base and head commits use different object formats")
	}
	maxBytes := request.MaxBytes
	if maxBytes == 0 {
		maxBytes = defaultDiffMaxBytes
	}
	if maxBytes < 0 || maxBytes >= maxGitOutput {
		return DiffResult{}, fmt.Errorf("diff byte cap must be between 1 and %d", maxGitOutput-1)
	}

	repositoryPath := request.Repository.HostPath
	if request.Bundle == nil {
		if err := service.validateLocalConfiguration(ctx, repositoryPath); err != nil {
			return DiffResult{}, err
		}
		if err := service.validateWorkTree(ctx, repositoryPath); err != nil {
			return DiffResult{}, err
		}
	} else {
		materialized, cleanup, err := service.materializeDiffBundle(ctx, request)
		if err != nil {
			return DiffResult{}, err
		}
		defer func() { returnErr = errors.Join(returnErr, cleanup()) }()
		repositoryPath = materialized
	}
	for label, commit := range map[string]string{"base": request.BaseCommit, "head": request.HeadCommit} {
		resolved, err := service.resolveCommit(ctx, repositoryPath, commit)
		if err != nil {
			return DiffResult{}, fmt.Errorf("resolve %s commit: %w", label, err)
		}
		if resolved != commit {
			return DiffResult{}, fmt.Errorf("resolved %s commit %s does not match expected %s", label, resolved, commit)
		}
	}
	capture := cappedCapture{limit: maxBytes + 1}
	if err := service.runGit(ctx, repositoryPath, &capture,
		"diff", "--binary", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", request.BaseCommit, request.HeadCommit, "--",
	); err != nil {
		return DiffResult{}, err
	}
	patch := append([]byte(nil), capture.Bytes()...)
	truncated := capture.truncated || len(patch) > maxBytes
	if len(patch) > maxBytes {
		patch = patch[:maxBytes]
	}
	if err := service.validateRepositoryIdentity(ctx, request.Repository); err != nil {
		return DiffResult{}, fmt.Errorf("repository identity changed during diff: %w", err)
	}
	return DiffResult{Patch: patch, Truncated: truncated}, nil
}

func (service *Service) materializeDiffBundle(ctx context.Context, request DiffRequest) (repositoryPath string, cleanup func() error, returnErr error) {
	cleanup = func() error { return nil }
	if request.Bundle == nil {
		return "", cleanup, errors.New("result bundle is required")
	}
	if request.Bundle.Ref == "" || !strings.HasPrefix(request.Bundle.Ref, "refs/heads/") ||
		strings.ContainsAny(request.Bundle.Ref, "\x00\r\n") {
		return "", cleanup, errors.New("result bundle ref must be a full branch ref")
	}
	privatePath, discardPrivate, err := copyValidatedBundle(request.Bundle.Path, request.Bundle.Digest)
	if err != nil {
		return "", cleanup, err
	}
	privateRoot := filepath.Dir(privatePath)
	cleanup = func() error { return os.RemoveAll(privateRoot) }
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, cleanup())
		}
	}()
	repositoryPath = filepath.Join(privateRoot, "repository.git")
	if err := os.Mkdir(repositoryPath, 0o700); err != nil {
		discardPrivate()
		return "", cleanup, fmt.Errorf("create private diff repository: %w", err)
	}
	initArguments := []string{"init", "--bare", "--quiet"}
	if len(request.HeadCommit) == 64 {
		initArguments = append(initArguments, "--object-format=sha256")
	}
	initArguments = append(initArguments, repositoryPath)
	if err := service.runGit(ctx, "", nil, initArguments...); err != nil {
		return "", cleanup, fmt.Errorf("initialize private diff repository: %w", err)
	}
	if err := service.runGit(ctx, repositoryPath, nil, "check-ref-format", request.Bundle.Ref); err != nil {
		return "", cleanup, fmt.Errorf("validate result bundle ref: %w", err)
	}
	if err := service.runGit(ctx, repositoryPath, nil, "bundle", "verify", privatePath); err != nil {
		return "", cleanup, fmt.Errorf("verify result bundle: %w", err)
	}
	commit, err := service.singleBundleHead(ctx, repositoryPath, privatePath, request.Bundle.Ref)
	if err != nil {
		return "", cleanup, err
	}
	if commit != request.HeadCommit {
		return "", cleanup, fmt.Errorf("result bundle commit %s does not match expected commit %s", commit, request.HeadCommit)
	}
	const materializedRef = "refs/dsx/result"
	refspec := request.Bundle.Ref + ":" + materializedRef
	if err := service.runGit(ctx, repositoryPath, nil,
		"fetch", "--atomic", "--force", "--no-tags", "--no-write-fetch-head", privatePath, refspec,
	); err != nil {
		return "", cleanup, fmt.Errorf("materialize verified result bundle: %w", err)
	}
	observed, err := service.resolveCommit(ctx, repositoryPath, materializedRef)
	if err != nil {
		return "", cleanup, fmt.Errorf("resolve materialized result: %w", err)
	}
	if observed != request.HeadCommit {
		return "", cleanup, fmt.Errorf("materialized result resolved to %s, want %s", observed, request.HeadCommit)
	}
	return repositoryPath, cleanup, nil
}

func sortedUnique(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}
