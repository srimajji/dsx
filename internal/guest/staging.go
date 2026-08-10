package guest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"golang.org/x/sys/unix"
)

var allowedRunRoots = map[string]struct{}{
	"home": {}, "auth": {}, "config": {}, "data": {}, "cache": {}, "tmp": {},
}

// EnsureRunDirectory securely creates and verifies one directory beneath an
// authorized per-run root. Every DSX-owned component is opened without
// following symlinks and remains anchored by descriptors until the complete
// chain has been revalidated.
func EnsureRunDirectory(name string) error {
	components, err := authorizedRunComponents(name, false)
	if err != nil {
		return err
	}
	tmpFD, err := openTemporaryRoot()
	if err != nil {
		return err
	}
	defer unix.Close(tmpFD)
	chain, err := openDirectoryChain(tmpFD, components, true, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		return err
	}
	defer chain.close()
	return chain.revalidate(tmpFD)
}

// StageRunFile creates a new private file from bounded stdin beneath an
// already verified auth or config root. Existing files are never replaced.
func StageRunFile(name string, input io.Reader, maxArtifactBytes int64) error {
	components, err := authorizedRunComponents(name, true)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("staged file input is required")
	}
	if err := validateArtifactSizeLimit(maxArtifactBytes); err != nil {
		return err
	}
	contents, err := io.ReadAll(io.LimitReader(input, maxArtifactBytes+1))
	if err != nil {
		return fmt.Errorf("read staged file input: %w", err)
	}
	if int64(len(contents)) > maxArtifactBytes {
		return errors.New("staged file exceeds size limit")
	}
	return stagePrivateContents(components, name, contents, maxArtifactBytes)
}

// StageReadOnlyRunFile creates a root-owned immutable-by-child configuration
// file beneath a separate read-only configuration tree.
func StageReadOnlyRunFile(name string, input io.Reader, maxArtifactBytes int64, childUID, childGID uint32) error {
	components, err := authorizedReadOnlyRunComponents(name)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("staged file input is required")
	}
	if err := validateArtifactSizeLimit(maxArtifactBytes); err != nil {
		return err
	}
	if os.Geteuid() != 0 || childUID == 0 || childGID == 0 {
		return errors.New("read-only staging requires root and a non-root child identity")
	}
	contents, err := io.ReadAll(io.LimitReader(input, maxArtifactBytes+1))
	if err != nil {
		return fmt.Errorf("read staged file input: %w", err)
	}
	if int64(len(contents)) > maxArtifactBytes {
		return errors.New("staged file exceeds size limit")
	}
	tmpFD, err := openTemporaryRoot()
	if err != nil {
		return err
	}
	defer unix.Close(tmpFD)
	rootChain, err := openDirectoryChainMode(tmpFD, components[:len(components)-1], true, 0, 0, 0o555)
	if err != nil {
		return err
	}
	defer rootChain.close()
	if err := rootChain.revalidate(tmpFD); err != nil {
		return err
	}
	parentFD := rootChain[len(rootChain)-1].fd
	leaf := components[len(components)-1]
	fd, err := unix.Openat(parentFD, leaf, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o444)
	if err != nil {
		return fmt.Errorf("create read-only staged file: %w", err)
	}
	cleanup := true
	defer func() {
		_ = unix.Close(fd)
		if cleanup {
			_ = unix.Unlinkat(parentFD, leaf, 0)
		}
	}()
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		return errors.New("create read-only staged file: invalid descriptor")
	}
	written, err := file.Write(contents)
	if err != nil || written != len(contents) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return fmt.Errorf("write read-only staged file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync read-only staged file: %w", err)
	}
	var metadata unix.Stat_t
	if err := unix.Fstat(fd, &metadata); err != nil {
		return fmt.Errorf("inspect read-only staged file: %w", err)
	}
	if err := validateOwnedFile(metadata, 0, 0, 0o444, int64(len(contents))); err != nil {
		return err
	}
	var linked unix.Stat_t
	if err := unix.Fstatat(parentFD, leaf, &linked, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("revalidate read-only staged file: %w", err)
	}
	if linked.Dev != metadata.Dev || linked.Ino != metadata.Ino || linked.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("read-only staged file changed during operation")
	}
	if err := rootChain.revalidate(tmpFD); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close read-only staged file: %w", err)
	}
	fd = -1
	cleanup = false
	return nil
}

type openedDirectory struct {
	fd       int
	name     string
	metadata unix.Stat_t
}

// RemoveReadOnlyRunRoot removes one exact root-owned read-only invocation tree
// after verifying every descendant through no-follow descriptors.
func RemoveReadOnlyRunRoot(name string) error {
	components, err := authorizedReadOnlyRootComponents(name)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("read-only cleanup requires root")
	}
	tmpFD, err := openTemporaryRoot()
	if err != nil {
		return err
	}
	defer unix.Close(tmpFD)
	chain, err := openDirectoryChainMode(tmpFD, components, false, 0, 0, 0o555)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer chain.close()
	if err := chain.revalidate(tmpFD); err != nil {
		return err
	}
	remaining := 128
	runFD := chain[len(chain)-1].fd
	if err := removeReadOnlyContents(runFD, 0, &remaining); err != nil {
		return err
	}
	if err := chain.revalidate(tmpFD); err != nil {
		return err
	}
	parentFD := chain[len(chain)-2].fd
	if err := unix.Unlinkat(parentFD, components[len(components)-1], unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove read-only run root: %w", err)
	}
	return nil
}

func removeReadOnlyContents(directoryFD, depth int, remaining *int) error {
	if depth > 8 || remaining == nil || *remaining < 0 {
		return errors.New("read-only config tree exceeds cleanup bounds")
	}
	duplicate, err := unix.Dup(directoryFD)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(duplicate), "read-only-config")
	if directory == nil {
		_ = unix.Close(duplicate)
		return errors.New("inspect read-only config directory: invalid descriptor")
	}
	entries, readErr := directory.ReadDir(129)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if len(entries) > 128 || len(entries) > *remaining {
		return errors.New("read-only config tree exceeds cleanup bounds")
	}
	*remaining -= len(entries)
	for _, entry := range entries {
		name := entry.Name()
		if !validRunPathComponent(name) {
			return errors.New("read-only config tree contains an unsafe name")
		}
		var metadata unix.Stat_t
		if err := unix.Fstatat(directoryFD, name, &metadata, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch metadata.Mode & unix.S_IFMT {
		case unix.S_IFREG:
			if err := validateOwnedFile(metadata, 0, 0, 0o444, metadata.Size); err != nil {
				return err
			}
			if err := unix.Unlinkat(directoryFD, name, 0); err != nil {
				return err
			}
		case unix.S_IFDIR:
			if err := validateOwnedDirectory(metadata, 0, 0, 0o555); err != nil {
				return err
			}
			childFD, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			removeErr := removeReadOnlyContents(childFD, depth+1, remaining)
			closeErr := unix.Close(childFD)
			if removeErr != nil || closeErr != nil {
				return errors.Join(removeErr, closeErr)
			}
			if err := unix.Unlinkat(directoryFD, name, unix.AT_REMOVEDIR); err != nil {
				return err
			}
		default:
			return errors.New("read-only config tree contains a non-file entry")
		}
	}
	return nil
}

type directoryChain []openedDirectory

func (chain directoryChain) close() {
	for index := len(chain) - 1; index >= 0; index-- {
		_ = unix.Close(chain[index].fd)
	}
}

func (chain directoryChain) revalidate(rootFD int) error {
	parentFD := rootFD
	for _, directory := range chain {
		var metadata unix.Stat_t
		if err := unix.Fstatat(parentFD, directory.name, &metadata, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("revalidate guest directory %q: %w", directory.name, err)
		}
		if metadata.Mode&unix.S_IFMT != unix.S_IFDIR || metadata.Dev != directory.metadata.Dev || metadata.Ino != directory.metadata.Ino {
			return fmt.Errorf("guest directory %q changed during operation", directory.name)
		}
		parentFD = directory.fd
	}
	return nil
}

func openTemporaryRoot() (int, error) {
	fd, err := unix.Open(guestTemporaryRootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open guest temporary root: %w", err)
	}
	return fd, nil
}

func openDirectoryChain(rootFD int, components []string, create bool, uid, gid uint32) (directoryChain, error) {
	return openDirectoryChainMode(rootFD, components, create, uid, gid, 0o700)
}

func openDirectoryChainMode(rootFD int, components []string, create bool, uid, gid uint32, mode uint16) (directoryChain, error) {
	chain := make(directoryChain, 0, len(components))
	parentFD := rootFD
	for _, component := range components {
		if create {
			if err := unix.Mkdirat(parentFD, component, uint32(mode)); err != nil && !errors.Is(err, unix.EEXIST) {
				chain.close()
				return nil, fmt.Errorf("create guest directory %q: %w", component, err)
			}
		}
		fd, err := unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			chain.close()
			return nil, fmt.Errorf("open guest directory %q: %w", component, err)
		}
		var metadata unix.Stat_t
		if err := unix.Fstat(fd, &metadata); err != nil {
			_ = unix.Close(fd)
			chain.close()
			return nil, fmt.Errorf("inspect guest directory %q: %w", component, err)
		}
		if err := validateOwnedDirectory(metadata, uid, gid, mode); err != nil {
			_ = unix.Close(fd)
			chain.close()
			return nil, fmt.Errorf("unsafe guest directory %q: %w", component, err)
		}
		chain = append(chain, openedDirectory{fd: fd, name: component, metadata: metadata})
		parentFD = fd
	}
	return chain, nil
}

func validatePrivateDirectory(metadata unix.Stat_t, uid, gid uint32) error {
	return validateOwnedDirectory(metadata, uid, gid, 0o700)
}

func validateOwnedDirectory(metadata unix.Stat_t, uid, gid uint32, mode uint16) error {
	if metadata.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("not a directory")
	}
	if metadata.Uid != uid || metadata.Gid != gid {
		return errors.New("owner does not match required identity")
	}
	if uint32(metadata.Mode)&0o7777 != uint32(mode) {
		return fmt.Errorf("mode must be %04o", mode)
	}
	return nil
}

func stagePrivateContents(components []string, displayName string, contents []byte, maxArtifactBytes int64) error {
	if len(components) < 2 {
		return errors.New("staged file path has no private parent")
	}
	tmpFD, err := openTemporaryRoot()
	if err != nil {
		return err
	}
	defer unix.Close(tmpFD)
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	parents := components[:len(components)-1]
	leaf := components[len(components)-1]
	chain, err := openDirectoryChain(tmpFD, parents, false, uid, gid)
	if err != nil {
		return err
	}
	defer chain.close()
	if err := chain.revalidate(tmpFD); err != nil {
		return err
	}
	parentFD := chain[len(chain)-1].fd
	fd, err := unix.Openat(parentFD, leaf, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create staged file: %w", err)
	}
	cleanup := true
	defer func() {
		_ = unix.Close(fd)
		if cleanup {
			_ = unix.Unlinkat(parentFD, leaf, 0)
		}
	}()
	file := os.NewFile(uintptr(fd), displayName)
	if file == nil {
		return errors.New("create staged file: invalid descriptor")
	}
	written, err := file.Write(contents)
	if err != nil || written != len(contents) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return fmt.Errorf("write staged file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync staged file: %w", err)
	}
	var metadata unix.Stat_t
	if err := unix.Fstat(fd, &metadata); err != nil {
		return fmt.Errorf("inspect staged file: %w", err)
	}
	if err := validatePrivateFile(metadata, uid, gid, int64(len(contents)), maxArtifactBytes); err != nil {
		return err
	}
	var linked unix.Stat_t
	if err := unix.Fstatat(parentFD, leaf, &linked, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("revalidate staged file: %w", err)
	}
	if linked.Dev != metadata.Dev || linked.Ino != metadata.Ino || linked.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("staged file changed during operation")
	}
	if err := chain.revalidate(tmpFD); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close staged file: %w", err)
	}
	fd = -1
	cleanup = false
	return nil
}

func validatePrivateFile(metadata unix.Stat_t, uid, gid uint32, size, maxArtifactBytes int64) error {
	if size < 0 || size > maxArtifactBytes {
		return errors.New("staged file size exceeds limit")
	}
	return validateOwnedFile(metadata, uid, gid, 0o600, size)
}

func validateOwnedFile(metadata unix.Stat_t, uid, gid uint32, mode uint16, size int64) error {
	if metadata.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("staged file is not regular")
	}
	if metadata.Uid != uid || metadata.Gid != gid {
		return errors.New("staged file owner does not match required identity")
	}
	if uint32(metadata.Mode)&0o7777 != uint32(mode) {
		return fmt.Errorf("staged file mode must be %04o", mode)
	}
	if metadata.Size != size || metadata.Size < 0 {
		return errors.New("staged file size does not match")
	}
	return nil
}

func validateArtifactSizeLimit(maxArtifactBytes int64) error {
	if maxArtifactBytes <= 0 || maxArtifactBytes > harness.MaxAuthArtifactBytes {
		return fmt.Errorf("artifact size limit must be between 1 and %d bytes", harness.MaxAuthArtifactBytes)
	}
	return nil
}

func authorizedReadOnlyRunComponents(name string) ([]string, error) {
	if name == "" || len(name) > 512 || path.Clean(name) != name {
		return nil, errors.New("read-only guest config path is not authorized")
	}
	parts := strings.Split(name, "/")
	if len(parts) < 5 || parts[0] != "" || parts[1] != "tmp" || parts[2] != "dsx-readonly" {
		return nil, errors.New("read-only guest config path is not authorized")
	}
	runID, err := model.ParseRunID(parts[3])
	if err != nil || string(runID) != parts[3] {
		return nil, errors.New("read-only guest config path is not authorized")
	}
	for _, component := range parts[2:] {
		if !validRunPathComponent(component) {
			return nil, errors.New("read-only guest config path is not authorized")
		}
	}
	return append([]string(nil), parts[2:]...), nil
}

func authorizedReadOnlyRootComponents(name string) ([]string, error) {
	if name == "" || len(name) > 256 || path.Clean(name) != name {
		return nil, errors.New("read-only guest cleanup path is not authorized")
	}
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "tmp" || parts[2] != "dsx-readonly" {
		return nil, errors.New("read-only guest cleanup path is not authorized")
	}
	runID, err := model.ParseRunID(parts[3])
	if err != nil || string(runID) != parts[3] {
		return nil, errors.New("read-only guest cleanup path is not authorized")
	}
	return []string{parts[2], parts[3]}, nil
}

func authorizedRunComponents(name string, file bool) ([]string, error) {
	if name == "" || len(name) > 512 || path.Clean(name) != name {
		return nil, errors.New("guest run path is not authorized")
	}
	parts := strings.Split(name, "/")
	if len(parts) < 4 || parts[0] != "" || parts[1] != "tmp" || parts[2] != "dsx-run" {
		return nil, errors.New("guest run path is not authorized")
	}
	runID, err := model.ParseRunID(parts[3])
	if err != nil || string(runID) != parts[3] {
		return nil, errors.New("guest run path is not authorized")
	}
	if len(parts) > 4 {
		if _, allowed := allowedRunRoots[parts[4]]; !allowed {
			return nil, errors.New("guest run path is not authorized")
		}
	}
	if file {
		if len(parts) < 6 || parts[4] != "auth" && parts[4] != "config" {
			return nil, errors.New("staged file path is not authorized")
		}
	} else if len(parts) < 5 {
		return nil, errors.New("guest run directory is not authorized")
	}
	for _, component := range parts[2:] {
		if !validRunPathComponent(component) {
			return nil, errors.New("guest run path is not authorized")
		}
	}
	return append([]string(nil), parts[2:]...), nil
}

func validRunPathComponent(component string) bool {
	if component == "" || component == "." || component == ".." || len(component) > 255 {
		return false
	}
	for _, character := range component {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
