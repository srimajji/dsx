package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type gitQuarantine struct {
	root          string
	objects       string
	index         string
	objectFormat  string
	commonObjects string
	info          os.FileInfo
}

func (service *Service) newGitQuarantine(ctx context.Context, repository Repository, tempRoot string, withIndex bool) (_ *gitQuarantine, returnErr error) {
	if err := validateTempRoot(tempRoot); err != nil {
		return nil, err
	}
	if err := service.validateRepositoryIdentity(ctx, repository); err != nil {
		return nil, err
	}
	commonObjects, err := resolveRepositoryCommonObjectDirectory(repository.HostPath)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(commonObjects, "\x00\r\n") {
		return nil, errors.New("repository common object path contains a forbidden byte")
	}
	formatBytes, err := service.gitOutput(ctx, repository.HostPath, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, fmt.Errorf("resolve repository object format: %w", err)
	}
	objectFormat := strings.TrimSpace(string(formatBytes))
	if objectFormat != "sha1" && objectFormat != "sha256" {
		return nil, fmt.Errorf("unsupported repository object format %q", objectFormat)
	}

	root, err := os.MkdirTemp(tempRoot, "dsx-git-quarantine-*")
	if err != nil {
		return nil, fmt.Errorf("create Git quarantine: %w", err)
	}
	quarantine := &gitQuarantine{
		root:          root,
		objects:       filepath.Join(root, "objects"),
		objectFormat:  objectFormat,
		commonObjects: commonObjects,
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, quarantine.Close())
		}
	}()
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure Git quarantine: %w", err)
	}
	quarantine.info, err = os.Lstat(root)
	if err != nil || !quarantine.info.IsDir() || quarantine.info.Mode()&os.ModeSymlink != 0 || quarantine.info.Mode().Perm() != 0o700 {
		return nil, errors.Join(errors.New("Git quarantine has unsafe metadata"), err)
	}
	if err := os.MkdirAll(filepath.Join(quarantine.objects, "info"), 0o700); err != nil {
		return nil, fmt.Errorf("create Git quarantine object directory: %w", err)
	}
	if err := writeAlternatesFile(filepath.Join(quarantine.objects, "info", "alternates"), []string{commonObjects}); err != nil {
		return nil, err
	}
	if withIndex {
		quarantine.index = filepath.Join(root, "index")
	}
	return quarantine, nil
}

func (quarantine *gitQuarantine) environment() []string {
	environment := []string{"GIT_OBJECT_DIRECTORY=" + quarantine.objects}
	if quarantine.index != "" {
		environment = append(environment, "GIT_INDEX_FILE="+quarantine.index)
	}
	return environment
}

func (quarantine *gitQuarantine) Close() error {
	if quarantine == nil || quarantine.root == "" {
		return nil
	}
	current, err := os.Lstat(quarantine.root)
	if errors.Is(err, os.ErrNotExist) {
		quarantine.root = ""
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Git quarantine before cleanup: %w", err)
	}
	if quarantine.info == nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(quarantine.info, current) {
		return errors.New("Git quarantine path identity changed before cleanup")
	}
	if err := os.RemoveAll(quarantine.root); err != nil {
		return fmt.Errorf("remove Git quarantine: %w", err)
	}
	quarantine.root = ""
	return nil
}

func writeAlternatesFile(filePath string, alternates []string) (returnErr error) {
	for _, alternate := range alternates {
		if alternate == "" || strings.ContainsAny(alternate, "\x00\r\n") || !filepath.IsAbs(alternate) || filepath.Clean(alternate) != alternate {
			return fmt.Errorf("Git alternate object path is unsafe: %q", alternate)
		}
	}
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create Git alternates file: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure Git alternates file: %w", err)
	}
	if _, err := file.WriteString(strings.Join(alternates, "\n") + "\n"); err != nil {
		return fmt.Errorf("write Git alternates file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync Git alternates file: %w", err)
	}
	return nil
}

func resolveRepositoryCommonObjectDirectory(repositoryPath string) (string, error) {
	if !cleanAbsolutePath(repositoryPath) {
		return "", fmt.Errorf("repository path must be clean and absolute, got %q", repositoryPath)
	}
	gitEntryPath := filepath.Join(repositoryPath, ".git")
	entry, err := os.Lstat(gitEntryPath)
	if err != nil {
		return "", fmt.Errorf("inspect repository Git metadata entry: %w", err)
	}

	var gitDirPath string
	var gitDirIdentity PhysicalPathIdentity
	var gitFile stableRegularFile
	switch {
	case entry.IsDir() && entry.Mode()&os.ModeSymlink == 0:
		gitDirPath = gitEntryPath
		gitDirIdentity, err = capturePhysicalPath(gitDirPath)
		if err != nil {
			return "", fmt.Errorf("resolve repository Git directory: %w", err)
		}
	case entry.Mode().IsRegular() && entry.Mode()&os.ModeSymlink == 0:
		gitFile, _, err = readStableRegularFile(gitEntryPath, "repository .git gitfile", maxGitMetadataFileBytes, false)
		if err != nil {
			return "", err
		}
		declaration, parseErr := parseGitDirDeclaration(gitFile.data)
		if parseErr != nil {
			return "", parseErr
		}
		gitDirPath, gitDirIdentity, err = resolvePhysicalGitDirectory(repositoryPath, declaration, "Git directory target")
		if err != nil {
			return "", err
		}
	default:
		return "", errors.New("repository .git entry must be a physical directory or bounded regular non-symlink gitfile")
	}

	commonDirPath := gitDirPath
	commonDirIdentity := gitDirIdentity
	commonPointerPath := filepath.Join(gitDirPath, "commondir")
	commonPointer, hasCommonPointer, err := readStableRegularFile(commonPointerPath, "repository Git common-directory pointer", maxGitMetadataFileBytes, true)
	if err != nil {
		return "", err
	}
	if hasCommonPointer {
		declaration, parseErr := parseCommonDirDeclaration(commonPointer.data)
		if parseErr != nil {
			return "", parseErr
		}
		commonDirPath, commonDirIdentity, err = resolvePhysicalGitDirectory(gitDirPath, declaration, "common Git directory")
		if err != nil {
			return "", err
		}
	}
	if err := revalidatePhysicalPath(gitDirIdentity); err != nil {
		return "", fmt.Errorf("Git directory changed while resolving object storage: %w", err)
	}
	if commonDirPath != gitDirPath {
		if err := revalidatePhysicalPath(commonDirIdentity); err != nil {
			return "", fmt.Errorf("common Git directory changed while resolving object storage: %w", err)
		}
	}
	if gitFile.info != nil {
		if err := revalidateStableRegularFile(gitFile.path, "repository .git gitfile", gitFile.info); err != nil {
			return "", err
		}
	}
	if hasCommonPointer {
		if err := revalidateStableRegularFile(commonPointer.path, "repository Git common-directory pointer", commonPointer.info); err != nil {
			return "", err
		}
	} else if err := revalidateMissingPath(commonPointerPath, "repository Git common-directory pointer"); err != nil {
		return "", err
	}
	objectsPath := filepath.Join(commonDirPath, "objects")
	identity, err := capturePhysicalPath(objectsPath)
	if err != nil {
		return "", fmt.Errorf("resolve repository common object directory: %w", err)
	}
	if err := revalidatePhysicalPath(identity); err != nil {
		return "", fmt.Errorf("repository common object directory changed while resolving: %w", err)
	}
	return identity.CanonicalPath, nil
}

func (service *Service) newBareRepositoryWithAlternates(ctx context.Context, tempRoot, objectFormat string, alternates []string) (_ string, returnErr error) {
	if err := validateTempRoot(tempRoot); err != nil {
		return "", err
	}
	if objectFormat != "sha1" && objectFormat != "sha256" {
		return "", fmt.Errorf("unsupported repository object format %q", objectFormat)
	}
	root, err := os.MkdirTemp(tempRoot, "dsx-source-repository-*")
	if err != nil {
		return "", fmt.Errorf("create temporary source repository: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			returnErr = errors.Join(returnErr, os.RemoveAll(root))
		}
	}()
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("secure temporary source repository: %w", err)
	}
	if err := service.runGit(ctx, "", nil, "init", "--bare", "--quiet", "--object-format="+objectFormat, root); err != nil {
		return "", fmt.Errorf("initialize temporary source repository: %w", err)
	}
	alternatesPath := filepath.Join(root, "objects", "info", "alternates")
	if err := writeAlternatesFile(alternatesPath, alternates); err != nil {
		return "", err
	}
	complete = true
	return root, nil
}
