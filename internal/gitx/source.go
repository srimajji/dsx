package gitx

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type repositoryState struct {
	trackedDirty  bool
	warnUntracked bool
	warnIgnored   bool
	untracked     []string
}

func (service *Service) PrepareSource(ctx context.Context, request SourceRequest) (artifact SourceArtifact, returnErr error) {
	if err := validateRepositoryDescriptor(request.Repository); err != nil {
		return SourceArtifact{}, err
	}
	if err := validateSandbox(request.Sandbox); err != nil {
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
	if err := service.validateRepositoryIdentity(ctx, repository); err != nil {
		return SourceArtifact{}, err
	}
	if err := service.validateLocalConfiguration(ctx, repository.HostPath); err != nil {
		return SourceArtifact{}, err
	}

	sourceRefBytes, err := service.gitOutput(ctx, repository.HostPath, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("resolve symbolic source ref: %w", err)
	}
	sourceRef := strings.TrimSuffix(string(sourceRefBytes), "\n")
	if sourceRef == "" || strings.ContainsAny(sourceRef, "\r\n\x00") || !strings.HasPrefix(sourceRef, "refs/heads/") {
		return SourceArtifact{}, fmt.Errorf("resolved source ref %q is not a local branch", sourceRef)
	}
	if err := service.runGit(ctx, repository.HostPath, nil, "check-ref-format", sourceRef); err != nil {
		return SourceArtifact{}, fmt.Errorf("validate source ref: %w", err)
	}
	sourceCommit, err := service.resolveCommit(ctx, repository.HostPath, sourceRef)
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("resolve source commit: %w", err)
	}
	state, err := service.inspectRepositoryState(ctx, repository.HostPath)
	if err != nil {
		return SourceArtifact{}, err
	}
	if state.trackedDirty {
		return SourceArtifact{}, errors.New("repository has tracked or index changes")
	}
	fingerprint, err := service.trackedFingerprint(ctx, repository.HostPath)
	if err != nil {
		return SourceArtifact{}, err
	}

	privateRef, err := sourceSnapshotRef()
	if err != nil {
		return SourceArtifact{}, err
	}
	if err := service.validateRepositoryIdentity(ctx, repository); err != nil {
		return SourceArtifact{}, err
	}
	if err := service.runGit(ctx, repository.HostPath, nil, "update-ref", privateRef, sourceCommit, strings.Repeat("0", len(sourceCommit))); err != nil {
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
	bundlePath, err := service.produceSourceBundle(ctx, repository.HostPath, request.TempRoot, privateRef, MaxSourceBundleBytes)
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
	bundleCommit, err := service.singleBundleHead(ctx, repository.HostPath, bundlePath, privateRef)
	if err != nil {
		return SourceArtifact{}, fmt.Errorf("verify private source snapshot ref: %w", err)
	}
	if bundleCommit != sourceCommit {
		return SourceArtifact{}, fmt.Errorf("source bundle commit %s does not match approved commit %s", bundleCommit, sourceCommit)
	}
	if err := service.removeSourceSnapshotRef(ctx, repository, privateRef); err != nil {
		return SourceArtifact{}, err
	}
	refOwned = false

	if err := service.validateSourceSnapshot(ctx, repository, sourceRef, sourceCommit, fingerprint); err != nil {
		return SourceArtifact{}, err
	}
	if err := service.registerArtifact(bundlePath, request.TempRoot); err != nil {
		return SourceArtifact{}, fmt.Errorf("register source bundle: %w", err)
	}
	owned = true
	return SourceArtifact{
		Repository:         repository,
		SourceRef:          sourceRef,
		SourceCommit:       sourceCommit,
		TrackedFingerprint: fingerprint,
		WarnUntracked:      state.warnUntracked,
		WarnIgnored:        state.warnIgnored,
		BundlePath:         bundlePath,
		BundleDigest:       digest,
		BundleRef:          privateRef,
	}, nil
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
