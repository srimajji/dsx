package guest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"

	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/model"
	"golang.org/x/sys/unix"
)

const maxSecretEnvironmentBytes = 1 << 20

func ReplaceProcessWithoutPrivilegeGains(argv []string, environmentFile string) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("exec argv is required")
	}
	environment := os.Environ()
	if environmentFile != "" {
		overlay, loadErr := loadSecretEnvironment(environmentFile)
		if loadErr != nil {
			return loadErr
		}
		environment = overlayEnvironment(environment, overlay)
	}
	if err := VerifyInstalledExecutable(); err != nil {
		return err
	}
	executable, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	if err := lockProcessPrivileges(); err != nil {
		return err
	}
	unix.Umask(0o077)
	return syscall.Exec(executable, argv, environment)
}

func StageSecretEnvironment(name string, input io.Reader) error {
	if err := validateSecretEnvironmentPath(name); err != nil {
		return err
	}
	if input == nil {
		return errors.New("secret environment input is required")
	}
	contents, err := io.ReadAll(io.LimitReader(input, maxSecretEnvironmentBytes+1))
	if err != nil {
		return fmt.Errorf("read secret environment input: %w", err)
	}
	if len(contents) > maxSecretEnvironmentBytes {
		return errors.New("secret environment exceeds size limit")
	}
	if _, err := parseSecretEnvironment(contents); err != nil {
		return err
	}

	components := strings.Split(name, "/")[2:]
	return stagePrivateContents(components, name, contents, maxSecretEnvironmentBytes)
}

func loadSecretEnvironment(name string) ([]string, error) {
	if err := validateSecretEnvironmentPath(name); err != nil {
		return nil, err
	}
	components := strings.Split(name, "/")[2:]
	tmpFD, err := openTemporaryRoot()
	if err != nil {
		return nil, err
	}
	defer unix.Close(tmpFD)
	chain, err := openDirectoryChain(tmpFD, components[:len(components)-1], false, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		return nil, err
	}
	defer chain.close()
	if err := chain.revalidate(tmpFD); err != nil {
		return nil, err
	}
	parentFD := chain[len(chain)-1].fd
	leaf := components[len(components)-1]
	fd, err := unix.Openat(parentFD, leaf, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		_ = unix.Unlinkat(parentFD, leaf, 0)
		return nil, fmt.Errorf("open secret environment: %w", err)
	}
	if err := unix.Unlinkat(parentFD, leaf, 0); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("unlink secret environment: %w", err)
	}
	if err := chain.revalidate(tmpFD); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open secret environment: invalid descriptor")
	}
	defer file.Close()

	var metadata unix.Stat_t
	if err := unix.Fstat(fd, &metadata); err != nil {
		return nil, fmt.Errorf("inspect secret environment: %w", err)
	}
	if err := validateSecretEnvironmentMetadata(metadata, uint32(os.Geteuid()), uint32(os.Getegid())); err != nil {
		return nil, err
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxSecretEnvironmentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read secret environment: %w", err)
	}
	if len(contents) > maxSecretEnvironmentBytes {
		return nil, errors.New("secret environment exceeds size limit")
	}
	return parseSecretEnvironment(contents)
}

func validateSecretEnvironmentPath(name string) error {
	if name == "" || len(name) > 256 || path.Clean(name) != name {
		return errors.New("secret environment path is not authorized")
	}
	parts := strings.Split(name, "/")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "tmp" || parts[2] != "dsx-run" {
		return errors.New("secret environment path is not authorized")
	}
	runID, err := model.ParseRunID(parts[3])
	if err != nil || string(runID) != parts[3] {
		return errors.New("secret environment path is not authorized")
	}
	leaf := parts[4]
	if len(leaf) != len("env-")+32 || !strings.HasPrefix(leaf, "env-") || !lowerHex(leaf[len("env-"):]) {
		return errors.New("secret environment path is not authorized")
	}
	return nil
}

func validateSecretEnvironmentMetadata(metadata unix.Stat_t, effectiveUID, effectiveGID uint32) error {
	if metadata.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("secret environment is not a regular file")
	}
	if metadata.Uid != effectiveUID || metadata.Gid != effectiveGID {
		return errors.New("secret environment has the wrong owner")
	}
	if metadata.Mode&0o7777 != 0o600 {
		return errors.New("secret environment mode must be 0600")
	}
	if metadata.Size < 0 || metadata.Size > maxSecretEnvironmentBytes {
		return errors.New("secret environment exceeds size limit")
	}
	return nil
}

func parseSecretEnvironment(contents []byte) ([]string, error) {
	if len(contents) == 0 || contents[len(contents)-1] != 0 {
		return nil, errors.New("secret environment must be non-empty and NUL-terminated")
	}
	entries := make([]string, 0, 8)
	seen := make(map[string]struct{})
	for len(contents) > 0 {
		separator := 0
		for separator < len(contents) && contents[separator] != 0 {
			separator++
		}
		entry := string(contents[:separator])
		contents = contents[separator+1:]
		if entry == "" || len(entry) > guestproto.MaxStringBytes {
			return nil, errors.New("secret environment contains an empty or oversized entry")
		}
		name, _, found := strings.Cut(entry, "=")
		if !found || !validEnvironmentName(name) {
			return nil, errors.New("secret environment contains an invalid entry")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("secret environment contains duplicate key %q", name)
		}
		seen[name] = struct{}{}
		entries = append(entries, entry)
		if len(entries) > guestproto.MaxEnvironment {
			return nil, errors.New("secret environment contains too many entries")
		}
	}
	return entries, nil
}

func overlayEnvironment(base, overlay []string) []string {
	names := make(map[string]struct{}, len(overlay))
	for _, entry := range overlay {
		name, _, _ := strings.Cut(entry, "=")
		names[name] = struct{}{}
	}
	result := make([]string, 0, len(base)+len(overlay))
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if _, replaced := names[name]; found && replaced {
			continue
		}
		result = append(result, entry)
	}
	return append(result, overlay...)
}

func validEnvironmentName(name string) bool {
	if name == "" || (name[0] != '_' && (name[0] < 'A' || name[0] > 'Z') && (name[0] < 'a' || name[0] > 'z')) {
		return false
	}
	for index := 1; index < len(name); index++ {
		character := name[index]
		if character != '_' && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func lowerHex(value string) bool {
	for index := range value {
		if (value[index] < '0' || value[index] > '9') && (value[index] < 'a' || value[index] > 'f') {
			return false
		}
	}
	return true
}
