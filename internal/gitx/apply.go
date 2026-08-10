package gitx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

func (service *Service) PrepareApply(ctx context.Context, request ApplyRequest) (ApplyTransaction, error) {
	if err := service.validateRepositoryIdentity(ctx, request.Repository); err != nil {
		return nil, err
	}
	if err := service.validateLocalConfiguration(ctx, request.Repository.HostPath); err != nil {
		return nil, err
	}
	if err := validateFullOID(request.SourceCommit, "source commit"); err != nil {
		return nil, err
	}
	if err := validateFullOID(request.ExpectedCommit, "expected result commit"); err != nil {
		return nil, err
	}
	if err := validateDigest(request.TrackedFingerprint); err != nil {
		return nil, fmt.Errorf("tracked fingerprint: %w", err)
	}
	sandbox, err := validateFetchedRef(request.FetchedRef)
	if err != nil {
		return nil, err
	}
	if err := validateSandbox(sandbox); err != nil {
		return nil, err
	}
	fetchedCommit, changedPaths, err := service.validateApplyState(ctx, request, "", nil)
	if err != nil {
		return nil, err
	}
	rollback, err := service.captureApplyRollback(ctx, request.Repository, changedPaths)
	if err != nil {
		return nil, fmt.Errorf("capture apply rollback state: %w", err)
	}
	return &applyTransaction{
		service: service, request: request, fetchedCommit: fetchedCommit,
		changedPaths: changedPaths, rollback: rollback,
	}, nil
}

type applyTransaction struct {
	mu              sync.Mutex
	service         *Service
	request         ApplyRequest
	fetchedCommit   string
	changedPaths    []string
	rollback        *applyRollback
	mutationStarted bool
	committed       bool
}

func (transaction *applyTransaction) Commit(ctx context.Context) (ApplyResult, error) {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.committed {
		return ApplyResult{}, errors.New("apply transaction is already committed")
	}
	if transaction.mutationStarted {
		return ApplyResult{}, errors.New("apply transaction requires rollback before another commit")
	}
	if err := transaction.service.validateLocalConfiguration(ctx, transaction.request.Repository.HostPath); err != nil {
		return ApplyResult{}, fmt.Errorf("revalidate repository-local Git configuration: %w", err)
	}
	if _, _, err := transaction.service.validateApplyState(ctx, transaction.request, transaction.fetchedCommit, transaction.changedPaths); err != nil {
		return ApplyResult{}, fmt.Errorf("revalidate prepared apply at mutation boundary: %w", err)
	}
	if err := transaction.service.validateRepositoryIdentity(ctx, transaction.request.Repository); err != nil {
		return ApplyResult{}, fmt.Errorf("revalidate repository identity at mutation boundary: %w", err)
	}
	if err := transaction.service.validateLocalConfiguration(ctx, transaction.request.Repository.HostPath); err != nil {
		return ApplyResult{}, fmt.Errorf("revalidate repository-local Git configuration at mutation boundary: %w", err)
	}
	if err := transaction.service.validateRepositoryIdentity(ctx, transaction.request.Repository); err != nil {
		return ApplyResult{}, fmt.Errorf("revalidate repository identity immediately before mutation: %w", err)
	}
	transaction.mutationStarted = true
	mergeErr := transaction.service.runGit(ctx, transaction.request.Repository.HostPath, nil,
		"-c", "rerere.enabled=false",
		"-c", "rerere.autoupdate=false",
		"merge", "--squash", "--no-commit", transaction.fetchedCommit,
	)
	if err := transaction.rollback.captureMutatedState(); err != nil {
		return ApplyResult{}, errors.Join(
			fmt.Errorf("capture post-merge rollback state: %w", err),
			mergeErr,
		)
	}
	if mergeErr != nil {
		return ApplyResult{}, fmt.Errorf("squash apply failed: %w", mergeErr)
	}
	current, err := transaction.service.resolveCommit(ctx, transaction.request.Repository.HostPath, "HEAD")
	if err != nil {
		return ApplyResult{}, fmt.Errorf("resolve host HEAD after squash apply: %w", err)
	}
	if current != transaction.request.SourceCommit {
		return ApplyResult{}, fmt.Errorf("host HEAD changed during squash apply: got %q, want %q", current, transaction.request.SourceCommit)
	}
	pathsOutput, err := transaction.service.gitOutput(ctx, transaction.request.Repository.HostPath, "diff", "--cached", "--name-only", "-z", "--")
	if err != nil {
		return ApplyResult{}, fmt.Errorf("inspect applied paths: %w", err)
	}
	for _, item := range cleanNULTerminated(pathsOutput) {
		if err := validateGitPath(string(item)); err != nil {
			return ApplyResult{}, err
		}
	}
	transaction.committed = true
	return ApplyResult{Repository: transaction.request.Repository.Name, AppliedCommit: transaction.fetchedCommit, Paths: append([]string(nil), transaction.changedPaths...)}, nil
}

func (transaction *applyTransaction) Rollback(ctx context.Context) error {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !transaction.mutationStarted {
		return nil
	}
	if err := transaction.service.validateRepositoryIdentity(ctx, transaction.request.Repository); err != nil {
		return fmt.Errorf("revalidate repository identity before rollback: %w", err)
	}
	if err := transaction.rollback.restore(); err != nil {
		return err
	}
	transaction.mutationStarted = false
	transaction.committed = false
	return nil
}

func (service *Service) validateApplyState(ctx context.Context, request ApplyRequest, expectedCommit string, expectedPaths []string) (string, []string, error) {
	if err := service.validateWorkTree(ctx, request.Repository.HostPath); err != nil {
		return "", nil, err
	}
	current, err := service.resolveCommit(ctx, request.Repository.HostPath, "HEAD")
	if err != nil {
		return "", nil, fmt.Errorf("resolve host HEAD: %w", err)
	}
	if current != request.SourceCommit {
		return "", nil, fmt.Errorf("host advanced from source commit %s to %s", request.SourceCommit, current)
	}
	state, err := service.inspectRepositoryState(ctx, request.Repository.HostPath)
	if err != nil {
		return "", nil, err
	}
	if state.trackedDirty {
		return "", nil, errors.New("host index or tracked work tree is not clean")
	}
	fingerprint, err := service.trackedFingerprint(ctx, request.Repository.HostPath)
	if err != nil {
		return "", nil, err
	}
	if fingerprint != request.TrackedFingerprint {
		return "", nil, errors.New("host tracked fingerprint changed since source preparation")
	}
	fetchedCommit, err := service.resolveCommit(ctx, request.Repository.HostPath, request.FetchedRef)
	if err != nil {
		return "", nil, fmt.Errorf("resolve fetched result ref: %w", err)
	}
	if fetchedCommit != request.ExpectedCommit {
		return "", nil, fmt.Errorf("fetched result ref resolved to %s, want recorded result %s", fetchedCommit, request.ExpectedCommit)
	}
	if expectedCommit != "" && fetchedCommit != expectedCommit {
		return "", nil, fmt.Errorf("fetched result changed from %s to %s", expectedCommit, fetchedCommit)
	}
	if err := service.runGit(ctx, request.Repository.HostPath, nil, "merge-base", "--is-ancestor", request.SourceCommit, fetchedCommit); err != nil {
		return "", nil, fmt.Errorf("fetched result is not descended from the prepared source: %w", err)
	}
	changedPaths, err := service.changedPaths(ctx, request.Repository.HostPath, request.SourceCommit, fetchedCommit)
	if err != nil {
		return "", nil, err
	}
	if expectedPaths != nil && !equalStrings(changedPaths, expectedPaths) {
		return "", nil, errors.New("fetched result paths changed after apply preparation")
	}
	for _, untracked := range state.untracked {
		for _, changed := range changedPaths {
			if gitPathsOverlap(untracked, changed) {
				return "", nil, fmt.Errorf("untracked or ignored path %q collides with result path %q", untracked, changed)
			}
		}
	}
	return fetchedCommit, changedPaths, nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateFetchedRef(ref string) (string, error) {
	if !strings.HasPrefix(ref, RefNamespace) {
		return "", fmt.Errorf("fetched ref must be beneath %q", RefNamespace)
	}
	sandbox := strings.TrimPrefix(ref, RefNamespace)
	if sandbox == "" || ref != RefNamespace+sandbox || strings.Contains(sandbox, "/") {
		return "", fmt.Errorf("invalid fetched ref %q", ref)
	}
	return sandbox, nil
}

func (service *Service) changedPaths(ctx context.Context, repositoryPath, baseCommit, headCommit string) ([]string, error) {
	output, err := service.gitOutput(ctx, repositoryPath, "diff", "--name-status", "-z", "--find-renames", baseCommit, headCommit, "--")
	if err != nil {
		return nil, fmt.Errorf("inspect result paths: %w", err)
	}
	parts := cleanNULTerminated(output)
	paths := make([]string, 0, len(parts))
	for index := 0; index < len(parts); {
		status := string(parts[index])
		index++
		if status == "" {
			return nil, errors.New("Git returned an empty diff status")
		}
		count := 1
		if status[0] == 'R' || status[0] == 'C' {
			count = 2
		}
		if index+count > len(parts) {
			return nil, errors.New("Git returned an incomplete name-status record")
		}
		for range count {
			value := string(parts[index])
			index++
			if err := validateGitPath(value); err != nil {
				return nil, err
			}
			paths = append(paths, value)
		}
	}
	return collapseGitPaths(sortedUnique(paths)), nil
}

func validateGitPath(value string) error {
	if value == "" || strings.IndexByte(value, 0) >= 0 || strings.HasPrefix(value, "/") || value == "." || value == ".." || strings.HasPrefix(value, "../") || filepath.ToSlash(filepath.Clean(filepath.FromSlash(value))) != value {
		return fmt.Errorf("Git returned unsafe repository path %q", value)
	}
	for remaining := value; ; {
		component, rest, found := strings.Cut(remaining, "/")
		if hfsDotGitAlias(component) {
			return fmt.Errorf("Git returned repository path %q that aliases Git metadata on macOS", value)
		}
		if !found {
			break
		}
		remaining = rest
	}
	return nil
}

func hfsDotGitAlias(component string) bool {
	const expected = ".git"
	index := 0
	for _, character := range component {
		if hfsIgnorable(character) {
			continue
		}
		if index == len(expected) || character > 0x7f {
			return false
		}
		if character >= 'A' && character <= 'Z' {
			character += 'a' - 'A'
		}
		if byte(character) != expected[index] {
			return false
		}
		index++
	}
	return index == len(expected)
}

func hfsIgnorable(character rune) bool {
	switch character {
	case '\u200c', '\u200d', '\u200e', '\u200f',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u206a', '\u206b', '\u206c', '\u206d', '\u206e', '\u206f',
		'\ufeff':
		return true
	default:
		return false
	}
}

func gitPathsOverlap(left, right string) bool {
	left = strings.TrimSuffix(left, "/")
	right = strings.TrimSuffix(right, "/")
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func collapseGitPaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	for _, candidate := range paths {
		covered := false
		for _, parent := range result {
			if candidate == parent || strings.HasPrefix(candidate, parent+"/") {
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, candidate)
		}
	}
	return result
}

type applyRollback struct {
	roots           []rollbackRoot
	mutated         []rollbackState
	mutatedCaptured bool
}

type rollbackRoot struct {
	label  string
	root   *os.Root
	paths  []string
	before []fileSnapshot
}

type rollbackState struct {
	files []fileSnapshot
}

type fileSnapshot struct {
	path     string
	exists   bool
	identity snapshotIdentity
	data     []byte
	link     string
	items    []fileSnapshot
	parents  []ancestorSnapshot
}

type ancestorSnapshot struct {
	path     string
	exists   bool
	identity snapshotIdentity
}

type snapshotIdentity struct {
	device  uint64
	inode   uint64
	size    int64
	modTime int64
	mode    os.FileMode
}

func (service *Service) captureApplyRollback(ctx context.Context, repository Repository, changedPaths []string) (*applyRollback, error) {
	if len(changedPaths) > maxApplyRollbackPaths-7 {
		return nil, fmt.Errorf("apply rollback path count exceeds %d", maxApplyRollbackPaths)
	}
	indexOutput, err := service.gitOutput(ctx, repository.HostPath, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return nil, err
	}
	indexPath := strings.TrimSpace(string(indexOutput))
	gitDir := repository.Identity.GitDir.CanonicalPath
	if !cleanAbsolutePath(indexPath) || !cleanAbsolutePath(gitDir) || !pathWithin(gitDir, indexPath) {
		return nil, errors.New("Git returned unsafe metadata paths")
	}
	worktreePaths := make([]string, 0, len(changedPaths))
	for _, changedPath := range changedPaths {
		if err := validateGitPath(changedPath); err != nil {
			return nil, err
		}
		worktreePaths = append(worktreePaths, filepath.FromSlash(changedPath))
	}
	indexRelative, err := filepath.Rel(gitDir, indexPath)
	if err != nil || !cleanRootPath(indexRelative) {
		return nil, errors.New("Git returned an unsafe index path")
	}
	gitPaths := []string{indexRelative, "ORIG_HEAD", "MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "AUTO_MERGE", "SQUASH_MSG"}
	worktreePaths = collapseFilesystemPaths(worktreePaths)
	gitPaths = collapseFilesystemPaths(gitPaths)

	worktree, err := openRollbackRoot("repository worktree", repository.Identity.Worktree, worktreePaths)
	if err != nil {
		return nil, err
	}
	worktree.paths, err = rollbackCaptureRoots(worktree.root, worktree.paths)
	if err != nil {
		_ = worktree.root.Close()
		return nil, err
	}
	metadata, err := openRollbackRoot("repository Git directory", repository.Identity.GitDir, gitPaths)
	if err != nil {
		_ = worktree.root.Close()
		return nil, err
	}
	rollback := &applyRollback{roots: []rollbackRoot{worktree, metadata}}
	budget := newSnapshotBudget()
	for index := range rollback.roots {
		rollback.roots[index].before, err = captureSnapshots(rollback.roots[index].root, rollback.roots[index].paths, budget)
		if err != nil {
			rollback.close()
			return nil, err
		}
	}
	return rollback, nil
}

func openRollbackRoot(label string, expected PhysicalPathIdentity, paths []string) (rollbackRoot, error) {
	root, err := os.OpenRoot(expected.CanonicalPath)
	if err != nil {
		return rollbackRoot{}, fmt.Errorf("open %s rollback root: %w", label, err)
	}
	info, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return rollbackRoot{}, fmt.Errorf("inspect %s rollback root: %w", label, err)
	}
	identity, err := snapshotIdentityOf(info)
	if err != nil || len(expected.Components) == 0 {
		_ = root.Close()
		return rollbackRoot{}, errors.Join(fmt.Errorf("inspect %s rollback root identity", label), err)
	}
	terminal := expected.Components[len(expected.Components)-1]
	if !info.IsDir() || identity.device != terminal.Device || identity.inode != terminal.Inode {
		_ = root.Close()
		return rollbackRoot{}, fmt.Errorf("%s changed while opening rollback root", label)
	}
	return rollbackRoot{label: label, root: root, paths: paths}, nil
}

func rollbackCaptureRoots(root *os.Root, paths []string) ([]string, error) {
	result := make([]string, 0, len(paths))
	for _, filePath := range paths {
		parts := strings.Split(filePath, string(filepath.Separator))
		candidate := filePath
		current := ""
		for _, part := range parts[:len(parts)-1] {
			current = filepath.Join(current, part)
			info, err := root.Lstat(current)
			if errors.Is(err, os.ErrNotExist) {
				candidate = current
				break
			}
			if err != nil {
				return nil, err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("rollback path parent %q is not a physical directory", current)
			}
		}
		result = append(result, candidate)
	}
	return collapseFilesystemPaths(result), nil
}

func cleanRootPath(value string) bool {
	return value != "" && value != "." && !filepath.IsAbs(value) &&
		filepath.Clean(value) == value && strings.IndexByte(value, 0) < 0 &&
		value != ".." && !strings.HasPrefix(value, ".."+string(filepath.Separator))
}

func collapseFilesystemPaths(paths []string) []string {
	sort.Strings(paths)
	result := make([]string, 0, len(paths))
	for _, candidate := range paths {
		if !cleanRootPath(candidate) {
			continue
		}
		covered := false
		for _, parent := range result {
			relative, err := filepath.Rel(parent, candidate)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, candidate)
		}
	}
	return result
}

type snapshotBudget struct {
	remainingBytes     int64
	remainingPaths     int
	remainingPathBytes int
}

func newSnapshotBudget() *snapshotBudget {
	return &snapshotBudget{
		remainingBytes:     maxApplyRollbackBytes,
		remainingPaths:     maxApplyRollbackPaths,
		remainingPathBytes: maxApplyRollbackPathBytes,
	}
}

func (budget *snapshotBudget) claimPath(filePath string) error {
	if budget.remainingPaths == 0 {
		return fmt.Errorf("apply rollback path count exceeds %d", maxApplyRollbackPaths)
	}
	if len(filePath) > budget.remainingPathBytes {
		return fmt.Errorf("apply rollback path bytes exceed %d", maxApplyRollbackPathBytes)
	}
	budget.remainingPaths--
	budget.remainingPathBytes -= len(filePath)
	return nil
}

func (budget *snapshotBudget) claimBytes(size int64) error {
	if size < 0 || size > budget.remainingBytes {
		return fmt.Errorf("apply rollback bytes exceed %d", maxApplyRollbackBytes)
	}
	budget.remainingBytes -= size
	return nil
}

func captureSnapshots(root *os.Root, paths []string, budget *snapshotBudget) ([]fileSnapshot, error) {
	snapshots := make([]fileSnapshot, 0, len(paths))
	for _, filePath := range paths {
		parents, err := captureParentChain(root, filePath)
		if err != nil {
			return nil, err
		}
		snapshot, err := captureFile(root, filePath, filePath, budget)
		if err != nil {
			return nil, err
		}
		snapshot.parents = parents
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func captureParentChain(root *os.Root, filePath string) ([]ancestorSnapshot, error) {
	parent := filepath.Dir(filePath)
	if parent == "." {
		return nil, nil
	}
	parts := strings.Split(parent, string(filepath.Separator))
	parents := make([]ancestorSnapshot, 0, len(parts))
	current := ""
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			parents = append(parents, ancestorSnapshot{path: current})
			break
		}
		if err != nil {
			return nil, err
		}
		identity, err := snapshotIdentityOf(info)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("rollback path parent %q is not a physical directory", current)
		}
		parents = append(parents, ancestorSnapshot{path: current, exists: true, identity: identity})
	}
	return parents, nil
}

func captureFile(root *os.Root, filePath, budgetPath string, budget *snapshotBudget) (fileSnapshot, error) {
	if !cleanRootPath(filePath) {
		return fileSnapshot{}, fmt.Errorf("cannot snapshot unsafe relative path %q", filePath)
	}
	if err := budget.claimPath(budgetPath); err != nil {
		return fileSnapshot{}, err
	}
	info, err := root.Lstat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{path: filePath}, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	identity, err := snapshotIdentityOf(info)
	if err != nil {
		return fileSnapshot{}, err
	}
	snapshot := fileSnapshot{path: filePath, exists: true, identity: identity}
	switch {
	case info.Mode().IsRegular():
		if err := budget.claimBytes(info.Size()); err != nil {
			return fileSnapshot{}, err
		}
		file, openErr := root.Open(filePath)
		if openErr != nil {
			return fileSnapshot{}, openErr
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
			_ = file.Close()
			return fileSnapshot{}, errors.Join(errors.New("rollback file identity changed while opening"), statErr)
		}
		snapshot.data, err = io.ReadAll(io.LimitReader(file, info.Size()+1))
		after, afterErr := file.Stat()
		closeErr := file.Close()
		if err != nil || afterErr != nil || closeErr != nil {
			return fileSnapshot{}, errors.Join(err, afterErr, closeErr)
		}
		afterIdentity, identityErr := snapshotIdentityOf(after)
		if identityErr != nil {
			return fileSnapshot{}, identityErr
		}
		if int64(len(snapshot.data)) != info.Size() || afterIdentity != identity {
			return fileSnapshot{}, errors.New("rollback file changed while reading")
		}
	case info.Mode()&os.ModeSymlink != 0:
		snapshot.link, err = root.Readlink(filePath)
		if err == nil {
			err = budget.claimBytes(int64(len(snapshot.link)))
		}
		after, afterErr := root.Lstat(filePath)
		if err != nil || afterErr != nil {
			return fileSnapshot{}, errors.Join(err, afterErr)
		}
		afterIdentity, identityErr := snapshotIdentityOf(after)
		if identityErr != nil || afterIdentity != identity {
			return fileSnapshot{}, errors.Join(errors.New("rollback symlink changed while reading"), identityErr)
		}
	case info.IsDir():
		directoryRoot, openErr := root.OpenRoot(filePath)
		if openErr != nil {
			return fileSnapshot{}, openErr
		}
		defer directoryRoot.Close()
		opened, statErr := directoryRoot.Stat(".")
		if statErr != nil || !os.SameFile(info, opened) || !opened.IsDir() {
			return fileSnapshot{}, errors.Join(errors.New("rollback directory identity changed while opening"), statErr)
		}
		directory, openErr := directoryRoot.Open(".")
		if openErr != nil {
			return fileSnapshot{}, openErr
		}
		names, readErr := directory.Readdirnames(-1)
		closeErr := directory.Close()
		if readErr != nil || closeErr != nil {
			return fileSnapshot{}, errors.Join(readErr, closeErr)
		}
		sort.Strings(names)
		for _, name := range names {
			child, childErr := captureFile(directoryRoot, name, filepath.Join(budgetPath, name), budget)
			if childErr != nil {
				return fileSnapshot{}, childErr
			}
			snapshot.items = append(snapshot.items, child)
		}
		after, afterErr := root.Lstat(filePath)
		openedAfter, openedAfterErr := directoryRoot.Stat(".")
		if afterErr != nil || openedAfterErr != nil || !os.SameFile(info, after) || !os.SameFile(info, openedAfter) {
			return fileSnapshot{}, errors.Join(errors.New("rollback directory changed while reading"), afterErr, openedAfterErr)
		}
		afterIdentity, identityErr := snapshotIdentityOf(after)
		if identityErr != nil || afterIdentity != identity {
			return fileSnapshot{}, errors.Join(errors.New("rollback directory metadata changed while reading"), identityErr)
		}
	default:
		return fileSnapshot{}, fmt.Errorf("cannot safely snapshot non-regular path %q", budgetPath)
	}
	return snapshot, nil
}

func snapshotIdentityOf(info os.FileInfo) (snapshotIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return snapshotIdentity{}, errors.New("rollback path has no device/inode identity")
	}
	return snapshotIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), size: info.Size(),
		modTime: info.ModTime().UnixNano(), mode: info.Mode(),
	}, nil
}

func (rollback *applyRollback) captureMutatedState() error {
	budget := newSnapshotBudget()
	mutated := make([]rollbackState, len(rollback.roots))
	for index := range rollback.roots {
		files, err := captureSnapshots(rollback.roots[index].root, rollback.roots[index].paths, budget)
		if err != nil {
			return fmt.Errorf("capture %s post-merge state: %w", rollback.roots[index].label, err)
		}
		mutated[index] = rollbackState{files: files}
	}
	rollback.mutated = mutated
	rollback.mutatedCaptured = true
	return nil
}

func (rollback *applyRollback) restore() error {
	if !rollback.mutatedCaptured {
		return errors.New("rollback refused because post-merge state was not captured")
	}
	budget := newSnapshotBudget()
	current := make([]rollbackState, len(rollback.roots))
	for index := range rollback.roots {
		files, err := captureSnapshots(rollback.roots[index].root, rollback.roots[index].paths, budget)
		if err != nil {
			return fmt.Errorf("rollback compare-and-swap could not inspect %s: %w", rollback.roots[index].label, err)
		}
		current[index] = rollbackState{files: files}
	}
	if !equalRollbackStates(current, rollback.mutated) {
		return errors.New("rollback compare-and-swap refused: repository state changed after DSX apply mutation")
	}
	for index := range rollback.roots {
		if err := restoreRoot(rollback.roots[index]); err != nil {
			return fmt.Errorf("restore %s: %w", rollback.roots[index].label, err)
		}
	}
	return nil
}

func equalRollbackStates(left, right []rollbackState) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !equalFileSnapshots(left[index].files, right[index].files) {
			return false
		}
	}
	return true
}

func equalFileSnapshots(left, right []fileSnapshot) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].path != right[index].path ||
			left[index].exists != right[index].exists ||
			left[index].identity != right[index].identity ||
			left[index].link != right[index].link ||
			!equalAncestorSnapshots(left[index].parents, right[index].parents) ||
			!equalBytes(left[index].data, right[index].data) ||
			!equalFileSnapshots(left[index].items, right[index].items) {
			return false
		}
	}
	return true
}

func equalAncestorSnapshots(left, right []ancestorSnapshot) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func restoreRoot(root rollbackRoot) error {
	roots := append([]fileSnapshot(nil), root.before...)
	sort.Slice(roots, func(left, right int) bool { return len(roots[left].path) > len(roots[right].path) })
	for _, snapshot := range roots {
		if err := root.root.RemoveAll(snapshot.path); err != nil {
			return err
		}
	}
	sort.Slice(roots, func(left, right int) bool { return len(roots[left].path) < len(roots[right].path) })
	for _, snapshot := range roots {
		if err := restoreFile(root.root, snapshot); err != nil {
			return err
		}
	}
	return nil
}

func restoreFile(root *os.Root, snapshot fileSnapshot) error {
	if !snapshot.exists {
		return nil
	}
	if err := root.MkdirAll(filepath.Dir(snapshot.path), 0o700); err != nil {
		return err
	}
	switch {
	case snapshot.identity.mode.IsRegular():
		file, err := root.OpenFile(snapshot.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, snapshot.identity.mode.Perm())
		if err != nil {
			return err
		}
		written, writeErr := file.Write(snapshot.data)
		if writeErr == nil && written != len(snapshot.data) {
			writeErr = io.ErrShortWrite
		}
		chmodErr := file.Chmod(snapshot.identity.mode.Perm())
		syncErr := file.Sync()
		closeErr := file.Close()
		return errors.Join(writeErr, chmodErr, syncErr, closeErr)
	case snapshot.identity.mode&os.ModeSymlink != 0:
		return root.Symlink(snapshot.link, snapshot.path)
	case snapshot.identity.mode.IsDir():
		if err := root.Mkdir(snapshot.path, snapshot.identity.mode.Perm()); err != nil {
			return err
		}
		directory, err := root.OpenRoot(snapshot.path)
		if err != nil {
			return err
		}
		for _, child := range snapshot.items {
			if err := restoreFile(directory, child); err != nil {
				_ = directory.Close()
				return err
			}
		}
		return errors.Join(directory.Chmod(".", snapshot.identity.mode.Perm()), directory.Close())
	default:
		return fmt.Errorf("cannot restore unsupported path %q", snapshot.path)
	}
}

func (rollback *applyRollback) close() {
	for index := range rollback.roots {
		if rollback.roots[index].root != nil {
			_ = rollback.roots[index].root.Close()
			rollback.roots[index].root = nil
		}
	}
}
