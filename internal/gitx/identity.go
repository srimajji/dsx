package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
)

func (service *Service) captureRepositoryIdentity(ctx context.Context, repository Repository, approvedRoot string) (RepositoryIdentity, error) {
	if err := validateRepositoryDescriptor(repository); err != nil {
		return RepositoryIdentity{}, err
	}
	if !cleanAbsolutePath(approvedRoot) {
		return RepositoryIdentity{}, fmt.Errorf("approved project root must be a clean absolute path, got %q", approvedRoot)
	}
	root, err := capturePhysicalPath(approvedRoot)
	if err != nil {
		return RepositoryIdentity{}, fmt.Errorf("capture approved project root identity: %w", err)
	}
	if !pathWithin(root.CanonicalPath, repository.HostPath) {
		return RepositoryIdentity{}, fmt.Errorf("repository path %q is outside approved project root %q", repository.HostPath, root.CanonicalPath)
	}
	worktreeBefore, err := capturePhysicalPath(repository.HostPath)
	if err != nil {
		return RepositoryIdentity{}, fmt.Errorf("capture repository worktree identity: %w", err)
	}
	worktreePath, gitDirPath, err := service.resolveRepositoryPaths(ctx, repository.HostPath)
	if err != nil {
		return RepositoryIdentity{}, err
	}
	if worktreePath != repository.HostPath {
		return RepositoryIdentity{}, fmt.Errorf("repository path %q is not the Git work-tree root %q", repository.HostPath, worktreePath)
	}
	if !pathWithin(root.CanonicalPath, worktreePath) || !pathWithin(root.CanonicalPath, gitDirPath) {
		return RepositoryIdentity{}, errors.New("repository worktree or Git directory is outside the approved project root")
	}
	rootAfter, err := capturePhysicalPath(root.CanonicalPath)
	if err != nil || !reflect.DeepEqual(root, rootAfter) {
		return RepositoryIdentity{}, errors.New("approved project root identity changed while resolving repository")
	}
	worktree, err := capturePhysicalPath(worktreePath)
	if err != nil || !reflect.DeepEqual(worktreeBefore, worktree) {
		return RepositoryIdentity{}, errors.New("repository worktree identity changed while resolving repository")
	}
	gitDir, err := capturePhysicalPath(gitDirPath)
	if err != nil {
		return RepositoryIdentity{}, fmt.Errorf("capture repository Git directory identity: %w", err)
	}
	return RepositoryIdentity{ApprovedRoot: root, Worktree: worktree, GitDir: gitDir}, nil
}

func (service *Service) validateRepositoryIdentity(ctx context.Context, repository Repository) error {
	if err := validateRepositoryDescriptor(repository); err != nil {
		return err
	}
	if err := ValidateRepositoryIdentityContract(repository.Identity); err != nil {
		return err
	}
	if repository.HostPath != repository.Identity.Worktree.CanonicalPath {
		return errors.New("repository host path does not match its captured worktree identity")
	}
	if !pathWithin(repository.Identity.ApprovedRoot.CanonicalPath, repository.Identity.Worktree.CanonicalPath) ||
		!pathWithin(repository.Identity.ApprovedRoot.CanonicalPath, repository.Identity.GitDir.CanonicalPath) {
		return errors.New("repository identity escapes its approved project root")
	}
	if err := revalidatePhysicalPath(repository.Identity.ApprovedRoot); err != nil {
		return fmt.Errorf("approved project root identity changed: %w", err)
	}
	if err := revalidatePhysicalPath(repository.Identity.Worktree); err != nil {
		return fmt.Errorf("repository worktree identity changed: %w", err)
	}
	if err := revalidatePhysicalPath(repository.Identity.GitDir); err != nil {
		return fmt.Errorf("repository Git directory identity changed: %w", err)
	}
	worktreePath, gitDirPath, err := service.resolveRepositoryPaths(ctx, repository.HostPath)
	if err != nil {
		return err
	}
	if worktreePath != repository.Identity.Worktree.CanonicalPath || gitDirPath != repository.Identity.GitDir.CanonicalPath {
		return errors.New("repository Git metadata retargeted after approval")
	}
	if err := revalidatePhysicalPath(repository.Identity.ApprovedRoot); err != nil {
		return fmt.Errorf("approved project root identity changed during validation: %w", err)
	}
	if err := revalidatePhysicalPath(repository.Identity.Worktree); err != nil {
		return fmt.Errorf("repository worktree identity changed during validation: %w", err)
	}
	if err := revalidatePhysicalPath(repository.Identity.GitDir); err != nil {
		return fmt.Errorf("repository Git directory identity changed during validation: %w", err)
	}
	return nil
}

// ValidateRepositoryIdentityContract validates the durable, platform-derived
// shape of a repository identity without consulting the live filesystem.
func ValidateRepositoryIdentityContract(identity RepositoryIdentity) error {
	if err := validatePhysicalPathContract("approved project root", identity.ApprovedRoot); err != nil {
		return err
	}
	if err := validatePhysicalPathContract("repository worktree", identity.Worktree); err != nil {
		return err
	}
	if err := validatePhysicalPathContract("repository Git directory", identity.GitDir); err != nil {
		return err
	}
	return nil
}

func validatePhysicalPathContract(label string, identity PhysicalPathIdentity) error {
	if !cleanAbsolutePath(identity.CanonicalPath) {
		return fmt.Errorf("%s identity has an invalid canonical path", label)
	}
	if len(identity.Components) == 0 || len(identity.Components) > 1024 {
		return fmt.Errorf("%s identity has an invalid component chain", label)
	}
	previous := ""
	for index, component := range identity.Components {
		if !cleanAbsolutePath(component.Path) || component.Inode == 0 {
			return fmt.Errorf("%s identity component %d is invalid", label, index)
		}
		if index == 0 {
			if component.Path != string(filepath.Separator) {
				return fmt.Errorf("%s identity component chain does not start at the filesystem root", label)
			}
		} else if filepath.Dir(component.Path) != previous {
			return fmt.Errorf("%s identity component chain is not contiguous", label)
		}
		previous = component.Path
	}
	if previous != identity.CanonicalPath {
		return fmt.Errorf("%s identity component chain does not end at its canonical path", label)
	}
	return nil
}

func capturePhysicalPath(value string) (PhysicalPathIdentity, error) {
	if !cleanAbsolutePath(value) {
		return PhysicalPathIdentity{}, fmt.Errorf("path must be clean and absolute, got %q", value)
	}
	resolved, err := filepath.EvalSymlinks(value)
	if err != nil {
		return PhysicalPathIdentity{}, err
	}
	if resolved != value {
		return PhysicalPathIdentity{}, fmt.Errorf("path %q is not canonical or contains a symbolic link", value)
	}
	paths := physicalComponentPaths(value)
	identity := PhysicalPathIdentity{CanonicalPath: value, Components: make([]PathComponentIdentity, 0, len(paths))}
	for _, componentPath := range paths {
		component, err := capturePathComponent(componentPath)
		if err != nil {
			return PhysicalPathIdentity{}, err
		}
		identity.Components = append(identity.Components, component)
	}
	return identity, nil
}

func revalidatePhysicalPath(expected PhysicalPathIdentity) error {
	if err := validatePhysicalPathContract("physical path", expected); err != nil {
		return err
	}
	observed, err := capturePhysicalPath(expected.CanonicalPath)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(observed, expected) {
		return errors.New("device or inode chain no longer matches")
	}
	return nil
}

func capturePathComponent(componentPath string) (PathComponentIdentity, error) {
	entry, err := os.Lstat(componentPath)
	if err != nil {
		return PathComponentIdentity{}, fmt.Errorf("inspect path component %q: %w", componentPath, err)
	}
	if !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
		return PathComponentIdentity{}, fmt.Errorf("path component %q is not a physical directory", componentPath)
	}
	descriptor, err := os.Open(componentPath)
	if err != nil {
		return PathComponentIdentity{}, fmt.Errorf("open path component %q: %w", componentPath, err)
	}
	defer descriptor.Close()
	info, err := descriptor.Stat()
	if err != nil {
		return PathComponentIdentity{}, fmt.Errorf("inspect open path component %q: %w", componentPath, err)
	}
	if !os.SameFile(entry, info) {
		return PathComponentIdentity{}, fmt.Errorf("path component %q changed while opening", componentPath)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return PathComponentIdentity{}, fmt.Errorf("path component %q has no device/inode identity", componentPath)
	}
	return PathComponentIdentity{Path: componentPath, Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func physicalComponentPaths(value string) []string {
	if value == string(filepath.Separator) {
		return []string{value}
	}
	parts := strings.Split(strings.TrimPrefix(value, string(filepath.Separator)), string(filepath.Separator))
	paths := make([]string, 1, len(parts)+1)
	paths[0] = string(filepath.Separator)
	current := string(filepath.Separator)
	for _, part := range parts {
		current = filepath.Join(current, part)
		paths = append(paths, current)
	}
	return paths
}

func (service *Service) resolveRepositoryPaths(ctx context.Context, repositoryPath string) (string, string, error) {
	if err := service.validateLocalConfiguration(ctx, repositoryPath); err != nil {
		return "", "", err
	}
	output, err := service.gitOutput(ctx, repositoryPath, "rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir")
	if err != nil {
		return "", "", fmt.Errorf("resolve repository physical paths: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != 2 || !cleanAbsolutePath(lines[0]) || !cleanAbsolutePath(lines[1]) {
		return "", "", errors.New("Git returned invalid repository physical paths")
	}
	return lines[0], lines[1], nil
}

func cleanAbsolutePath(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value && strings.IndexByte(value, 0) < 0
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
