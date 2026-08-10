package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/buildinfo"
	"github.com/srimajji/dsx/internal/config"
	projectinspect "github.com/srimajji/dsx/internal/inspect"
	"github.com/srimajji/dsx/internal/plan"
)

const (
	maxAuthorityEntries    = projectinspect.MaxEntries
	maxAuthorityDepth      = projectinspect.MaxDepth
	maxAuthorityFileBytes  = projectinspect.MaxFileBytes
	maxAuthorityTotalBytes = int64(512 << 20)

	standardBrowserImageReference = "dsx.local/browser:mvp@sha256:dce1d9a9cc9ad38edf545ad29a7f2f3448210a73be3a1cf3651d1c8932b023c0"
	standardBrowserImageDigest    = "dce1d9a9cc9ad38edf545ad29a7f2f3448210a73be3a1cf3651d1c8932b023c0"
)

func collectAuthorityInputs(root string, validated config.ValidatedConfig, imported []plan.ImportedValue, facts projectinspect.Facts, resolveMount HostMountResolver) (plan.AuthorityInputs, error) {
	browserReference, browserDigest, err := browserImageAuthority()
	if err != nil {
		return plan.AuthorityInputs{}, err
	}
	authority := plan.AuthorityInputs{
		BrowserImageReference: browserReference,
		BrowserImageDigest:    browserDigest,
	}
	if selection := validated.Document.Imports.Devcontainer; selection != nil {
		selected := normalizeProjectPath(selection.Path)
		for _, declaration := range facts.DevContainers {
			if normalizeProjectPath(declaration.Path) != selected {
				continue
			}
			authority.ImportedContent = append(authority.ImportedContent, plan.ContentDigest{Path: declaration.Path, Digest: declaration.ContentDigest})
			break
		}
	}
	sort.Slice(authority.ImportedContent, func(i, j int) bool {
		if authority.ImportedContent[i].Path != authority.ImportedContent[j].Path {
			return authority.ImportedContent[i].Path < authority.ImportedContent[j].Path
		}
		return authority.ImportedContent[i].Digest < authority.ImportedContent[j].Digest
	})

	if build := selectedBuild(validated.Document, imported); build != nil {
		digest, err := digestBuildInput(root, *build)
		if err != nil {
			return plan.AuthorityInputs{}, err
		}
		authority.BuildContext = &plan.ContentDigest{Path: build.Context, Digest: digest}
	}

	for _, mount := range selectedMounts(validated.Document, imported) {
		if mount.Source.Type != "host" {
			continue
		}
		resolved, err := resolveMount(mount.Source.Path)
		if err != nil {
			return plan.AuthorityInputs{}, fmt.Errorf("resolve host mount %q: %w", mount.Source.Path, err)
		}
		authority.HostMounts = append(authority.HostMounts, resolved)
	}
	if validated.Document.AWS.Mode == "leapp" {
		resolved, err := bridge.ResolveLeappDirectory(validated.Document.AWS.Directory)
		if err != nil {
			return plan.AuthorityInputs{}, fmt.Errorf("resolve Leapp directory %q: %w", validated.Document.AWS.Directory, err)
		}
		authority.LeappDirectory = &plan.HostMountAuthority{
			DeclaredPath:  resolved.DeclaredPath,
			CanonicalPath: resolved.CanonicalPath,
			Identity:      resolved.Identity,
		}
	}
	sort.Slice(authority.HostMounts, func(i, j int) bool {
		return authority.HostMounts[i].DeclaredPath < authority.HostMounts[j].DeclaredPath
	})
	return authority, nil
}

func browserImageAuthority() (string, string, error) {
	reference := buildinfo.BrowserImage
	if reference == "" || reference == "unknown" {
		if buildinfo.Version == "dev" {
			return standardBrowserImageReference, standardBrowserImageDigest, nil
		}
		return "", "", fmt.Errorf("published browser image release metadata is unavailable")
	}
	if strings.ContainsAny(reference, " \t\r\n") || strings.Count(reference, "@") != 1 {
		return "", "", fmt.Errorf("published browser image release metadata is malformed")
	}
	digest, ok := pinnedImageDigest(reference)
	if !ok {
		return "", "", fmt.Errorf("published browser image release metadata is not digest-pinned")
	}
	name := strings.TrimSuffix(reference, "@sha256:"+digest)
	slash := strings.IndexByte(name, '/')
	if slash <= 0 || slash == len(name)-1 {
		return "", "", fmt.Errorf("published browser image release metadata has no registry")
	}
	registry := name[:slash]
	if registry == "localhost" || registry == "dsx.local" || (!strings.ContainsRune(registry, '.') && !strings.ContainsRune(registry, ':')) {
		return "", "", fmt.Errorf("browser image release metadata must reference a published registry")
	}
	return reference, digest, nil
}

func selectedBuild(document config.ConfigDocument, imported []plan.ImportedValue) *config.ImageBuild {
	if document.Image.Ref != "" {
		return nil
	}
	if document.Image.Build != nil {
		build := *document.Image.Build
		return &build
	}
	for _, value := range imported {
		if value.Pointer == "/image/build" {
			if build, ok := value.Value.(config.ImageBuild); ok {
				return &build
			}
		}
	}
	return nil
}

func selectedMounts(document config.ConfigDocument, imported []plan.ImportedValue) []config.MountSpec {
	if len(document.Mounts) != 0 {
		return document.Mounts
	}
	var mounts []config.MountSpec
	for _, value := range imported {
		if value.Pointer == "/mounts" {
			if selected, ok := value.Value.([]config.MountSpec); ok {
				mounts = selected
			}
		}
	}
	return mounts
}

func resolveHostMount(source string) (plan.HostMountAuthority, error) {
	if err := config.ValidateHostMountPath(filepath.ToSlash(source)); err != nil {
		return plan.HostMountAuthority{}, err
	}
	if !filepath.IsAbs(source) {
		return plan.HostMountAuthority{}, fmt.Errorf("host mount source must be absolute")
	}
	clean := filepath.Clean(source)
	if err := rejectSymlinkComponents(clean); err != nil {
		return plan.HostMountAuthority{}, err
	}
	canonical, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return plan.HostMountAuthority{}, fmt.Errorf("canonicalize host mount: %w", err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return plan.HostMountAuthority{}, fmt.Errorf("absolutize host mount: %w", err)
	}
	canonical = filepath.Clean(canonical)
	if err := config.ValidateHostMountPath(filepath.ToSlash(canonical)); err != nil {
		return plan.HostMountAuthority{}, err
	}
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		return plan.HostMountAuthority{}, fmt.Errorf("resolve current user home for mount policy: %w", homeErr)
	}
	canonicalHome, homeErr := filepath.EvalSymlinks(filepath.Clean(home))
	if homeErr != nil {
		return plan.HostMountAuthority{}, fmt.Errorf("canonicalize current user home for mount policy: %w", homeErr)
	}
	canonicalHome, homeErr = filepath.Abs(canonicalHome)
	if homeErr != nil {
		return plan.HostMountAuthority{}, fmt.Errorf("absolutize current user home for mount policy: %w", homeErr)
	}
	canonicalHome = filepath.Clean(canonicalHome)
	if pathWithin(canonicalHome, canonical) || pathWithin(canonical, canonicalHome) {
		return plan.HostMountAuthority{}, fmt.Errorf("host mount is denied: current user home paths cannot be mounted")
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		return plan.HostMountAuthority{}, fmt.Errorf("inspect canonical host mount: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return plan.HostMountAuthority{}, fmt.Errorf("canonical host mount must not be a symlink")
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return plan.HostMountAuthority{}, fmt.Errorf("host mount must be a regular file or directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return plan.HostMountAuthority{}, fmt.Errorf("host mount has no stable filesystem identity")
	}
	identity := fmt.Sprintf("dev=%d;ino=%d;mode=%#o", stat.Dev, stat.Ino, uint32(info.Mode()))
	return plan.HostMountAuthority{DeclaredPath: source, CanonicalPath: canonical, Identity: identity}, nil
}

func rejectSymlinkComponents(absolute string) error {
	volume := filepath.VolumeName(absolute)
	current := volume + string(filepath.Separator)
	remainder := strings.TrimPrefix(absolute, current)
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect host mount component %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("host mount component %q is a symlink", current)
		}
	}
	return nil
}

var buildInputStageOpened func(string)

func digestBuildInput(projectRoot string, build config.ImageBuild) (string, error) {
	return digestBuildInputInto(context.Background(), projectRoot, build, "")
}

func stageBuildInput(ctx context.Context, projectRoot string, build config.ImageBuild) (string, string, error) {
	stageRoot, err := newPrivateBuildStage(projectRoot)
	if err != nil {
		return "", "", err
	}
	cleanup := func() {
		_ = os.RemoveAll(stageRoot)
	}
	copiedDigest, err := digestBuildInputInto(ctx, projectRoot, build, stageRoot)
	if err != nil {
		cleanup()
		return "", "", err
	}
	stagedDigest, err := digestBuildInputInto(ctx, stageRoot, build, "")
	if err != nil {
		cleanup()
		return "", "", fmt.Errorf("verify staged build input: %w", err)
	}
	if copiedDigest != stagedDigest {
		cleanup()
		return "", "", fmt.Errorf("staged build input digest mismatch: copied %s, staged %s", copiedDigest, stagedDigest)
	}
	return stageRoot, stagedDigest, nil
}

func newPrivateBuildStage(projectRoot string) (string, error) {
	bases := []string{"", filepath.Dir(projectRoot)}
	var lastErr error
	for _, base := range bases {
		stageRoot, err := os.MkdirTemp(base, ".dsx-build-")
		if err != nil {
			lastErr = err
			continue
		}
		if pathWithin(projectRoot, stageRoot) {
			_ = os.RemoveAll(stageRoot)
			lastErr = fmt.Errorf("temporary build directory is inside the project")
			continue
		}
		if err := os.Chmod(stageRoot, 0o700); err != nil {
			_ = os.RemoveAll(stageRoot)
			lastErr = err
			continue
		}
		return stageRoot, nil
	}
	return "", fmt.Errorf("create private build staging directory outside project: %w", lastErr)
}

func digestBuildInputInto(ctx context.Context, projectRoot string, build config.ImageBuild, stageRoot string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	contextPath, err := projectAbsolutePath(projectRoot, build.Context)
	if err != nil {
		return "", fmt.Errorf("build context: %w", err)
	}
	if err := rejectProjectSymlinkComponents(projectRoot, contextPath); err != nil {
		return "", fmt.Errorf("build context: %w", err)
	}
	contextInfo, err := os.Lstat(contextPath)
	if err != nil {
		return "", fmt.Errorf("inspect build context: %w", err)
	}
	if !contextInfo.IsDir() {
		return "", fmt.Errorf("build context %q is not a directory", build.Context)
	}
	if stageRoot != "" {
		stagedContext, err := projectAbsolutePath(stageRoot, build.Context)
		if err != nil {
			return "", fmt.Errorf("staged build context: %w", err)
		}
		if err := os.MkdirAll(stagedContext, 0o700); err != nil {
			return "", fmt.Errorf("create staged build context: %w", err)
		}
	}

	digest := sha256.New()
	_, _ = digest.Write([]byte("dsx.build-input/v1\x00"))
	entries := 0
	var totalBytes int64
	err = filepath.WalkDir(contextPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(contextPath, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		entries++
		if entries > maxAuthorityEntries {
			return fmt.Errorf("entry limit %d exceeded", maxAuthorityEntries)
		}
		if pathDepth(rel) > maxAuthorityDepth {
			return fmt.Errorf("depth limit %d exceeded at %q", maxAuthorityDepth, filepath.ToSlash(rel))
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink %q is not allowed", filepath.ToSlash(rel))
		}
		destination, err := authorityStagePath(projectRoot, stageRoot, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if destination == "" {
				return nil
			}
			return os.MkdirAll(destination, 0o700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("special file %q is not allowed", filepath.ToSlash(rel))
		}
		return hashAuthorityFileInto(ctx, digest, path, filepath.ToSlash(rel), &totalBytes, destination)
	})
	if err != nil {
		return "", fmt.Errorf("hash build context %q: %w", build.Context, err)
	}

	filePath, err := projectAbsolutePath(projectRoot, build.File)
	if err != nil {
		return "", fmt.Errorf("build file: %w", err)
	}
	if err := rejectProjectSymlinkComponents(projectRoot, filePath); err != nil {
		return "", fmt.Errorf("build file: %w", err)
	}
	fileInfo, err := os.Lstat(filePath)
	if err != nil {
		return "", fmt.Errorf("inspect build file %q: %w", build.File, err)
	}
	if !fileInfo.Mode().IsRegular() {
		return "", fmt.Errorf("build file %q is not a regular file", build.File)
	}
	if !pathWithin(contextPath, filePath) {
		destination, err := authorityStagePath(projectRoot, stageRoot, filePath)
		if err != nil {
			return "", err
		}
		if err := hashAuthorityFileInto(ctx, digest, filePath, "@dockerfile/"+normalizeProjectPath(build.File), &totalBytes, destination); err != nil {
			return "", fmt.Errorf("hash build file %q: %w", build.File, err)
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func authorityStagePath(projectRoot, stageRoot, sourcePath string) (string, error) {
	if stageRoot == "" {
		return "", nil
	}
	relative, err := filepath.Rel(projectRoot, sourcePath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("build input path escapes project root")
	}
	return filepath.Join(stageRoot, relative), nil
}

func hashAuthorityFile(digest hash.Hash, path, relative string, totalBytes *int64) error {
	return hashAuthorityFileInto(context.Background(), digest, path, relative, totalBytes, "")
}

func hashAuthorityFileInto(ctx context.Context, digest hash.Hash, path, relative string, totalBytes *int64, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if before.Size() > maxAuthorityFileBytes {
		return fmt.Errorf("file size limit %d exceeded", maxAuthorityFileBytes)
	}
	if *totalBytes > maxAuthorityTotalBytes-before.Size() {
		return fmt.Errorf("total byte limit %d exceeded", maxAuthorityTotalBytes)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !sameAuthorityFile(before, opened) || !opened.Mode().IsRegular() {
		return fmt.Errorf("file changed during authority inspection")
	}
	if destination != "" && buildInputStageOpened != nil {
		buildInputStageOpened(path)
	}

	_, _ = fmt.Fprintf(digest, "%d:%s:%#o:%d:", len(relative), relative, uint32(opened.Mode()), opened.Size())
	writer := io.Writer(digest)
	var staged *os.File
	if destination != "" {
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		stagedFD, err := unix.Open(destination, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		staged = os.NewFile(uintptr(stagedFD), destination)
		defer func() {
			if staged != nil {
				_ = staged.Close()
			}
		}()
		writer = io.MultiWriter(digest, staged)
	}
	written, err := io.Copy(writer, io.LimitReader(file, maxAuthorityFileBytes+1))
	if err != nil {
		return err
	}
	if written != opened.Size() {
		return fmt.Errorf("file size changed during authority inspection")
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	pathAfter, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("file changed during authority inspection: %w", err)
	}
	if !sameAuthorityFile(opened, after) || !sameAuthorityFile(opened, pathAfter) {
		return fmt.Errorf("file changed during authority inspection")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if staged != nil {
		if err := staged.Chmod(opened.Mode()); err != nil {
			return err
		}
		if err := staged.Close(); err != nil {
			return err
		}
		staged = nil
	}
	_, _ = digest.Write([]byte{0})
	*totalBytes += written
	return nil
}

func sameAuthorityFile(expected, actual os.FileInfo) bool {
	return os.SameFile(expected, actual) &&
		expected.Size() == actual.Size() &&
		expected.Mode() == actual.Mode() &&
		expected.ModTime().Equal(actual.ModTime())
}

func projectAbsolutePath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(filepath.FromSlash(relative)) {
		return "", fmt.Errorf("path %q is not project-relative", relative)
	}
	absolute := filepath.Clean(filepath.Join(root, filepath.FromSlash(relative)))
	if !pathWithin(root, absolute) {
		return "", fmt.Errorf("path %q escapes project root", relative)
	}
	return absolute, nil
}

func rejectProjectSymlinkComponents(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes project root")
	}
	current := root
	if relative == "." {
		return nil
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("component %q is a symlink", filepath.ToSlash(relative))
		}
	}
	return nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func pathDepth(relative string) int {
	if relative == "." || relative == "" {
		return 0
	}
	return strings.Count(filepath.Clean(relative), string(filepath.Separator)) + 1
}
