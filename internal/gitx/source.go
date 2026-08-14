package gitx

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type repositoryState struct {
	trackedDirty  bool
	warnUntracked bool
	warnIgnored   bool
	untracked     []string
}

type prepareSourceOptions struct {
	expectedBranch       string
	previousRevision     string
	previousHeadRevision string
	previousTree         string
	snapshot             bool
}

func (service *Service) PrepareSource(ctx context.Context, request SourceRequest) (SourceArtifact, error) {
	if err := validateRepositoryDescriptor(request.Repository); err != nil {
		return SourceArtifact{}, err
	}
	if err := validateWorkspace(request.Workspace); err != nil {
		return SourceArtifact{}, err
	}
	if err := validateTempRoot(request.TempRoot); err != nil {
		return SourceArtifact{}, err
	}
	identity, err := service.captureRepositoryIdentity(ctx, request.Repository, request.ApprovedRoot)
	if err != nil {
		return SourceArtifact{}, err
	}
	repository := request.Repository
	repository.Identity = identity
	return service.prepareSourceArtifact(ctx, repository, request.Workspace, request.TempRoot, prepareSourceOptions{snapshot: request.Snapshot})
}

func (service *Service) PrepareUpdateSource(ctx context.Context, request UpdateSourceRequest) (SourceArtifact, error) {
	if err := validateRepositoryDescriptor(request.Repository); err != nil {
		return SourceArtifact{}, err
	}
	if err := validateWorkspace(request.Workspace); err != nil {
		return SourceArtifact{}, err
	}
	if err := validateTempRoot(request.TempRoot); err != nil {
		return SourceArtifact{}, err
	}
	if err := validateSourceBranch(request.SourceBranch); err != nil {
		return SourceArtifact{}, err
	}
	if err := validateFullOID(request.SourceRevision, "recorded source revision"); err != nil {
		return SourceArtifact{}, err
	}
	if request.SourceTree != "" && request.SourceHeadRevision == "" {
		return SourceArtifact{}, errors.New("recorded source tree requires a source head revision")
	}
	previousHead := request.SourceHeadRevision
	previousTree := request.SourceTree
	if previousHead == "" {
		previousHead = request.SourceRevision
	} else if err := validateFullOID(previousHead, "recorded source head revision"); err != nil {
		return SourceArtifact{}, err
	}
	if previousTree != "" {
		if err := validateFullOID(previousTree, "recorded source tree"); err != nil {
			return SourceArtifact{}, err
		}
		if len(previousTree) != len(request.SourceRevision) {
			return SourceArtifact{}, errors.New("recorded source provenance uses inconsistent Git object formats")
		}
	}
	if len(previousHead) != len(request.SourceRevision) {
		return SourceArtifact{}, errors.New("recorded source provenance uses inconsistent Git object formats")
	}
	if err := service.validateRepositoryIdentity(ctx, request.Repository); err != nil {
		return SourceArtifact{}, err
	}
	return service.prepareSourceArtifact(ctx, request.Repository, request.Workspace, request.TempRoot, prepareSourceOptions{
		expectedBranch:       request.SourceBranch,
		previousRevision:     request.SourceRevision,
		previousHeadRevision: previousHead,
		previousTree:         previousTree,
		snapshot:             request.Snapshot,
	})
}

func (service *Service) prepareSourceArtifact(
	ctx context.Context,
	repository Repository,
	workspace string,
	tempRoot string,
	options prepareSourceOptions,
) (SourceArtifact, error) {
	if options.snapshot {
		return service.prepareSnapshotSourceArtifact(ctx, repository, workspace, tempRoot, options)
	}
	return service.prepareCleanSourceArtifact(ctx, repository, workspace, tempRoot, options)
}

func (service *Service) prepareCleanSourceArtifact(
	ctx context.Context,
	repository Repository,
	workspace string,
	tempRoot string,
	options prepareSourceOptions,
) (artifact SourceArtifact, returnErr error) {
	if err := service.validateRepositoryIdentity(ctx, repository); err != nil {
		return SourceArtifact{}, err
	}
	if err := service.validateLocalConfiguration(ctx, repository.HostPath); err != nil {
		return SourceArtifact{}, err
	}

	sourceBranch, sourceRevision, sourceRef, err := service.resolveSourceIdentity(ctx, repository, options.expectedBranch)
	if err != nil {
		return SourceArtifact{}, err
	}
	state, err := service.inspectRepositoryState(ctx, repository.HostPath)
	if err != nil {
		return SourceArtifact{}, err
	}
	if state.trackedDirty {
		return SourceArtifact{}, errors.New("repository has tracked or index changes; source snapshot was not requested")
	}
	fingerprint, err := service.trackedFingerprint(ctx, repository.HostPath)
	if err != nil {
		return SourceArtifact{}, err
	}
	sourceTree, err := service.resolveTree(ctx, repository.HostPath, sourceRevision)
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("resolve source tree: %w", err)
	}
	if options.previousHeadRevision != "" {
		if sourceRevision == options.previousHeadRevision {
			return SourceArtifact{}, errors.New("local source branch has no newer committed revision")
		}
		if err := service.runGit(ctx, repository.HostPath, nil, "merge-base", "--is-ancestor", options.previousHeadRevision, sourceRevision); err != nil {
			return SourceArtifact{}, fmt.Errorf("local source revision does not descend from recorded source head revision: %w", err)
		}
	}

	privateRef, err := sourceSnapshotRef()
	if err != nil {
		return SourceArtifact{}, err
	}
	if err := service.validateRepositoryIdentity(ctx, repository); err != nil {
		return SourceArtifact{}, err
	}
	if err := service.runGit(ctx, repository.HostPath, nil, "update-ref", privateRef, sourceRevision, strings.Repeat("0", len(sourceRevision))); err != nil {
		return SourceArtifact{}, fmt.Errorf("create private source snapshot ref: %w", err)
	}
	refOwned := true
	defer func() {
		if refOwned {
			returnErr = errors.Join(returnErr, service.removeSourceSnapshotRef(ctx, repository, privateRef))
		}
	}()

	if err := service.validateRepositoryIdentity(ctx, repository); err != nil {
		return SourceArtifact{}, fmt.Errorf("revalidate repository before source bundle: %w", err)
	}
	bundlePath, err := service.produceSourceBundle(ctx, repository.HostPath, tempRoot, privateRef, MaxSourceBundleBytes)
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("create source bundle: %w", err)
	}
	owned := false
	defer func() {
		if !owned {
			returnErr = errors.Join(returnErr, os.Remove(bundlePath))
		}
	}()
	digest, err := bundleSHA256(bundlePath)
	if err != nil {
		return SourceArtifact{}, err
	}
	if err := service.verifyBundleInRepository(ctx, bundlePath, digest, repository.HostPath); err != nil {
		return SourceArtifact{}, fmt.Errorf("verify source bundle: %w", err)
	}
	bundleRevision, err := service.singleBundleHead(ctx, repository.HostPath, bundlePath, privateRef)
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("verify private source snapshot ref: %w", err)
	}
	if bundleRevision != sourceRevision {
		return SourceArtifact{}, fmt.Errorf("source bundle revision %s does not match approved revision %s", bundleRevision, sourceRevision)
	}
	if err := service.removeSourceSnapshotRef(ctx, repository, privateRef); err != nil {
		return SourceArtifact{}, err
	}
	refOwned = false

	if err := service.validateSourceSnapshot(ctx, repository, sourceRef, sourceRevision, fingerprint); err != nil {
		return SourceArtifact{}, err
	}
	if err := service.registerArtifact(bundlePath, tempRoot); err != nil {
		return SourceArtifact{}, fmt.Errorf("register source bundle: %w", err)
	}
	owned = true
	return SourceArtifact{
		Repository: repository, SourceBranch: sourceBranch, SourceRevision: sourceRevision,
		SourceHeadRevision: sourceRevision, SourceTree: sourceTree,
		TrackedFingerprint: fingerprint, WarnUntracked: state.warnUntracked, WarnIgnored: state.warnIgnored,
		BundlePath: bundlePath, BundleDigest: digest, BundleRef: privateRef,
	}, nil
}

func (service *Service) prepareSnapshotSourceArtifact(
	ctx context.Context,
	repository Repository,
	workspace string,
	tempRoot string,
	options prepareSourceOptions,
) (artifact SourceArtifact, returnErr error) {
	if err := service.validateRepositoryIdentity(ctx, repository); err != nil {
		return SourceArtifact{}, err
	}
	if err := service.validateLocalConfiguration(ctx, repository.HostPath); err != nil {
		return SourceArtifact{}, err
	}
	sourceBranch, sourceHeadRevision, sourceRef, err := service.resolveSourceIdentity(ctx, repository, options.expectedBranch)
	if err != nil {
		return SourceArtifact{}, err
	}
	if options.previousHeadRevision != "" {
		if err := service.runGit(ctx, repository.HostPath, nil, "merge-base", "--is-ancestor", options.previousHeadRevision, sourceHeadRevision); err != nil {
			return SourceArtifact{}, fmt.Errorf("local source revision does not descend from recorded source head revision: %w", err)
		}
	}
	unmerged, err := service.gitOutput(ctx, repository.HostPath, "ls-files", "--unmerged", "-z")
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("inspect repository unmerged paths: %w", err)
	}
	if len(unmerged) != 0 {
		return SourceArtifact{}, errors.New("repository has unmerged paths")
	}
	baseIndex, err := service.gitOutput(ctx, repository.HostPath, "ls-files", "--stage", "-z")
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("inspect repository index: %w", err)
	}
	if containsGitlink(baseIndex) {
		return SourceArtifact{}, errors.New("repository contains Git submodules or embedded Git repositories")
	}
	hostFingerprint, err := service.trackedFingerprint(ctx, repository.HostPath)
	if err != nil {
		return SourceArtifact{}, err
	}
	state, err := service.inspectRepositoryState(ctx, repository.HostPath)
	if err != nil {
		return SourceArtifact{}, err
	}
	candidates, inputBytes, err := snapshotCandidateListing(ctx, service, repository.HostPath)
	if err != nil {
		return SourceArtifact{}, err
	}

	quarantine, err := service.newGitQuarantine(ctx, repository, tempRoot, true)
	if err != nil {
		return SourceArtifact{}, err
	}
	quarantineOwned := true
	defer func() {
		if quarantineOwned {
			returnErr = errors.Join(returnErr, quarantine.Close())
		}
	}()
	environment := quarantine.environment()
	if err := service.runGitWithEnvironment(ctx, repository.HostPath, environment, nil, "read-tree", sourceHeadRevision); err != nil {
		return SourceArtifact{}, fmt.Errorf("seed snapshot index: %w", err)
	}
	if err := service.runGitWithEnvironment(ctx, repository.HostPath, environment, nil, "add", "-A", "--", "."); err != nil {
		return SourceArtifact{}, fmt.Errorf("materialize snapshot index: %w", err)
	}
	capturedIndex, err := service.gitOutputWithEnvironment(ctx, repository.HostPath, environment, "ls-files", "--stage", "-z")
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("inspect captured snapshot index: %w", err)
	}
	if containsGitlink(capturedIndex) {
		return SourceArtifact{}, errors.New("repository contains Git submodules or embedded Git repositories")
	}
	sourceTree, err := service.writeTree(ctx, repository.HostPath, environment)
	if err != nil {
		return SourceArtifact{}, err
	}
	snapshotFingerprint := fingerprintIndex(capturedIndex)
	if options.previousHeadRevision != "" && sourceHeadRevision == options.previousHeadRevision {
		previousTree := options.previousTree
		if previousTree == "" {
			previousTree, err = service.resolveTree(ctx, repository.HostPath, options.previousRevision)
			if err != nil {
				return SourceArtifact{}, fmt.Errorf("resolve recorded source tree: %w", err)
			}
		}
		if sourceTree == previousTree {
			return SourceArtifact{}, errors.New("local source snapshot has not changed")
		}
	}
	timestamp, err := service.commitTimestamp(ctx, repository.HostPath, sourceHeadRevision)
	if err != nil {
		return SourceArtifact{}, err
	}
	commitEnvironment := appendSnapshotCommitEnvironment(environment, timestamp)
	commitBytes, err := service.gitOutputWithEnvironment(
		ctx,
		repository.HostPath,
		commitEnvironment,
		"commit-tree", sourceTree, "-p", sourceHeadRevision, "-m", "DSX workspace source snapshot",
	)
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("create deterministic snapshot commit: %w", err)
	}
	sourceRevision := strings.TrimSpace(string(commitBytes))
	if err := validateFullOID(sourceRevision, "snapshot source revision"); err != nil {
		return SourceArtifact{}, err
	}

	privateRef, err := sourceSnapshotRef()
	if err != nil {
		return SourceArtifact{}, err
	}
	bareRoot, err := service.newBareRepositoryWithAlternates(
		ctx,
		tempRoot,
		quarantine.objectFormat,
		[]string{quarantine.objects, quarantine.commonObjects},
	)
	if err != nil {
		return SourceArtifact{}, err
	}
	bareOwned := true
	defer func() {
		if bareOwned {
			returnErr = errors.Join(returnErr, os.RemoveAll(bareRoot))
		}
	}()
	if err := service.runGit(ctx, bareRoot, nil, "update-ref", privateRef, sourceRevision, strings.Repeat("0", len(sourceRevision))); err != nil {
		return SourceArtifact{}, fmt.Errorf("create isolated private source ref: %w", err)
	}
	bundlePath, err := service.produceSourceBundle(ctx, bareRoot, tempRoot, privateRef, MaxSourceBundleBytes)
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("create snapshot source bundle: %w", err)
	}
	bundleOwned := true
	defer func() {
		if bundleOwned {
			returnErr = errors.Join(returnErr, os.Remove(bundlePath))
		}
	}()
	digest, err := bundleSHA256(bundlePath)
	if err != nil {
		return SourceArtifact{}, err
	}
	if err := service.verifyBundleInRepository(ctx, bundlePath, digest, bareRoot); err != nil {
		return SourceArtifact{}, fmt.Errorf("verify snapshot source bundle: %w", err)
	}
	bundleRevision, err := service.singleBundleHead(ctx, bareRoot, bundlePath, privateRef)
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("verify isolated private source ref: %w", err)
	}
	if bundleRevision != sourceRevision {
		return SourceArtifact{}, fmt.Errorf("source bundle revision %s does not match approved snapshot revision %s", bundleRevision, sourceRevision)
	}

	if err := service.validateRepositoryIdentity(ctx, repository); err != nil {
		return SourceArtifact{}, fmt.Errorf("revalidate repository after snapshot bundle: %w", err)
	}
	if err := service.validateLocalConfiguration(ctx, repository.HostPath); err != nil {
		return SourceArtifact{}, err
	}
	finalBranch, finalHead, finalRef, err := service.resolveSourceIdentity(ctx, repository, options.expectedBranch)
	if err != nil {
		return SourceArtifact{}, err
	}
	if finalBranch != sourceBranch || finalRef != sourceRef || finalHead != sourceHeadRevision {
		return SourceArtifact{}, errors.New("source branch or HEAD changed during snapshot")
	}
	currentHostFingerprint, err := service.trackedFingerprint(ctx, repository.HostPath)
	if err != nil {
		return SourceArtifact{}, err
	}
	if currentHostFingerprint != hostFingerprint {
		return SourceArtifact{}, errors.New("host index changed during source snapshot")
	}
	finalCandidates, finalInputBytes, err := snapshotCandidateListing(ctx, service, repository.HostPath)
	if err != nil {
		return SourceArtifact{}, err
	}
	if !bytes.Equal(candidates, finalCandidates) || inputBytes != finalInputBytes {
		return SourceArtifact{}, errors.New("snapshot candidate files changed during source snapshot")
	}
	recheckTree, recheckFingerprint, err := service.rebuildSnapshotTree(ctx, repository, tempRoot, sourceHeadRevision)
	if err != nil {
		return SourceArtifact{}, err
	}
	if recheckTree != sourceTree || recheckFingerprint != snapshotFingerprint {
		return SourceArtifact{}, errors.New("snapshot content changed during source snapshot")
	}
	if err := service.validateRepositoryIdentity(ctx, repository); err != nil {
		return SourceArtifact{}, fmt.Errorf("final repository revalidation after source snapshot: %w", err)
	}
	if err := os.RemoveAll(bareRoot); err != nil {
		return SourceArtifact{}, fmt.Errorf("remove temporary source repository: %w", err)
	}
	bareOwned = false
	if err := quarantine.Close(); err != nil {
		return SourceArtifact{}, err
	}
	quarantineOwned = false
	if err := service.registerArtifact(bundlePath, tempRoot); err != nil {
		return SourceArtifact{}, fmt.Errorf("register source bundle: %w", err)
	}
	bundleOwned = false
	return SourceArtifact{
		Repository: repository, SourceBranch: sourceBranch, SourceRevision: sourceRevision,
		SourceSnapshot: true, SourceHeadRevision: sourceHeadRevision, SourceTree: sourceTree,
		TrackedFingerprint: snapshotFingerprint, WarnUntracked: false, WarnIgnored: state.warnIgnored,
		BundlePath: bundlePath, BundleDigest: digest, BundleRef: privateRef,
	}, nil
}

func (service *Service) resolveSourceIdentity(ctx context.Context, repository Repository, expectedBranch string) (branch, revision, ref string, returnErr error) {
	sourceBranchBytes, err := service.gitOutput(ctx, repository.HostPath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", "", "", fmt.Errorf("resolve symbolic source branch: %w", err)
	}
	branch = strings.TrimSuffix(string(sourceBranchBytes), "\n")
	if err := validateSourceBranch(branch); err != nil {
		return "", "", "", err
	}
	if expectedBranch != "" && branch != expectedBranch {
		return "", "", "", fmt.Errorf("local checkout branch %q does not match recorded source branch %q", branch, expectedBranch)
	}
	ref = "refs/heads/" + branch
	if err := service.runGit(ctx, repository.HostPath, nil, "check-ref-format", ref); err != nil {
		return "", "", "", fmt.Errorf("validate source branch: %w", err)
	}
	revision, err = service.resolveCommit(ctx, repository.HostPath, ref)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve source revision: %w", err)
	}
	return branch, revision, ref, nil
}

func snapshotCandidateListing(ctx context.Context, service *Service, repositoryPath string) ([]byte, int64, error) {
	output, err := service.gitOutput(ctx, repositoryPath, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=no")
	if err != nil {
		return nil, 0, fmt.Errorf("inspect snapshot candidates: %w", err)
	}
	parts := cleanNULTerminated(output)
	var total int64
	for index := 0; index < len(parts); index++ {
		record := parts[index]
		if len(record) < 3 || record[2] != ' ' {
			return nil, 0, errors.New("Git returned malformed snapshot candidate status")
		}
		size, sizeErr := snapshotPathSize(repositoryPath, string(record[3:]))
		if sizeErr != nil {
			return nil, 0, sizeErr
		}
		if size > MaxSnapshotInputBytes-total {
			return nil, 0, fmt.Errorf("snapshot materialized input exceeds %d bytes", MaxSnapshotInputBytes)
		}
		total += size
		code := record[:2]
		if code[0] == 'R' || code[0] == 'C' || code[1] == 'R' || code[1] == 'C' {
			index++
			if index >= len(parts) {
				return nil, 0, errors.New("Git returned incomplete snapshot rename status")
			}
			size, sizeErr = snapshotPathSize(repositoryPath, string(parts[index]))
			if sizeErr != nil {
				return nil, 0, sizeErr
			}
			if size > MaxSnapshotInputBytes-total {
				return nil, 0, fmt.Errorf("snapshot materialized input exceeds %d bytes", MaxSnapshotInputBytes)
			}
			total += size
		}
	}
	return output, total, nil
}

func snapshotPathSize(repositoryPath, relative string) (int64, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.IndexByte(relative, 0) >= 0 {
		return 0, fmt.Errorf("Git returned unsafe snapshot path %q", relative)
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return 0, fmt.Errorf("Git returned snapshot path outside repository %q", relative)
	}
	info, err := os.Lstat(filepath.Join(repositoryPath, clean))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect snapshot input %q: %w", relative, err)
	}
	if info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		if info.Size() < 0 {
			return 0, fmt.Errorf("snapshot input %q has invalid size", relative)
		}
		return info.Size(), nil
	}
	if info.IsDir() {
		gitEntry, gitErr := os.Lstat(filepath.Join(repositoryPath, clean, ".git"))
		if gitErr == nil && (gitEntry.IsDir() || gitEntry.Mode().IsRegular()) && gitEntry.Mode()&os.ModeSymlink == 0 {
			return 0, errors.New("repository contains Git submodules or embedded Git repositories")
		}
		if gitErr != nil && !errors.Is(gitErr, os.ErrNotExist) {
			return 0, fmt.Errorf("inspect embedded repository candidate %q: %w", relative, gitErr)
		}
		return 0, nil
	}
	return 0, fmt.Errorf("snapshot input %q is not a regular file or symlink", relative)
}

func containsGitlink(index []byte) bool {
	for _, record := range cleanNULTerminated(index) {
		if bytes.HasPrefix(record, []byte("160000 ")) {
			return true
		}
	}
	return false
}

func fingerprintIndex(index []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("dsx-clean-tracked-v1\x00"))
	_, _ = hash.Write(index)
	return hex.EncodeToString(hash.Sum(nil))
}

func (service *Service) writeTree(ctx context.Context, repositoryPath string, environment []string) (string, error) {
	output, err := service.gitOutputWithEnvironment(ctx, repositoryPath, environment, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write snapshot tree: %w", err)
	}
	tree := strings.TrimSpace(string(output))
	if err := validateFullOID(tree, "snapshot source tree"); err != nil {
		return "", err
	}
	return tree, nil
}

func (service *Service) resolveTree(ctx context.Context, repositoryPath, revision string) (string, error) {
	output, err := service.gitOutput(ctx, repositoryPath, "rev-parse", "--verify", revision+"^{tree}")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(output))
	if err := validateFullOID(tree, "resolved tree"); err != nil {
		return "", err
	}
	return tree, nil
}

func (service *Service) commitTimestamp(ctx context.Context, repositoryPath, revision string) (string, error) {
	output, err := service.gitOutput(ctx, repositoryPath, "show", "-s", "--format=%ct", revision)
	if err != nil {
		return "", fmt.Errorf("resolve source commit timestamp: %w", err)
	}
	timestamp := strings.TrimSpace(string(output))
	value, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || value < 0 {
		return "", fmt.Errorf("source commit timestamp %q is invalid", timestamp)
	}
	return timestamp, nil
}

func appendSnapshotCommitEnvironment(environment []string, timestamp string) []string {
	result := make([]string, 0, len(environment)+6)
	result = append(result, environment...)
	date := "@" + timestamp + " +0000"
	result = append(result,
		"GIT_AUTHOR_NAME=DSX Snapshot",
		"GIT_AUTHOR_EMAIL=snapshot@dsx.invalid",
		"GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME=DSX Snapshot",
		"GIT_COMMITTER_EMAIL=snapshot@dsx.invalid",
		"GIT_COMMITTER_DATE="+date,
	)
	return result
}

func (service *Service) rebuildSnapshotTree(ctx context.Context, repository Repository, tempRoot, sourceHeadRevision string) (tree, fingerprint string, returnErr error) {
	quarantine, err := service.newGitQuarantine(ctx, repository, tempRoot, true)
	if err != nil {
		return "", "", err
	}
	defer func() { returnErr = errors.Join(returnErr, quarantine.Close()) }()
	environment := quarantine.environment()
	if err := service.runGitWithEnvironment(ctx, repository.HostPath, environment, nil, "read-tree", sourceHeadRevision); err != nil {
		return "", "", fmt.Errorf("seed snapshot recheck index: %w", err)
	}
	if err := service.runGitWithEnvironment(ctx, repository.HostPath, environment, nil, "add", "-A", "--", "."); err != nil {
		return "", "", fmt.Errorf("materialize snapshot recheck index: %w", err)
	}
	index, err := service.gitOutputWithEnvironment(ctx, repository.HostPath, environment, "ls-files", "--stage", "-z")
	if err != nil {
		return "", "", fmt.Errorf("inspect snapshot recheck index: %w", err)
	}
	if containsGitlink(index) {
		return "", "", errors.New("repository contains Git submodules or embedded Git repositories")
	}
	tree, err = service.writeTree(ctx, repository.HostPath, environment)
	if err != nil {
		return "", "", err
	}
	return tree, fingerprintIndex(index), nil
}

func (service *Service) produceSourceBundle(ctx context.Context, repositoryPath, tempRoot, privateRef string, maximumBytes int64) (bundlePath string, returnErr error) {
	if maximumBytes < 1 || maximumBytes > MaxSourceBundleBytes {
		return "", errors.New("source bundle size limit is invalid")
	}
	file, err := os.CreateTemp(tempRoot, "dsx-source-*.bundle")
	if err != nil {
		return "", fmt.Errorf("create private source bundle: %w", err)
	}
	ownedPath := file.Name()
	bundlePath = ownedPath
	complete := false
	closed := false
	var createdInfo os.FileInfo
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, file.Close())
		}
		if complete {
			return
		}
		bundlePath = ""
		linked, inspectErr := os.Lstat(ownedPath)
		switch {
		case errors.Is(inspectErr, os.ErrNotExist):
		case inspectErr != nil:
			returnErr = errors.Join(returnErr, inspectErr)
		case createdInfo != nil && !os.SameFile(createdInfo, linked):
			returnErr = errors.Join(returnErr, errors.New("partial source bundle path identity changed"))
		default:
			returnErr = errors.Join(returnErr, os.Remove(ownedPath))
		}
	}()
	if err := file.Chmod(SourceBundleMode); err != nil {
		return "", fmt.Errorf("set source bundle mode: %w", err)
	}
	createdInfo, err = file.Stat()
	if err != nil || !createdInfo.Mode().IsRegular() || createdInfo.Mode().Perm() != SourceBundleMode {
		return "", errors.Join(errors.New("new source bundle has unsafe metadata"), err)
	}

	var stderr cappedCapture
	stderr.limit = maxGitErrorOutput
	arguments := []string{"bundle", "create", "--version=2", "-", privateRef}
	exit, runErr := service.runner.Run(ctx, Command{
		Argv:           service.gitArgv(arguments...),
		Dir:            repositoryPath,
		Env:            append([]string(nil), service.environment...),
		Stdout:         file,
		Stderr:         &stderr,
		StdoutMaxBytes: maximumBytes,
	})
	if runErr != nil || exit.Code != 0 || exit.Signal != "" {
		detail := strings.TrimSpace(string(stderr.Bytes()))
		if stderr.truncated {
			detail += " [diagnostic truncated]"
		}
		if detail == "" {
			detail = "no diagnostic"
		}
		if runErr != nil {
			return "", fmt.Errorf("git bundle failed (exit=%d signal=%q): %s: %w", exit.Code, exit.Signal, detail, runErr)
		}
		return "", fmt.Errorf("git bundle failed (exit=%d signal=%q): %s", exit.Code, exit.Signal, detail)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync source bundle: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close source bundle: %w", err)
	}
	closed = true
	info, err := os.Lstat(bundlePath)
	if err != nil {
		return "", fmt.Errorf("inspect produced source bundle: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != SourceBundleMode ||
		info.Size() < 0 || info.Size() > maximumBytes || !os.SameFile(createdInfo, info) {
		return "", fmt.Errorf("produced source bundle exceeds %d bytes or has unsafe metadata", maximumBytes)
	}
	complete = true
	return bundlePath, nil
}

func sourceSnapshotRef() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate private source snapshot ref: %w", err)
	}
	return "refs/dsx/private/source/" + hex.EncodeToString(nonce[:]), nil
}

func (service *Service) removeSourceSnapshotRef(parent context.Context, repository Repository, ref string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
	defer cancel()
	if err := service.validateRepositoryIdentity(cleanupCtx, repository); err != nil {
		return fmt.Errorf("revalidate repository before private source ref cleanup: %w", err)
	}
	if err := service.runGit(cleanupCtx, repository.HostPath, nil, "update-ref", "-d", ref); err != nil {
		return fmt.Errorf("remove private source snapshot ref: %w", err)
	}
	return nil
}

func (service *Service) validateSourceSnapshot(ctx context.Context, repository Repository, sourceRef, sourceCommit, fingerprint string) error {
	if err := service.validateRepositoryIdentity(ctx, repository); err != nil {
		return err
	}
	current, err := service.resolveCommit(ctx, repository.HostPath, sourceRef)
	if err != nil {
		return fmt.Errorf("recheck source ref: %w", err)
	}
	if current != sourceCommit {
		return fmt.Errorf("source ref changed during snapshot: got %s, want %s", current, sourceCommit)
	}
	state, err := service.inspectRepositoryState(ctx, repository.HostPath)
	if err != nil {
		return err
	}
	if state.trackedDirty {
		return errors.New("repository became dirty during source snapshot")
	}
	currentFingerprint, err := service.trackedFingerprint(ctx, repository.HostPath)
	if err != nil {
		return err
	}
	if currentFingerprint != fingerprint {
		return errors.New("tracked repository fingerprint changed during source snapshot")
	}
	if err := service.validateRepositoryIdentity(ctx, repository); err != nil {
		return err
	}
	current, err = service.resolveCommit(ctx, repository.HostPath, sourceRef)
	if err != nil {
		return fmt.Errorf("final source ref recheck: %w", err)
	}
	if current != sourceCommit {
		return fmt.Errorf("source ref changed during snapshot: got %s, want %s", current, sourceCommit)
	}
	return nil
}

func (service *Service) validateWorkTree(ctx context.Context, repositoryPath string) error {
	rootBytes, err := service.gitOutput(ctx, repositoryPath, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("validate Git work tree: %w", err)
	}
	root := strings.TrimSuffix(string(rootBytes), "\n")
	requested, requestedErr := filepath.EvalSymlinks(repositoryPath)
	resolved, resolvedErr := filepath.EvalSymlinks(root)
	if requestedErr != nil || resolvedErr != nil || requested != resolved {
		return fmt.Errorf("repository path %q is not the Git work-tree root", repositoryPath)
	}
	return nil
}

func (service *Service) resolveCommit(ctx context.Context, repositoryPath, revision string) (string, error) {
	output, err := service.gitOutput(ctx, repositoryPath, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(output))
	if err := validateFullOID(commit, "resolved commit"); err != nil {
		return "", err
	}
	return commit, nil
}

func (service *Service) inspectRepositoryState(ctx context.Context, repositoryPath string) (repositoryState, error) {
	output, err := service.gitOutput(ctx, repositoryPath, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return repositoryState{}, fmt.Errorf("inspect repository status: %w", err)
	}
	parts := cleanNULTerminated(output)
	var state repositoryState
	for index := 0; index < len(parts); index++ {
		record := parts[index]
		if len(record) < 3 || record[2] != ' ' {
			return repositoryState{}, errors.New("Git returned malformed porcelain status")
		}
		code := string(record[:2])
		name := string(record[3:])
		switch code {
		case "??":
			state.warnUntracked = true
			state.untracked = append(state.untracked, name)
		case "!!":
			state.warnIgnored = true
			state.untracked = append(state.untracked, name)
		default:
			state.trackedDirty = true
		}
		if code[0] == 'R' || code[0] == 'C' || code[1] == 'R' || code[1] == 'C' {
			index++
			if index >= len(parts) {
				return repositoryState{}, errors.New("Git returned incomplete rename status")
			}
		}
	}
	return state, nil
}

func (service *Service) trackedFingerprint(ctx context.Context, repositoryPath string) (string, error) {
	output, err := service.gitOutput(ctx, repositoryPath, "ls-files", "--stage", "-z")
	if err != nil {
		return "", fmt.Errorf("fingerprint tracked state: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("dsx-clean-tracked-v1\x00"))
	_, _ = hash.Write(output)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func bundleSHA256(bundlePath string) (string, error) {
	file, err := os.Open(bundlePath)
	if err != nil {
		return "", fmt.Errorf("open bundle: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash bundle: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (service *Service) VerifyBundle(ctx context.Context, bundlePath, expectedDigest string) error {
	privatePath, cleanup, err := copyValidatedBundle(bundlePath, expectedDigest)
	if err != nil {
		return err
	}
	defer cleanup()
	bareRoot, err := os.MkdirTemp("", "dsx-bundle-repository-*")
	if err != nil {
		return fmt.Errorf("create verification repository: %w", err)
	}
	defer os.RemoveAll(bareRoot)
	if err := os.Chmod(bareRoot, 0o700); err != nil {
		return fmt.Errorf("secure verification repository: %w", err)
	}
	if err := service.runGit(ctx, "", nil, "init", "--bare", "--quiet", bareRoot); err != nil {
		return fmt.Errorf("initialize verification repository: %w", err)
	}
	if err := service.runGit(ctx, bareRoot, nil, "bundle", "verify", privatePath); err != nil {
		return fmt.Errorf("git bundle verify: %w", err)
	}
	return nil
}

func (service *Service) verifyBundleInRepository(ctx context.Context, bundlePath, expectedDigest, repositoryPath string) error {
	privatePath, cleanup, err := copyValidatedBundle(bundlePath, expectedDigest)
	if err != nil {
		return err
	}
	defer cleanup()
	return service.runGit(ctx, repositoryPath, nil, "bundle", "verify", privatePath)
}

func copyValidatedBundle(bundlePath, expectedDigest string) (string, func(), error) {
	if err := validateDigest(expectedDigest); err != nil {
		return "", func() {}, err
	}
	if bundlePath == "" || !filepath.IsAbs(bundlePath) || filepath.Clean(bundlePath) != bundlePath || strings.IndexByte(bundlePath, 0) >= 0 {
		return "", func() {}, errors.New("bundle path must be a clean absolute path")
	}
	before, err := os.Lstat(bundlePath)
	if err != nil {
		return "", func() {}, fmt.Errorf("inspect bundle: %w", err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm() != ResultBundleMode {
		return "", func() {}, fmt.Errorf("bundle must be a regular non-symlink mode-%04o file", ResultBundleMode)
	}
	if before.Size() < 0 || before.Size() > MaxResultBundleBytes {
		return "", func() {}, fmt.Errorf("bundle exceeds %d bytes", MaxResultBundleBytes)
	}
	source, err := os.Open(bundlePath)
	if err != nil {
		return "", func() {}, fmt.Errorf("open bundle: %w", err)
	}
	defer source.Close()
	after, err := source.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm() != ResultBundleMode || after.Size() != before.Size() {
		return "", func() {}, errors.New("bundle identity, size, or permissions changed while opening")
	}
	privateRoot, err := os.MkdirTemp("", "dsx-verified-bundle-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create private bundle directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(privateRoot) }
	if err := os.Chmod(privateRoot, 0o700); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("secure private bundle directory: %w", err)
	}
	privatePath := filepath.Join(privateRoot, "transfer.bundle")
	destination, err := os.OpenFile(privatePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, ResultBundleMode)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("create private bundle copy: %w", err)
	}
	hash := sha256.New()
	copied, copyErr := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(source, MaxResultBundleBytes+1))
	syncErr := destination.Sync()
	closeErr := destination.Close()
	if copied > MaxResultBundleBytes {
		cleanup()
		return "", func() {}, fmt.Errorf("bundle exceeds %d bytes", MaxResultBundleBytes)
	}
	if copyErr != nil || syncErr != nil || closeErr != nil {
		cleanup()
		return "", func() {}, errors.Join(copyErr, syncErr, closeErr)
	}
	var actual [sha256.Size]byte
	copy(actual[:], hash.Sum(nil))
	if !digestMatches(expectedDigest, actual) {
		cleanup()
		return "", func() {}, errors.New("bundle sha256 digest mismatch")
	}
	return privatePath, cleanup, nil
}
