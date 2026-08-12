package auth

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/srimajji/dsx/internal/harness"
)

// HostImportState is safe to render; it never contains a host path or secret value.
type HostImportState string

const (
	HostImportAvailable     HostImportState = "available"
	HostImportUnavailable   HostImportState = "unavailable"
	HostImportLoginRequired HostImportState = "login_required"
	HostImportInvalid       HostImportState = "invalid"
)

// HostDiscovery discovers only fixed, harness-specific portable artifacts below
// one real home directory. It never walks or copies a complete harness directory.
type HostDiscovery struct {
	home string
}

type HostSource struct {
	harness   harness.Name
	home      string
	directory string
	artifacts []hostArtifact
	forbidden []string
}

type hostArtifact struct {
	path     string
	required bool
}

func NewHostDiscovery(home string) (*HostDiscovery, error) {
	if home == "" || !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return nil, errors.New("host credential home must be a clean absolute path")
	}
	info, err := os.Lstat(home)
	if err != nil {
		return nil, errors.New("host credential home is unavailable")
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("host credential home must be a real directory")
	}
	return &HostDiscovery{home: home}, nil
}

func (discovery *HostDiscovery) Status(ctx context.Context, name harness.Name, maxArtifactBytes int64) HostImportState {
	if name == harness.Claude {
		return HostImportLoginRequired
	}
	source, err := discovery.Discover(name)
	if err != nil {
		return HostImportUnavailable
	}
	if err := source.validate(ctx, maxArtifactBytes); err != nil {
		return HostImportInvalid
	}
	return HostImportAvailable
}

func (discovery *HostDiscovery) Discover(name harness.Name) (HostSource, error) {
	if discovery == nil {
		return HostSource{}, errors.New("host credential discovery is unavailable")
	}
	if _, err := harness.ParseName(string(name)); err != nil {
		return HostSource{}, err
	}
	switch name {
	case harness.OMP:
		return HostSource{
			harness:   name,
			home:      discovery.home,
			directory: ".omp/agent",
			artifacts: []hostArtifact{
				{path: "agent.db", required: true},
				{path: "agent.db-wal", required: false},
			},
			forbidden: []string{"agent.db-shm"},
		}, nil
	case harness.Codex:
		return HostSource{harness: name, home: discovery.home, directory: ".codex", artifacts: []hostArtifact{{path: "auth.json", required: true}}}, nil
	case harness.OpenCode:
		return HostSource{harness: name, home: discovery.home, directory: ".local/share", artifacts: []hostArtifact{{path: "opencode/auth.json", required: true}}}, nil
	case harness.Claude:
		return HostSource{}, errors.New("Claude host authentication is not portable; DSX login is required")
	default:
		return HostSource{}, errors.New("unsupported host credential import")
	}
}

func (source HostSource) validate(ctx context.Context, maxArtifactBytes int64) error {
	files, err := source.open(ctx, maxArtifactBytes)
	if err != nil {
		return err
	}
	for _, file := range files {
		_ = file.file.Close()
	}
	return nil
}

type openedHostArtifact struct {
	artifact hostArtifact
	file     *os.File
	before   os.FileInfo
}

func (source HostSource) open(ctx context.Context, maxArtifactBytes int64) ([]openedHostArtifact, error) {
	if maxArtifactBytes <= 0 || maxArtifactBytes > harness.MaxAuthArtifactBytes {
		return nil, errors.New("host credential artifact size limit is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := source.openDirectory()
	if err != nil {
		return nil, errors.New("host credential directory is unavailable or unsafe")
	}
	_ = unix.Close(directory)
	for _, forbidden := range source.forbidden {
		file, err := source.openArtifact(forbidden)
		if err == nil {
			_ = file.Close()
			return nil, errors.New("host credential database is active; close OMP before importing")
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOENT) {
			return nil, errors.New("host credential database activity cannot be verified")
		}
	}

	result := make([]openedHostArtifact, 0, len(source.artifacts))
	closeResult := func() {
		for _, artifact := range result {
			_ = artifact.file.Close()
		}
	}
	for _, artifact := range source.artifacts {
		if err := ctx.Err(); err != nil {
			closeResult()
			return nil, err
		}
		file, err := source.openArtifact(artifact.path)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
			if artifact.required {
				closeResult()
				return nil, errors.New("required host credential artifact is unavailable")
			}
			continue
		}
		if err != nil {
			closeResult()
			return nil, errors.New("host credential artifact is unsafe")
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			closeResult()
			return nil, errors.New("host credential artifact is not a regular file")
		}
		if info.Size() > maxArtifactBytes {
			_ = file.Close()
			closeResult()
			return nil, errors.New("host credential artifact exceeds its size limit")
		}
		result = append(result, openedHostArtifact{artifact: artifact, file: file, before: info})
	}
	for _, forbidden := range source.forbidden {
		file, err := source.openArtifact(forbidden)
		if err == nil {
			_ = file.Close()
			closeResult()
			return nil, errors.New("host credential database changed while snapshotting")
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOENT) {
			closeResult()
			return nil, errors.New("host credential database activity cannot be verified")
		}
	}
	return result, nil
}

// ImportHost snapshots only the discovered allowlist into a restrictive private
// directory before adapter validation and atomic canonical persistence.
func (repository *Repository) ImportHost(ctx context.Context, project Project, source HostSource, replace bool, seeder Seeder) (string, error) {
	if err := validateProject(project); err != nil {
		return "", err
	}
	if source.harness != project.Harness {
		return "", errors.New("host credential source does not match the selected harness")
	}
	layout, err := validateSeeder(seeder)
	if err != nil {
		return "", err
	}
	if err := source.matchesLayout(layout); err != nil {
		return "", err
	}
	snapshot, err := repository.snapshotHost(ctx, source, layout.MaxArtifactBytes)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(snapshot)
	return repository.ImportProject(ctx, project, snapshot, replace, seeder)
}

func (source HostSource) matchesLayout(layout harness.AuthLayout) error {
	allowed := append([]string(nil), layout.CredentialArtifacts...)
	sort.Strings(allowed)
	discovered := make([]string, len(source.artifacts))
	for index, artifact := range source.artifacts {
		discovered[index] = artifact.path
	}
	sort.Strings(discovered)
	if strings.Join(allowed, "\x00") != strings.Join(discovered, "\x00") {
		return errors.New("host credential allowlist does not match the pinned harness layout")
	}
	return nil
}

func (repository *Repository) snapshotHost(ctx context.Context, source HostSource, maxArtifactBytes int64) (string, error) {
	opened, err := source.open(ctx, maxArtifactBytes)
	if err != nil {
		return "", err
	}
	defer func() {
		for _, artifact := range opened {
			_ = artifact.file.Close()
		}
	}()

	destination, err := os.MkdirTemp(repository.root, ".host-import-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		_ = os.RemoveAll(destination)
		return "", err
	}
	remove := true
	defer func() {
		if remove {
			_ = os.RemoveAll(destination)
		}
	}()

	for _, artifact := range opened {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		path := filepath.Join(destination, filepath.FromSlash(artifact.artifact.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", err
		}
		copied, copyErr := io.Copy(output, io.LimitReader(artifact.file, maxArtifactBytes+1))
		syncErr := output.Sync()
		closeErr := output.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil {
			return "", errors.Join(copyErr, syncErr, closeErr)
		}
		if copied > maxArtifactBytes {
			return "", errors.New("host credential artifact exceeds its size limit")
		}
		after, err := artifact.file.Stat()
		if err != nil || !os.SameFile(artifact.before, after) || after.Size() != artifact.before.Size() || !after.ModTime().Equal(artifact.before.ModTime()) || copied != after.Size() {
			return "", errors.New("host credential artifact changed while snapshotting")
		}
	}
	for _, forbidden := range source.forbidden {
		file, err := source.openArtifact(forbidden)
		if err == nil {
			_ = file.Close()
			return "", errors.New("host credential database changed while snapshotting")
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOENT) {
			return "", errors.New("host credential database activity cannot be verified")
		}
	}
	if err := syncDirectory(destination); err != nil {
		return "", err
	}
	remove = false
	return destination, nil
}

func (source HostSource) openDirectory() (int, error) {
	if source.home == "" || !filepath.IsAbs(source.home) || filepath.Clean(source.home) != source.home {
		return -1, errors.New("invalid host credential home")
	}
	directory, err := unix.Open(source.home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	if source.directory == "" || filepath.IsAbs(source.directory) || filepath.Clean(source.directory) != source.directory || strings.Contains(source.directory, "\x00") {
		_ = unix.Close(directory)
		return -1, errors.New("invalid host credential directory")
	}
	for _, component := range strings.Split(filepath.ToSlash(source.directory), "/") {
		next, openErr := unix.Openat(directory, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(directory)
		if openErr != nil {
			return -1, openErr
		}
		directory = next
	}
	return directory, nil
}

func (source HostSource) openArtifact(relative string) (*os.File, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || strings.Contains(relative, "\x00") {
		return nil, errors.New("invalid host credential artifact name")
	}
	directory, err := source.openDirectory()
	if err != nil {
		return nil, err
	}
	components := strings.Split(filepath.ToSlash(relative), "/")
	for _, component := range components[:len(components)-1] {
		next, openErr := unix.Openat(directory, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(directory)
		if openErr != nil {
			return nil, openErr
		}
		directory = next
	}
	fileDescriptor, err := unix.Openat(directory, components[len(components)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	_ = unix.Close(directory)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fileDescriptor), "credential-artifact")
	if file == nil {
		_ = unix.Close(fileDescriptor)
		return nil, errors.New("open host credential artifact")
	}
	return file, nil
}
