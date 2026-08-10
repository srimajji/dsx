package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/srimajji/dsx/internal/model"
)

const (
	maxGitOutput              = 8 << 20
	maxGitErrorOutput         = 64 << 10
	defaultDiffMaxBytes       = 1 << 20
	maxApplyRollbackBytes     = 32 << 20
	maxApplyRollbackPaths     = 8192
	maxApplyRollbackPathBytes = 1 << 20
)

type Service struct {
	runner        Runner
	gitExecutable string
	environment   []string

	artifactMu sync.Mutex
	artifacts  map[string]artifactIdentity
}

type artifactIdentity struct {
	root string
	info os.FileInfo
}

func NewService(runner Runner, gitExecutable string) (*Service, error) {
	if runner == nil {
		return nil, errors.New("git runner is nil")
	}
	if gitExecutable == "" || strings.IndexByte(gitExecutable, 0) >= 0 ||
		!filepath.IsAbs(gitExecutable) || filepath.Clean(gitExecutable) != gitExecutable {
		return nil, fmt.Errorf("git executable must be a clean absolute path, got %q", gitExecutable)
	}
	info, err := os.Stat(gitExecutable)
	if err != nil {
		return nil, fmt.Errorf("inspect git executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("git executable %q is not an executable regular file", gitExecutable)
	}
	return &Service{
		runner:        runner,
		gitExecutable: gitExecutable,
		environment:   controlledGitEnvironment(),
		artifacts:     make(map[string]artifactIdentity),
	}, nil
}

func controlledGitEnvironment() []string {
	return []string{
		"GIT_ALLOW_PROTOCOL=file",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PROTOCOL_FROM_USER=0",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
}

func (service *Service) gitArgv(arguments ...string) []string {
	argv := []string{
		service.gitExecutable,
		"--no-pager",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.protectHFS=true",
		"-c", "core.protectNTFS=true",
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "diff.external=",
		"-c", "interactive.diffFilter=",
	}
	return append(argv, arguments...)
}

func (service *Service) runGit(ctx context.Context, directory string, stdout io.Writer, arguments ...string) error {
	var stderr cappedCapture
	stderr.limit = maxGitErrorOutput
	exit, err := service.runner.Run(ctx, Command{
		Argv:   service.gitArgv(arguments...),
		Dir:    directory,
		Env:    append([]string(nil), service.environment...),
		Stdout: stdout,
		Stderr: &stderr,
	})
	if err == nil && exit.Code == 0 && exit.Signal == "" {
		return nil
	}
	detail := strings.TrimSpace(string(stderr.Bytes()))
	if stderr.truncated {
		detail += " [diagnostic truncated]"
	}
	if detail == "" {
		detail = "no diagnostic"
	}
	if err != nil {
		return fmt.Errorf("git %s failed (exit=%d signal=%q): %s: %w", commandLabel(arguments), exit.Code, exit.Signal, detail, err)
	}
	return fmt.Errorf("git %s failed (exit=%d signal=%q): %s", commandLabel(arguments), exit.Code, exit.Signal, detail)
}

func (service *Service) gitOutput(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	capture := cappedCapture{limit: maxGitOutput}
	if err := service.runGit(ctx, directory, &capture, arguments...); err != nil {
		return nil, err
	}
	if capture.truncated {
		return nil, fmt.Errorf("git %s output exceeds %d bytes", commandLabel(arguments), maxGitOutput)
	}
	return capture.Bytes(), nil
}

func rejectRepositoryIncludeConfig(repositoryPath string) error {
	configPath := filepath.Join(repositoryPath, ".git", "config")
	before, err := os.Lstat(configPath)
	if err != nil {
		return nil
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maxGitOutput {
		return errors.New("repository-local Git configuration must be a bounded regular file")
	}
	file, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("open repository-local Git configuration: %w", err)
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("inspect opened repository-local Git configuration: %w", err)
	}
	if !os.SameFile(before, opened) {
		_ = file.Close()
		return errors.New("repository-local Git configuration changed while opening")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxGitOutput+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if len(data) > maxGitOutput {
		return errors.New("repository-local Git configuration exceeds the supported bound")
	}
	after, err := os.Lstat(configPath)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return errors.New("repository-local Git configuration changed while reading")
	}
	for _, line := range strings.Split(string(data), "\n") {
		normalized := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(normalized, "[include") {
			return errors.New("repository-local Git configuration \"include.path\" is not allowlisted")
		}
	}
	return nil
}

func (service *Service) validateLocalConfiguration(ctx context.Context, repositoryPath string) error {
	if err := rejectRepositoryIncludeConfig(repositoryPath); err != nil {
		return err
	}
	output, err := service.gitOutput(ctx, repositoryPath, "config", "--local", "--no-includes", "--null", "--list")
	if err != nil {
		return fmt.Errorf("inspect repository-local Git configuration: %w", err)
	}
	for _, raw := range cleanNULTerminated(output) {
		record := string(raw)
		key, value, found := strings.Cut(record, "\n")
		key = strings.ToLower(key)
		if !found || !allowlistedLocalGitConfig(key, value) {
			return fmt.Errorf("repository-local Git configuration %q is not allowlisted", key)
		}
	}
	return nil
}

// This list is intentionally limited to repository format facts, inert
// identity/preferences, and common branch/remote metadata. Command-bearing
// keys and config that can cause automatic object acquisition are omitted.
func allowlistedLocalGitConfig(key, value string) bool {
	switch key {
	case "core.bare", "core.filemode", "core.ignorecase", "core.logallrefupdates",
		"core.precomposeunicode", "core.symlinks", "commit.gpgsign", "tag.gpgsign",
		"rerere.enabled", "rerere.autoupdate":
		return validGitBoolean(value)
	case "core.repositoryformatversion":
		return value == "0" || value == "1"
	case "extensions.objectformat":
		return value == "sha1" || value == "sha256"
	case "extensions.refstorage":
		return value == "files" || value == "reftable"
	case "user.name", "user.email":
		return safeGitConfigScalar(value)
	}

	parts := strings.Split(key, ".")
	if len(parts) < 3 || strings.Join(parts[1:len(parts)-1], ".") == "" {
		return false
	}
	leaf := parts[len(parts)-1]
	switch parts[0] {
	case "branch":
		switch leaf {
		case "description", "merge", "pushremote", "remote":
			return safeGitConfigScalar(value)
		case "rebase":
			return validGitBoolean(value) || value == "merges"
		}
	case "remote":
		switch leaf {
		case "url", "pushurl":
			return safeRemoteURL(value)
		case "fetch", "push":
			return safeGitConfigScalar(value)
		case "mirror", "prune", "prunetags", "skipdefaultupdate", "skipfetchall":
			return validGitBoolean(value)
		case "tagopt":
			return value == "--tags" || value == "--no-tags"
		}
	}
	return false
}

func validGitBoolean(value string) bool {
	switch strings.ToLower(value) {
	case "", "true", "false", "yes", "no", "on", "off", "1", "0":
		return true
	default:
		return false
	}
}

func safeGitConfigScalar(value string) bool {
	return value != "" && !strings.ContainsAny(value, "\x00\r\n")
}

func safeRemoteURL(value string) bool {
	if !safeGitConfigScalar(value) || strings.TrimSpace(value) != value || strings.Contains(value, "::") {
		return false
	}
	scheme, _, hasScheme := strings.Cut(value, "://")
	if hasScheme {
		switch strings.ToLower(scheme) {
		case "file", "git", "http", "https":
			return true
		default:
			return false
		}
	}
	colon := strings.IndexByte(value, ':')
	slash := strings.IndexByte(value, '/')
	return colon < 0 || (slash >= 0 && slash < colon)
}

func commandLabel(arguments []string) string {
	if len(arguments) == 0 {
		return "command"
	}
	for _, argument := range arguments {
		if argument != "--" && !strings.HasPrefix(argument, "-") {
			return argument
		}
	}
	return "command"
}

type cappedCapture struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (capture *cappedCapture) Write(value []byte) (int, error) {
	original := len(value)
	remaining := capture.limit - capture.buffer.Len()
	if remaining <= 0 {
		capture.truncated = capture.truncated || original != 0
		return original, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		capture.truncated = true
	}
	_, err := capture.buffer.Write(value)
	return original, err
}

func (capture *cappedCapture) Bytes() []byte { return capture.buffer.Bytes() }

func validateRepositoryDescriptor(repository Repository) error {
	if !validRepositoryName(repository.Name) {
		return fmt.Errorf("invalid repository name %q", repository.Name)
	}
	if repository.HostPath == "" || !filepath.IsAbs(repository.HostPath) || filepath.Clean(repository.HostPath) != repository.HostPath || strings.IndexByte(repository.HostPath, 0) >= 0 {
		return fmt.Errorf("repository host path must be a clean absolute path, got %q", repository.HostPath)
	}
	info, err := os.Lstat(repository.HostPath)
	if err != nil {
		return fmt.Errorf("inspect repository host path: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("repository host path %q is not a directory", repository.HostPath)
	}
	if !path.IsAbs(repository.GuestPath) || path.Clean(repository.GuestPath) != repository.GuestPath || repository.GuestPath == "/" || strings.IndexByte(repository.GuestPath, 0) >= 0 {
		return fmt.Errorf("repository guest path must be canonical and absolute below /, got %q", repository.GuestPath)
	}
	return nil
}

// ValidateRepository revalidates the approved physical worktree and Git
// metadata identity without mutating the repository.
func (service *Service) ValidateRepository(ctx context.Context, repository Repository) error {
	return service.validateRepositoryIdentity(ctx, repository)
}

func validRepositoryName(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func validateSandbox(value string) error {
	_, err := model.ParseSandboxName(value)
	return err
}

func validateTempRoot(root string) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.IndexByte(root, 0) >= 0 {
		return fmt.Errorf("temporary root must be a clean absolute path, got %q", root)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect temporary root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("temporary root must be a non-symlink directory")
	}
	return nil
}

func validateFullOID(value, label string) error {
	if len(value) != 40 && len(value) != 64 {
		return fmt.Errorf("%s must be a full Git object ID", label)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return fmt.Errorf("%s must be a lowercase hexadecimal Git object ID", label)
	}
	return nil
}

func validateDigest(value string) error {
	if len(value) != sha256.Size*2 {
		return errors.New("bundle digest must be a full sha256 digest")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return errors.New("bundle digest must be lowercase hexadecimal sha256")
	}
	return nil
}

func digestMatches(expected string, actual [sha256.Size]byte) bool {
	decoded, _ := hex.DecodeString(expected)
	return len(decoded) == sha256.Size && subtle.ConstantTimeCompare(decoded, actual[:]) == 1
}

func cleanNULTerminated(value []byte) [][]byte {
	if len(value) == 0 {
		return nil
	}
	parts := bytes.Split(value, []byte{0})
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func (service *Service) registerArtifact(bundlePath, root string) error {
	info, err := os.Lstat(bundlePath)
	if err != nil {
		return err
	}
	service.artifactMu.Lock()
	service.artifacts[bundlePath] = artifactIdentity{root: root, info: info}
	service.artifactMu.Unlock()
	return nil
}

func (service *Service) RemoveArtifact(bundlePath string) error {
	if bundlePath == "" || !filepath.IsAbs(bundlePath) || filepath.Clean(bundlePath) != bundlePath {
		return errors.New("artifact path must be a clean absolute path")
	}
	service.artifactMu.Lock()
	identity, found := service.artifacts[bundlePath]
	if found {
		delete(service.artifacts, bundlePath)
	}
	service.artifactMu.Unlock()
	if !found {
		return errors.New("artifact is not owned by this Git service")
	}
	relative, err := filepath.Rel(identity.root, bundlePath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("artifact is outside its owned temporary root")
	}
	info, err := os.Lstat(bundlePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect artifact: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != SourceBundleMode || !os.SameFile(identity.info, info) {
		return errors.New("artifact identity or permissions changed")
	}
	if err := os.Remove(bundlePath); err != nil {
		return fmt.Errorf("remove artifact: %w", err)
	}
	return nil
}
