package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/srimajji/dsx/internal/runtime"
	"golang.org/x/sys/unix"
)

const maxGuestHelperCacheEntries = 4

// StageGuestHelper copies the installed helper into a private, content-addressed
// cache directory that contains no sibling files. The returned directory is
// the only host path later exposed to the guest.
func StageGuestHelper(source runtime.HostPath, cacheRoot string) (runtime.HostPath, error) {
	return stageGuestHelper(source, cacheRoot, "")
}

// StageVerifiedGuestHelper stages source only when its bytes match the
// release digest compiled into the host executable.
func StageVerifiedGuestHelper(source runtime.HostPath, cacheRoot, expectedSHA256 string) (runtime.HostPath, error) {
	decoded, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(decoded) != sha256.Size || expectedSHA256 != strings.ToLower(expectedSHA256) {
		return "", errors.New("release guest helper SHA-256 is missing or invalid")
	}
	return stageGuestHelper(source, cacheRoot, expectedSHA256)
}

func stageGuestHelper(source runtime.HostPath, cacheRoot, expectedSHA256 string) (runtime.HostPath, error) {
	if err := validateGuestHelperSource(source); err != nil {
		return "", err
	}
	if filepath.Base(string(source)) != "dsx-guest" {
		return "", errors.New("guest helper must be installed as dsx-guest")
	}
	if err := ensurePrivateHostDirectory(cacheRoot); err != nil {
		return "", fmt.Errorf("prepare guest helper cache: %w", err)
	}
	lock, err := acquireGuestHelperCacheLock(cacheRoot)
	if err != nil {
		return "", fmt.Errorf("lock guest helper cache: %w", err)
	}
	defer lock.release()
	sourceInfo, err := os.Lstat(string(source))
	if err != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Mode().Perm()&0o111 == 0 {
		return "", errors.New("guest helper source is not a stable executable regular file")
	}
	input, err := os.Open(string(source))
	if err != nil {
		return "", err
	}
	defer input.Close()
	openedInfo, err := input.Stat()
	if err != nil || !os.SameFile(sourceInfo, openedInfo) || !openedInfo.Mode().IsRegular() {
		return "", errors.New("guest helper source changed before staging")
	}
	sourceDigest, err := digestOpenFile(input)
	if err != nil {
		return "", fmt.Errorf("digest guest helper source: %w", err)
	}
	if expectedSHA256 != "" && sourceDigest != expectedSHA256 {
		return "", errors.New("installed guest helper does not match release SHA-256")
	}
	directory := filepath.Join(cacheRoot, "sha256-"+sourceDigest)
	helper := filepath.Join(directory, "dsx-guest")
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		if err := stageGuestHelperDirectory(input, cacheRoot, directory, sourceDigest); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if err := verifyGuestHelperDirectory(directory, sourceDigest); err != nil {
		return "", fmt.Errorf("verify staged guest helper: %w", err)
	}
	if err := pruneGuestHelperCache(cacheRoot, filepath.Base(directory)); err != nil {
		return "", fmt.Errorf("prune guest helper cache: %w", err)
	}
	return runtime.HostPath(helper), nil
}

type guestHelperCacheLock struct {
	directoryFD int
	file        *os.File
}

func acquireGuestHelperCacheLock(cacheRoot string) (*guestHelperCacheLock, error) {
	directoryFD, err := unix.Open(cacheRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open cache directory: %w", err)
	}
	fd := -1
	for attempt := range 32 {
		fd, err = unix.Openat(directoryFD, ".lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.ENOENT) {
			_ = unix.Close(directoryFD)
			return nil, fmt.Errorf("open cache lock: %w", err)
		}
		if attempt < 31 {
			time.Sleep(time.Millisecond)
		}
	}
	if err != nil {
		_ = unix.Close(directoryFD)
		return nil, fmt.Errorf("open cache lock after concurrent initialization: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = unix.Close(fd)
		_ = unix.Close(directoryFD)
		return nil, err
	}
	var metadata unix.Stat_t
	if err := unix.Fstat(fd, &metadata); err != nil ||
		metadata.Mode&unix.S_IFMT != unix.S_IFREG ||
		metadata.Mode&0o7777 != 0o600 ||
		int(metadata.Uid) != os.Getuid() || int(metadata.Gid) != os.Getgid() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
		_ = unix.Close(directoryFD)
		return nil, errors.New("guest helper cache lock owner or mode is unsafe")
	}
	var linked unix.Stat_t
	if err := unix.Fstatat(directoryFD, ".lock", &linked, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
		_ = unix.Close(directoryFD)
		return nil, fmt.Errorf("inspect guest helper cache lock link: %w", err)
	}
	if linked.Dev != metadata.Dev || linked.Ino != metadata.Ino || linked.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
		_ = unix.Close(directoryFD)
		return nil, errors.New("guest helper cache lock changed during acquisition")
	}
	file := os.NewFile(uintptr(fd), filepath.Join(cacheRoot, ".lock"))
	if file == nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
		_ = unix.Close(directoryFD)
		return nil, errors.New("guest helper cache lock descriptor is invalid")
	}
	return &guestHelperCacheLock{directoryFD: directoryFD, file: file}, nil
}

func (lock *guestHelperCacheLock) release() {
	if lock == nil {
		return
	}
	if lock.file != nil {
		_ = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
		_ = lock.file.Close()
	}
	if lock.directoryFD >= 0 {
		_ = unix.Close(lock.directoryFD)
	}
}

func validateGuestHelperMountSource(source runtime.HostPath) error {
	if err := validateGuestHelperSource(source); err != nil {
		return err
	}
	directory := filepath.Dir(string(source))
	if err := verifyOwnedMode(directory, true, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != "dsx-guest" {
		return errors.New("guest helper mount directory must contain only dsx-guest")
	}
	return verifyOwnedMode(string(source), false, 0o700)
}

func stageGuestHelperDirectory(input *os.File, cacheRoot, destination, digest string) error {
	temporary, err := os.MkdirTemp(cacheRoot, ".staging-")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(filepath.Join(temporary, "dsx-guest"))
			_ = os.Remove(temporary)
		}
	}()
	if err := os.Chmod(temporary, 0o700); err != nil {
		return err
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	output, err := os.OpenFile(filepath.Join(temporary, "dsx-guest"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if err := verifyGuestHelperDirectory(temporary, digest); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		if _, statErr := os.Lstat(destination); statErr == nil {
			return nil
		}
		return err
	}
	keep = true
	return nil
}

func ensurePrivateHostDirectory(directory string) error {
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("private helper directory must be a clean absolute path")
	}
	parent := filepath.Dir(directory)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil || resolvedParent != parent {
		return errors.New("private helper parent must not contain symlink components")
	}
	if err := rejectSymlinkComponents(parent); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return verifyOwnedMode(directory, true, 0o700)
}

func verifyGuestHelperDirectory(directory, digest string) error {
	if err := verifyOwnedMode(directory, true, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != "dsx-guest" || entries[0].Type()&os.ModeSymlink != 0 {
		return errors.New("staged helper directory must contain only dsx-guest")
	}
	helper := filepath.Join(directory, "dsx-guest")
	if err := verifyOwnedMode(helper, false, 0o700); err != nil {
		return err
	}
	file, err := os.Open(helper)
	if err != nil {
		return err
	}
	defer file.Close()
	observed, err := digestOpenFile(file)
	if err != nil {
		return err
	}
	if observed != digest {
		return errors.New("staged helper digest does not match source")
	}
	return nil
}

func verifyOwnedMode(name string, directory bool, mode os.FileMode) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if directory && !info.IsDir() || !directory && !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		return errors.New("staged helper type or mode is unsafe")
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(metadata.Uid) != os.Getuid() || int(metadata.Gid) != os.Getgid() {
		return errors.New("staged helper owner does not match DSX")
	}
	return nil
}

func digestOpenFile(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, io.LimitReader(file, 128<<20)); err != nil {
		return "", err
	}
	var extra [1]byte
	if count, err := file.Read(extra[:]); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	} else if count != 0 {
		return "", errors.New("guest helper exceeds size limit")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func pruneGuestHelperCache(cacheRoot, keep string) error {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return err
	}
	type cached struct {
		name    string
		modTime time.Time
	}
	candidates := make([]cached, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == ".lock" {
			if err := verifyOwnedMode(filepath.Join(cacheRoot, entry.Name()), false, 0o600); err != nil {
				return err
			}
			continue
		}
		if entry.Name() == keep {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".staging-") {
			if err := removeStaleGuestHelperStaging(filepath.Join(cacheRoot, entry.Name())); err != nil {
				return err
			}
			continue
		}
		if !strings.HasPrefix(entry.Name(), "sha256-") || len(entry.Name()) != len("sha256-")+64 {
			return fmt.Errorf("unexpected entry %q in guest helper cache", entry.Name())
		}
		digest := strings.TrimPrefix(entry.Name(), "sha256-")
		directory := filepath.Join(cacheRoot, entry.Name())
		if err := verifyGuestHelperDirectory(directory, digest); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		candidates = append(candidates, cached{name: entry.Name(), modTime: info.ModTime()})
	}
	sort.Slice(candidates, func(left, right int) bool { return candidates[left].modTime.Before(candidates[right].modTime) })
	removeCount := len(candidates) + 1 - maxGuestHelperCacheEntries
	if removeCount < 0 {
		removeCount = 0
	}
	for index := range removeCount {
		directory := filepath.Join(cacheRoot, candidates[index].name)
		if err := os.Remove(filepath.Join(directory, "dsx-guest")); err != nil {
			return err
		}
		if err := os.Remove(directory); err != nil {
			return err
		}
	}
	return nil
}

func removeStaleGuestHelperStaging(directory string) error {
	if err := verifyOwnedMode(directory, true, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) > 1 || len(entries) == 1 && entries[0].Name() != "dsx-guest" {
		return errors.New("unsafe stale guest helper staging directory")
	}
	if len(entries) == 1 {
		helper := filepath.Join(directory, "dsx-guest")
		if err := verifyOwnedMode(helper, false, 0o700); err != nil {
			return err
		}
		if err := os.Remove(helper); err != nil {
			return err
		}
	}
	return os.Remove(directory)
}
