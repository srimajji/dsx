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
	LeappGuestDirectory       = "/run/dsx/aws"
	LeappCurrentGuestPath     = LeappGuestDirectory + "/current"
	LeappConfigGuestPath      = LeappCurrentGuestPath + "/config"
	LeappCredentialsGuestPath = LeappCurrentGuestPath + "/credentials"
	LeappAllProfilesWarning   = "AWS/Leapp access exposes every profile in the approved directory; AWS_PROFILE selects a default but is not credential isolation"
	MaxLeappFileBytes         = 4 << 20
	leappConfigFile           = "config"
	leappCredentialsFile      = "credentials"
)

// LeappAuthority binds an approved physical directory to its filesystem object.
// Identity contains device and inode only; credential contents never enter the
// executable plan or logs.
type LeappAuthority struct {
	DeclaredPath  string
	CanonicalPath string
	Identity      string
}

// LeappSourceIdentity is the exact approved filesystem object.
type LeappSourceIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	UID    uint32 `json:"uid"`
}

// LeappDirectorySnapshot contains one complete descriptor-bound read of the
// two standard AWS files. Its bytes must never be serialized into helper
// ledgers, status responses, or errors.
type LeappDirectorySnapshot struct {
	Config      []byte
	Credentials []byte
}

var (
	ErrLeappSourceIdentity  = errors.New("Leapp source directory identity changed")
	ErrLeappSourceUnsafe    = errors.New("Leapp source file is unsafe")
	ErrLeappSourceOversized = errors.New("Leapp source file exceeds size limit")
)

// LeappWorkspaceGrant is the complete, non-secret guest grant for Leapp.
type LeappWorkspaceGrant struct {
	Source      string
	Target      string
	ReadOnly    bool
	Environment []string
	Warning     string
}

// LeappDirectory holds an opened approved directory while a runtime consumes
// its path. Opening each component with O_NOFOLLOW makes validation independent
// of symlink resolution races.
type LeappDirectory struct {
	fd        int
	authority LeappAuthority
}

func (directory *LeappDirectory) Close() error {
	if directory == nil || directory.fd < 0 {
		return nil
	}
	fd := directory.fd
	directory.fd = -1
	return unix.Close(fd)
}

func (directory *LeappDirectory) Authority() LeappAuthority {
	if directory == nil {
		return LeappAuthority{}
	}
	return directory.authority
}

// SourceIdentity returns the opened directory identity used by the mirror
// helper. It does not reopen any pathname.
func (directory *LeappDirectory) SourceIdentity() LeappSourceIdentity {
	if directory == nil || directory.fd < 0 {
		return LeappSourceIdentity{}
	}
	var stat unix.Stat_t
	if unix.Fstat(directory.fd, &stat) != nil {
		return LeappSourceIdentity{}
	}
	return leappSourceIdentity(stat)
}

// Snapshot reads both bounded standard files through descriptors relative to
// the already-open approved directory. Atomic source renames therefore yield
// either the complete old inode or the complete new inode.
func (directory *LeappDirectory) Snapshot() (LeappDirectorySnapshot, error) {
	return directory.snapshotWithHook(nil)
}

func (directory *LeappDirectory) snapshotWithHook(afterConfig func()) (LeappDirectorySnapshot, error) {
	if directory == nil || directory.fd < 0 {
		return LeappDirectorySnapshot{}, ErrLeappSourceIdentity
	}
	var candidate LeappDirectorySnapshot
	haveCandidate := false
	for range 5 {
		var before unix.Stat_t
		if err := unix.Fstat(directory.fd, &before); err != nil {
			return LeappDirectorySnapshot{}, ErrLeappSourceIdentity
		}
		config, err := readLeappFileAt(directory.fd, leappConfigFile)
		if err != nil {
			return LeappDirectorySnapshot{}, err
		}
		if afterConfig != nil {
			afterConfig()
		}
		credentials, err := readLeappFileAt(directory.fd, leappCredentialsFile)
		if err != nil {
			return LeappDirectorySnapshot{}, err
		}
		var after unix.Stat_t
		if err := unix.Fstat(directory.fd, &after); err != nil {
			return LeappDirectorySnapshot{}, ErrLeappSourceIdentity
		}
		if !sameLeappDirectoryVersion(before, after) {
			haveCandidate = false
			continue
		}
		if haveCandidate && bytes.Equal(candidate.Config, config) && bytes.Equal(candidate.Credentials, credentials) {
			return LeappDirectorySnapshot{Config: config, Credentials: credentials}, nil
		}
		candidate = LeappDirectorySnapshot{Config: config, Credentials: credentials}
		haveCandidate = true
	}
	return LeappDirectorySnapshot{}, fmt.Errorf("%w: paired files did not stabilize during bounded snapshot", ErrLeappSourceUnsafe)
}

// ResolveLeappDirectory resolves and validates one physical directory. Both
// standard AWS files must exist as regular, non-symlink entries.
func ResolveLeappDirectory(source string) (LeappAuthority, error) {
	canonical, err := canonicalPhysicalPath(source)
	if err != nil {
		return LeappAuthority{}, err
	}
	directory, err := openLeappDirectory(canonical)
	if err != nil {
		return LeappAuthority{}, err
	}
	defer directory.Close()
	directory.authority.DeclaredPath = source
	return directory.authority, nil
}

// OpenApprovedLeappDirectory reopens an approved directory and refuses a path
// whose device/inode identity or standard file safety has changed.
func OpenApprovedLeappDirectory(approved LeappAuthority) (*LeappDirectory, error) {
	if approved.DeclaredPath == "" || approved.CanonicalPath == "" || approved.Identity == "" {
		return nil, errors.New("Leapp directory authority is incomplete")
	}
	canonical, err := canonicalPhysicalPath(approved.CanonicalPath)
	if err != nil {
		return nil, err
	}
	if canonical != approved.CanonicalPath {
		return nil, fmt.Errorf("%w: canonical path changed after approval", ErrLeappSourceIdentity)
	}
	directory, err := openLeappDirectory(canonical)
	if err != nil {
		return nil, err
	}
	if directory.authority.Identity != approved.Identity {
		_ = directory.Close()
		return nil, fmt.Errorf("%w: directory identity changed after approval", ErrLeappSourceIdentity)
	}
	directory.authority = approved
	return directory, nil
}

// LeappGrant returns the exact directory mount and standard AWS environment.
func LeappGrant(approved LeappAuthority, profile string) (LeappWorkspaceGrant, error) {
	if approved.CanonicalPath == "" || approved.Identity == "" {
		return LeappWorkspaceGrant{}, errors.New("Leapp directory authority is incomplete")
	}
	if strings.IndexByte(profile, 0) >= 0 {
		return LeappWorkspaceGrant{}, errors.New("AWS profile contains NUL")
	}
	environment := []string{
		"AWS_CONFIG_FILE=" + LeappConfigGuestPath,
		"AWS_SHARED_CREDENTIALS_FILE=" + LeappCredentialsGuestPath,
	}
	if profile != "" {
		environment = append(environment, "AWS_PROFILE="+profile)
	}
	return LeappWorkspaceGrant{
		Source:      approved.CanonicalPath,
		Target:      LeappGuestDirectory,
		ReadOnly:    true,
		Environment: environment,
		Warning:     LeappAllProfilesWarning,
	}, nil
}

func canonicalPhysicalPath(source string) (string, error) {
	if source == "" {
		return "", errors.New("Leapp directory is missing")
	}
	if !filepath.IsAbs(source) {
		return "", errors.New("Leapp directory must be an absolute path")
	}
	if filepath.Clean(source) != source {
		return "", errors.New("Leapp directory must be a clean canonical path")
	}
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("resolve Leapp directory: %w", err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("absolutize Leapp directory: %w", err)
	}
	canonical = filepath.Clean(canonical)
	if canonical != source {
		return "", errors.New("Leapp directory must not contain symlink components")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve current user home for Leapp policy: %w", err)
	}
	physicalHome, err := filepath.EvalSymlinks(filepath.Clean(home))
	if err != nil {
		return "", fmt.Errorf("canonicalize current user home for Leapp policy: %w", err)
	}
	physicalHome, err = filepath.Abs(physicalHome)
	if err != nil {
		return "", fmt.Errorf("absolutize current user home for Leapp policy: %w", err)
	}
	physicalHome = filepath.Clean(physicalHome)
	if pathContains(canonical, physicalHome) {
		return "", errors.New("Leapp directory must not mount the current user home or one of its ancestors")
	}
	return canonical, nil
}

func openLeappDirectory(canonical string) (*LeappDirectory, error) {
	fd, stat, err := openDirectoryNoSymlinks(canonical)
	if err != nil {
		return nil, err
	}
	directory := &LeappDirectory{
		fd: fd,
		authority: LeappAuthority{
			CanonicalPath: canonical,
			Identity:      filesystemIdentity(stat),
		},
	}
	if err := validateLeappFile(fd, leappConfigFile); err != nil {
		_ = directory.Close()
		return nil, err
	}
	if err := validateLeappFile(fd, leappCredentialsFile); err != nil {
		_ = directory.Close()
		return nil, err
	}
	return directory, nil
}

func openDirectoryNoSymlinks(absolute string) (int, unix.Stat_t, error) {
	root := string(filepath.Separator)
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, unix.Stat_t{}, fmt.Errorf("open filesystem root for Leapp directory: %w", err)
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
			return -1, unix.Stat_t{}, fmt.Errorf("open Leapp directory component %q: %w", component, openErr)
		}
		current = next
	}
	var stat unix.Stat_t
	if err := unix.Fstat(current, &stat); err != nil {
		_ = unix.Close(current)
		return -1, unix.Stat_t{}, fmt.Errorf("inspect opened Leapp directory: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(current)
		return -1, unix.Stat_t{}, errors.New("Leapp path must be a directory")
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

func validateLeappFile(directoryFD int, name string) error {
	fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: open Leapp %s", ErrLeappSourceUnsafe, name)
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("%w: inspect Leapp %s", ErrLeappSourceUnsafe, name)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%w: Leapp %s must be an owner regular file", ErrLeappSourceUnsafe, name)
	}
	if stat.Size < 0 || stat.Size > MaxLeappFileBytes {
		return fmt.Errorf("%w: Leapp %s", ErrLeappSourceOversized, name)
	}
	return nil
}

func filesystemIdentity(stat unix.Stat_t) string {
	identity := leappSourceIdentity(stat)
	return fmt.Sprintf("dev=%d;ino=%d;uid=%d", identity.Device, identity.Inode, identity.UID)
}

func leappSourceIdentity(stat unix.Stat_t) LeappSourceIdentity {
	return LeappSourceIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: stat.Uid}
}

func readLeappFileAt(directoryFD int, name string) ([]byte, error) {
	fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open Leapp %s", ErrLeappSourceUnsafe, name)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w: open Leapp %s descriptor", ErrLeappSourceUnsafe, name)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 {
		return nil, fmt.Errorf("%w: inspect Leapp %s", ErrLeappSourceUnsafe, name)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("%w: Leapp %s owner", ErrLeappSourceUnsafe, name)
	}
	if before.Size() > MaxLeappFileBytes {
		return nil, fmt.Errorf("%w: Leapp %s", ErrLeappSourceOversized, name)
	}
	contents, err := io.ReadAll(io.LimitReader(file, MaxLeappFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read Leapp %s", ErrLeappSourceUnsafe, name)
	}
	if int64(len(contents)) > MaxLeappFileBytes {
		return nil, fmt.Errorf("%w: Leapp %s", ErrLeappSourceOversized, name)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(contents)) != after.Size() {
		return nil, fmt.Errorf("%w: Leapp %s changed while reading", ErrLeappSourceUnsafe, name)
	}
	return contents, nil
}
