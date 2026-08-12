package bridge

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	HostAWSGuestDirectory       = "/run/dsx/aws"
	HostAWSCurrentGuestPath     = HostAWSGuestDirectory + "/current"
	HostAWSConfigGuestPath      = HostAWSCurrentGuestPath + "/config"
	HostAWSCredentialsGuestPath = HostAWSCurrentGuestPath + "/credentials"
	HostAWSDefaultOnlyWarning   = "Host AWS access publishes the temporary host default only; named profiles are unavailable"
	MaxHostAWSFileBytes         = 4 << 20
	hostAWSConfigFile           = "config"
	hostAWSCredentialsFile      = "credentials"
)

// HostAWSAuthority binds an approved physical directory to its filesystem
// object. Identity contains device and inode only; credential contents never
// enter the executable plan or logs.
type HostAWSAuthority struct {
	DeclaredPath  string
	CanonicalPath string
	Identity      string
}

// HostAWSSourceIdentity is the exact approved filesystem object.
type HostAWSSourceIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	UID    uint32 `json:"uid"`
}

// HostAWSDirectorySnapshot contains one complete descriptor-bound read of the
// two standard AWS files. Its bytes must never be serialized into helper
// ledgers, status responses, or errors.
type HostAWSDirectorySnapshot struct {
	Config      []byte
	Credentials []byte
}

var (
	ErrHostAWSSourceIdentity  = errors.New("host AWS source directory identity changed")
	ErrHostAWSSourceUnsafe    = errors.New("host AWS source file is unsafe")
	ErrHostAWSSourceOversized = errors.New("host AWS source file exceeds size limit")
)

// HostAWSWorkspaceGrant is the complete, non-secret guest grant.
type HostAWSWorkspaceGrant struct {
	Source      string
	Target      string
	ReadOnly    bool
	Environment []string
	Warning     string
}

// HostAWSDirectory holds an opened approved directory while a runtime consumes
// its path. Opening each component with O_NOFOLLOW makes validation independent
// of symlink resolution races.
type HostAWSDirectory struct {
	fd        int
	authority HostAWSAuthority
}

func (directory *HostAWSDirectory) Close() error {
	if directory == nil || directory.fd < 0 {
		return nil
	}
	fd := directory.fd
	directory.fd = -1
	return unix.Close(fd)
}

func (directory *HostAWSDirectory) Authority() HostAWSAuthority {
	if directory == nil {
		return HostAWSAuthority{}
	}
	return directory.authority
}

// SourceIdentity returns the opened directory identity used by the mirror
// helper. It does not reopen any pathname.
func (directory *HostAWSDirectory) SourceIdentity() HostAWSSourceIdentity {
	if directory == nil || directory.fd < 0 {
		return HostAWSSourceIdentity{}
	}
	var stat unix.Stat_t
	if unix.Fstat(directory.fd, &stat) != nil {
		return HostAWSSourceIdentity{}
	}
	return hostAWSSourceIdentity(stat)
}

// Snapshot reads both bounded standard files through descriptors relative to
// the already-open approved directory. Atomic source renames therefore yield
// either the complete old inode or the complete new inode.
func (directory *HostAWSDirectory) Snapshot() (HostAWSDirectorySnapshot, error) {
	return directory.snapshotWithHook(nil)
}

func (directory *HostAWSDirectory) snapshotWithHook(afterConfig func()) (HostAWSDirectorySnapshot, error) {
	if directory == nil || directory.fd < 0 {
		return HostAWSDirectorySnapshot{}, ErrHostAWSSourceIdentity
	}
	var candidate HostAWSDirectorySnapshot
	haveCandidate := false
	for range 5 {
		var before unix.Stat_t
		if err := unix.Fstat(directory.fd, &before); err != nil {
			return HostAWSDirectorySnapshot{}, ErrHostAWSSourceIdentity
		}
		config, err := readHostAWSFileAt(directory.fd, hostAWSConfigFile)
		if err != nil {
			return HostAWSDirectorySnapshot{}, err
		}
		if afterConfig != nil {
			afterConfig()
		}
		credentials, err := readHostAWSFileAt(directory.fd, hostAWSCredentialsFile)
		if err != nil {
			return HostAWSDirectorySnapshot{}, err
		}
		var after unix.Stat_t
		if err := unix.Fstat(directory.fd, &after); err != nil {
			return HostAWSDirectorySnapshot{}, ErrHostAWSSourceIdentity
		}
		if !sameLeappDirectoryVersion(before, after) {
			haveCandidate = false
			continue
		}
		if haveCandidate && bytes.Equal(candidate.Config, config) && bytes.Equal(candidate.Credentials, credentials) {
			return HostAWSDirectorySnapshot{Config: config, Credentials: credentials}, nil
		}
		candidate = HostAWSDirectorySnapshot{Config: config, Credentials: credentials}
		haveCandidate = true
	}
	return HostAWSDirectorySnapshot{}, fmt.Errorf("%w: paired files did not stabilize during bounded snapshot", ErrHostAWSSourceUnsafe)
}

// ResolveHostAWSDirectory binds one canonical physical directory to its
// filesystem identity. Credential availability and contents are deliberately
// deferred to OpenApprovedHostAWSDirectory at runtime.
func ResolveHostAWSDirectory(source string) (HostAWSAuthority, error) {
	canonical, err := canonicalPhysicalPath(source)
	if err != nil {
		return HostAWSAuthority{}, err
	}
	fd, stat, err := openDirectoryNoSymlinks(canonical)
	if err != nil {
		return HostAWSAuthority{}, err
	}
	defer unix.Close(fd)
	return HostAWSAuthority{
		DeclaredPath:  source,
		CanonicalPath: canonical,
		Identity:      filesystemIdentity(stat),
	}, nil
}

// OpenApprovedHostAWSDirectory reopens an approved directory and refuses a path
// whose device/inode identity or runtime standard-file safety has changed.
func OpenApprovedHostAWSDirectory(approved HostAWSAuthority) (*HostAWSDirectory, error) {
	if approved.DeclaredPath == "" || approved.CanonicalPath == "" || approved.Identity == "" {
		return nil, errors.New("host AWS directory authority is incomplete")
	}
	canonical, err := canonicalPhysicalPath(approved.CanonicalPath)
	if err != nil {
		return nil, err
	}
	if canonical != approved.CanonicalPath {
		return nil, fmt.Errorf("%w: canonical path changed after approval", ErrHostAWSSourceIdentity)
	}
	directory, err := openHostAWSDirectory(canonical)
	if err != nil {
		return nil, err
	}
	if directory.authority.Identity != approved.Identity {
		_ = directory.Close()
		return nil, fmt.Errorf("%w: directory identity changed after approval", ErrHostAWSSourceIdentity)
	}
	directory.authority = approved
	return directory, nil
}

// HostDefaultGrant returns the exact directory mount and default-only standard
// AWS file environment.
func HostDefaultGrant(approved HostAWSAuthority) (HostAWSWorkspaceGrant, error) {
	if approved.CanonicalPath == "" || approved.Identity == "" {
		return HostAWSWorkspaceGrant{}, errors.New("host AWS directory authority is incomplete")
	}
	return HostAWSWorkspaceGrant{
		Source:   approved.CanonicalPath,
		Target:   HostAWSGuestDirectory,
		ReadOnly: true,
		Environment: []string{
			"AWS_CONFIG_FILE=" + HostAWSConfigGuestPath,
			"AWS_SHARED_CREDENTIALS_FILE=" + HostAWSCredentialsGuestPath,
		},
		Warning: HostAWSDefaultOnlyWarning,
	}, nil
}

func canonicalPhysicalPath(source string) (string, error) {
	if source == "" {
		return "", errors.New("host AWS directory is missing")
	}
	if !filepath.IsAbs(source) {
		return "", errors.New("host AWS directory must be an absolute path")
	}
	if filepath.Clean(source) != source {
		return "", errors.New("host AWS directory must be a clean canonical path")
	}
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("resolve host AWS directory: %w", err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("absolutize host AWS directory: %w", err)
	}
	canonical = filepath.Clean(canonical)
	if canonical != source {
		return "", errors.New("host AWS directory must not contain symlink components")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve current user home for host AWS policy: %w", err)
	}
	physicalHome, err := filepath.EvalSymlinks(filepath.Clean(home))
	if err != nil {
		return "", fmt.Errorf("canonicalize current user home for host AWS policy: %w", err)
	}
	physicalHome, err = filepath.Abs(physicalHome)
	if err != nil {
		return "", fmt.Errorf("absolutize current user home for host AWS policy: %w", err)
	}
	physicalHome = filepath.Clean(physicalHome)
	if pathContains(canonical, physicalHome) {
		return "", errors.New("host AWS directory must not mount the current user home or one of its ancestors")
	}
	return canonical, nil
}

func openHostAWSDirectory(canonical string) (*HostAWSDirectory, error) {
	fd, stat, err := openDirectoryNoSymlinks(canonical)
	if err != nil {
		return nil, err
	}
	directory := &HostAWSDirectory{
		fd: fd,
		authority: HostAWSAuthority{
			CanonicalPath: canonical,
			Identity:      filesystemIdentity(stat),
		},
	}
	if err := validateHostAWSFile(fd, hostAWSConfigFile); err != nil {
		_ = directory.Close()
		return nil, err
	}
	if err := validateHostAWSFile(fd, hostAWSCredentialsFile); err != nil {
		_ = directory.Close()
		return nil, err
	}
	return directory, nil
}

func openDirectoryNoSymlinks(absolute string) (int, unix.Stat_t, error) {
	root := string(filepath.Separator)
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, unix.Stat_t{}, fmt.Errorf("open filesystem root for host AWS directory: %w", err)
	}
	current := fd
	components := strings.Split(strings.TrimPrefix(absolute, root), string(filepath.Separator))
	for _, component := range components {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(current)
		if openErr != nil {
			return -1, unix.Stat_t{}, fmt.Errorf("open host AWS directory component %q: %w", component, openErr)
		}
		current = next
	}
	var stat unix.Stat_t
	if err := unix.Fstat(current, &stat); err != nil {
		_ = unix.Close(current)
		return -1, unix.Stat_t{}, fmt.Errorf("inspect opened host AWS directory: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(current)
		return -1, unix.Stat_t{}, errors.New("host AWS path must be a directory")
	}
	return current, stat, nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validateHostAWSFile(directoryFD int, name string) error {
	fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: open host AWS %s", ErrHostAWSSourceUnsafe, name)
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("%w: inspect host AWS %s", ErrHostAWSSourceUnsafe, name)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%w: host AWS %s must be an owner regular file", ErrHostAWSSourceUnsafe, name)
	}
	if stat.Size < 0 || stat.Size > MaxHostAWSFileBytes {
		return fmt.Errorf("%w: host AWS %s", ErrHostAWSSourceOversized, name)
	}
	return nil
}

func filesystemIdentity(stat unix.Stat_t) string {
	identity := hostAWSSourceIdentity(stat)
	return fmt.Sprintf("dev=%d;ino=%d;uid=%d", identity.Device, identity.Inode, identity.UID)
}

func hostAWSSourceIdentity(stat unix.Stat_t) HostAWSSourceIdentity {
	return HostAWSSourceIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: stat.Uid}
}

func readHostAWSFileAt(directoryFD int, name string) ([]byte, error) {
	fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open host AWS %s", ErrHostAWSSourceUnsafe, name)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w: open host AWS %s descriptor", ErrHostAWSSourceUnsafe, name)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 {
		return nil, fmt.Errorf("%w: inspect host AWS %s", ErrHostAWSSourceUnsafe, name)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("%w: host AWS %s owner", ErrHostAWSSourceUnsafe, name)
	}
	if before.Size() > MaxHostAWSFileBytes {
		return nil, fmt.Errorf("%w: host AWS %s", ErrHostAWSSourceOversized, name)
	}
	contents, err := io.ReadAll(io.LimitReader(file, MaxHostAWSFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read host AWS %s", ErrHostAWSSourceUnsafe, name)
	}
	if int64(len(contents)) > MaxHostAWSFileBytes {
		return nil, fmt.Errorf("%w: host AWS %s", ErrHostAWSSourceOversized, name)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(contents)) != after.Size() {
		return nil, fmt.Errorf("%w: host AWS %s changed while reading", ErrHostAWSSourceUnsafe, name)
	}
	return contents, nil
}
