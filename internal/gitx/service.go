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
	"slices"
	"strconv"
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
	maxGitMetadataFileBytes   = 64 << 10
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
	return service.runGitWithEnvironment(ctx, directory, nil, stdout, arguments...)
}

func (service *Service) runGitWithEnvironment(ctx context.Context, directory string, environment []string, stdout io.Writer, arguments ...string) error {
	var stderr cappedCapture
	stderr.limit = maxGitErrorOutput
	commandEnvironment := make([]string, 0, len(service.environment)+len(environment))
	commandEnvironment = append(commandEnvironment, service.environment...)
	commandEnvironment = append(commandEnvironment, environment...)
	exit, err := service.runner.Run(ctx, Command{
		Argv:   service.gitArgv(arguments...),
		Dir:    directory,
		Env:    commandEnvironment,
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
	return service.gitOutputWithEnvironment(ctx, directory, nil, arguments...)
}

func (service *Service) gitOutputWithEnvironment(ctx context.Context, directory string, environment []string, arguments ...string) ([]byte, error) {
	capture := cappedCapture{limit: maxGitOutput}
	if err := service.runGitWithEnvironment(ctx, directory, environment, &capture, arguments...); err != nil {
		return nil, err
	}
	if capture.truncated {
		return nil, fmt.Errorf("git %s output exceeds %d bytes", commandLabel(arguments), maxGitOutput)
	}
	return capture.Bytes(), nil
}

// repositoryConfigPreflight inspects assignments written directly in the
// repository's resolved common configuration without following include paths
// or the per-worktree configuration that extensions.worktreeConfig can enable.
type repositoryConfigPreflight struct {
	includeKeys           []string
	worktreeConfigEnabled bool
}

func inspectRepositoryConfigPreflight(repositoryPath string) (repositoryConfigPreflight, error) {
	config, found, err := readRepositoryCommonConfig(repositoryPath)
	if err != nil || !found {
		return repositoryConfigPreflight{}, err
	}
	data := bytes.TrimPrefix(config.data, []byte{0xef, 0xbb, 0xbf})
	lines := strings.Split(string(data), "\n")
	if len(data) > 0 && data[len(data)-1] == '\n' {
		lines = lines[:len(lines)-1]
	}
	var result repositoryConfigPreflight
	var section, subsection string
	var hasQuotedSubsection bool
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSuffix(lines[index], "\r")
		if gitConfigLineContinues(line) {
			var continued strings.Builder
			continued.WriteString(line[:len(line)-1])
			for index++; index < len(lines); index++ {
				line = strings.TrimSuffix(lines[index], "\r")
				if gitConfigLineContinues(line) {
					continued.WriteString(line[:len(line)-1])
					continue
				}
				continued.WriteString(line)
				break
			}
			line = continued.String()
		}

		remainder := strings.TrimLeft(line, " \t\r")
		for strings.HasPrefix(remainder, "[") {
			nextSection, nextSubsection, nextHasQuotedSubsection, rest, ok := parseGitSectionHeader(remainder[1:])
			if !ok {
				section, subsection, hasQuotedSubsection, remainder = "", "", false, ""
				break
			}
			section, subsection, hasQuotedSubsection = nextSection, nextSubsection, nextHasQuotedSubsection
			remainder = strings.TrimLeft(rest, " \t\r")
		}
		assignment, ok := parseGitConfigAssignment(remainder)
		if !ok {
			continue
		}
		if assignment.name == "path" &&
			(section == "include" && !hasQuotedSubsection ||
				section == "includeif" && hasQuotedSubsection && subsection != "") {
			key := section + ".path"
			if subsection != "" {
				key = section + "." + subsection + ".path"
			}
			result.includeKeys = append(result.includeKeys, key)
		}
		if section == "extensions" && !hasQuotedSubsection &&
			assignment.name == "worktreeconfig" && gitConfigBooleanEnabled(assignment) {
			result.worktreeConfigEnabled = true
		}
	}
	if err := revalidateStableRegularFile(config.path, "repository common Git configuration", config.info); err != nil {
		return repositoryConfigPreflight{}, err
	}
	return result, nil
}

type gitConfigAssignment struct {
	name     string
	value    string
	implicit bool
}

func parseGitConfigAssignment(line string) (gitConfigAssignment, bool) {
	line = strings.TrimLeft(line, " \t\r")
	if line == "" || line[0] == '#' || line[0] == ';' || !isGitConfigNameStart(line[0]) {
		return gitConfigAssignment{}, false
	}
	end := 1
	for end < len(line) && isGitConfigNameByte(line[end]) {
		end++
	}
	assignment := gitConfigAssignment{name: strings.ToLower(line[:end])}
	remainder := strings.TrimLeft(line[end:], " \t\r")
	if remainder == "" || remainder[0] == '#' || remainder[0] == ';' {
		assignment.implicit = true
		return assignment, true
	}
	if remainder[0] != '=' {
		return gitConfigAssignment{}, false
	}
	value, ok := parseGitConfigValue(remainder[1:])
	if !ok {
		return gitConfigAssignment{}, false
	}
	assignment.value = value
	return assignment, true
}

func parseGitConfigValue(value string) (string, bool) {
	value = strings.TrimLeft(value, " \t\r")
	var parsed strings.Builder
	quoted := false
	pendingWhitespace := -1
	for index := 0; index < len(value); index++ {
		character := value[index]
		if !quoted && (character == '#' || character == ';') {
			break
		}
		if !quoted && (character == ' ' || character == '\t' || character == '\r') {
			if pendingWhitespace < 0 {
				pendingWhitespace = index
			}
			continue
		}
		if pendingWhitespace >= 0 {
			parsed.WriteString(value[pendingWhitespace:index])
			pendingWhitespace = -1
		}
		switch character {
		case '"':
			quoted = !quoted
		case '\\':
			index++
			if index >= len(value) {
				return "", false
			}
			switch value[index] {
			case 'n':
				parsed.WriteByte('\n')
			case 't':
				parsed.WriteByte('\t')
			case 'b':
				parsed.WriteByte('\b')
			case '\\', '"':
				parsed.WriteByte(value[index])
			default:
				return "", false
			}
		default:
			parsed.WriteByte(character)
		}
	}
	if quoted {
		return "", false
	}
	return parsed.String(), true
}

func gitConfigLineContinues(line string) bool {
	quoted := false
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case '#', ';':
			if !quoted {
				return false
			}
		case '"':
			quoted = !quoted
		case '\\':
			if index+1 == len(line) {
				return true
			}
			index++
		}
	}
	return false
}

func gitConfigBooleanEnabled(assignment gitConfigAssignment) bool {
	if assignment.implicit {
		return true
	}
	switch strings.ToLower(assignment.value) {
	case "true", "yes", "on":
		return true
	case "", "false", "no", "off":
		return false
	}
	return gitConfigIntegerIsNonzero(assignment.value)
}

func gitConfigIntegerIsNonzero(value string) bool {
	number := value
	multiplier := int64(1)
	if len(number) > 1 {
		switch number[len(number)-1] {
		case 'k', 'K':
			number, multiplier = number[:len(number)-1], 1024
		case 'm', 'M':
			number, multiplier = number[:len(number)-1], 1024*1024
		case 'g', 'G':
			number, multiplier = number[:len(number)-1], 1024*1024*1024
		}
	}
	lowerNumber := strings.ToLower(strings.TrimLeft(number, "+-"))
	if strings.Contains(number, "_") || strings.HasPrefix(lowerNumber, "0b") {
		return false
	}
	parsed, err := strconv.ParseInt(number, 0, 64)
	if err != nil ||
		parsed > 0 && parsed > (1<<63-1)/multiplier ||
		parsed < 0 && parsed < (-1<<63)/multiplier {
		return false
	}
	return parsed != 0
}

func isGitConfigNameStart(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func isGitConfigNameByte(character byte) bool {
	return isGitConfigNameStart(character) ||
		character >= '0' && character <= '9' ||
		character == '-'
}

type stableRegularFile struct {
	path string
	data []byte
	info os.FileInfo
}

func readRepositoryCommonConfig(repositoryPath string) (stableRegularFile, bool, error) {
	if !cleanAbsolutePath(repositoryPath) {
		return stableRegularFile{}, false, fmt.Errorf("repository path must be clean and absolute, got %q", repositoryPath)
	}
	gitEntryPath := filepath.Join(repositoryPath, ".git")
	entry, err := os.Lstat(gitEntryPath)
	if err != nil {
		return stableRegularFile{}, false, fmt.Errorf("inspect repository Git metadata entry: %w", err)
	}

	var gitDirPath string
	var gitDirIdentity PhysicalPathIdentity
	var gitFile stableRegularFile
	switch {
	case entry.IsDir() && entry.Mode()&os.ModeSymlink == 0:
		gitDirPath = gitEntryPath
		gitDirIdentity, err = capturePhysicalPath(gitDirPath)
		if err != nil {
			return stableRegularFile{}, false, fmt.Errorf("resolve repository Git directory as a canonical physical directory: %w", err)
		}
		current, inspectErr := os.Lstat(gitEntryPath)
		if inspectErr != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(entry, current) {
			return stableRegularFile{}, false, errors.Join(errors.New("repository Git directory changed while resolving"), inspectErr)
		}
	case entry.Mode().IsRegular() && entry.Mode()&os.ModeSymlink == 0:
		gitFile, _, err = readStableRegularFile(gitEntryPath, "repository .git gitfile", maxGitMetadataFileBytes, false)
		if err != nil {
			return stableRegularFile{}, false, err
		}
		gitDirDeclaration, parseErr := parseGitDirDeclaration(gitFile.data)
		if parseErr != nil {
			return stableRegularFile{}, false, parseErr
		}
		gitDirPath, gitDirIdentity, err = resolvePhysicalGitDirectory(repositoryPath, gitDirDeclaration, "Git directory target")
		if err != nil {
			return stableRegularFile{}, false, err
		}
	default:
		return stableRegularFile{}, false, errors.New("repository .git entry must be a physical directory or bounded regular non-symlink gitfile")
	}

	commonDirPath := gitDirPath
	commonDirIdentity := gitDirIdentity
	commonPointerPath := filepath.Join(gitDirPath, "commondir")
	commonPointer, hasCommonPointer, err := readStableRegularFile(
		commonPointerPath,
		"repository Git common-directory pointer",
		maxGitMetadataFileBytes,
		true,
	)
	if err != nil {
		return stableRegularFile{}, false, err
	}
	if hasCommonPointer {
		commonDirDeclaration, parseErr := parseCommonDirDeclaration(commonPointer.data)
		if parseErr != nil {
			return stableRegularFile{}, false, parseErr
		}
		commonDirPath, commonDirIdentity, err = resolvePhysicalGitDirectory(gitDirPath, commonDirDeclaration, "common Git directory")
		if err != nil {
			return stableRegularFile{}, false, err
		}
	}

	configPath := filepath.Join(commonDirPath, "config")
	config, found, err := readStableRegularFile(
		configPath,
		"repository common Git configuration",
		maxGitOutput,
		true,
	)
	if err != nil {
		return stableRegularFile{}, false, err
	}
	if err := revalidatePhysicalPath(gitDirIdentity); err != nil {
		return stableRegularFile{}, false, fmt.Errorf("Git directory target changed while resolving configuration: %w", err)
	}
	if commonDirPath != gitDirPath {
		if err := revalidatePhysicalPath(commonDirIdentity); err != nil {
			return stableRegularFile{}, false, fmt.Errorf("common Git directory changed while resolving configuration: %w", err)
		}
	}
	if gitFile.info != nil {
		if err := revalidateStableRegularFile(gitFile.path, "repository .git gitfile", gitFile.info); err != nil {
			return stableRegularFile{}, false, err
		}
	}
	if hasCommonPointer {
		if err := revalidateStableRegularFile(commonPointer.path, "repository Git common-directory pointer", commonPointer.info); err != nil {
			return stableRegularFile{}, false, err
		}
	} else if err := revalidateMissingPath(commonPointerPath, "repository Git common-directory pointer"); err != nil {
		return stableRegularFile{}, false, err
	}
	if !found {
		if err := revalidateMissingPath(configPath, "repository common Git configuration"); err != nil {
			return stableRegularFile{}, false, err
		}
		return stableRegularFile{}, false, nil
	}
	return config, true, nil
}

func resolvePhysicalGitDirectory(base, declaration, label string) (string, PhysicalPathIdentity, error) {
	if declaration == "" || strings.IndexByte(declaration, 0) >= 0 {
		return "", PhysicalPathIdentity{}, fmt.Errorf("%s path is empty or contains NUL", label)
	}
	candidate := declaration
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(base, candidate)
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", PhysicalPathIdentity{}, fmt.Errorf("canonicalize %s: %w", label, err)
	}
	candidate = filepath.Clean(absolute)
	identity, err := capturePhysicalPath(candidate)
	if err != nil {
		return "", PhysicalPathIdentity{}, fmt.Errorf("resolve %s as a canonical physical directory: %w", label, err)
	}
	return candidate, identity, nil
}

func parseGitDirDeclaration(data []byte) (string, error) {
	line, err := parseSingleGitMetadataLine(data, "repository .git gitfile")
	if err != nil {
		return "", err
	}
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) || len(line) == len(prefix) {
		return "", errors.New("repository .git gitfile must contain exactly one nonempty gitdir: declaration")
	}
	return line[len(prefix):], nil
}

func parseCommonDirDeclaration(data []byte) (string, error) {
	line, err := parseSingleGitMetadataLine(data, "repository Git common-directory pointer")
	if err != nil {
		return "", err
	}
	if line == "" {
		return "", errors.New("repository Git common-directory pointer must contain exactly one nonempty path")
	}
	return line, nil
}

func parseSingleGitMetadataLine(data []byte, label string) (string, error) {
	switch {
	case bytes.HasSuffix(data, []byte("\r\n")):
		data = data[:len(data)-2]
	case bytes.HasSuffix(data, []byte("\n")):
		data = data[:len(data)-1]
	}
	if bytes.ContainsAny(data, "\x00\r\n") {
		return "", fmt.Errorf("%s must contain exactly one declaration line", label)
	}
	return string(data), nil
}

func readStableRegularFile(filePath, label string, maximum int64, allowMissing bool) (stableRegularFile, bool, error) {
	before, err := os.Lstat(filePath)
	if err != nil {
		if allowMissing && errors.Is(err, os.ErrNotExist) {
			if _, verifyErr := os.Lstat(filePath); errors.Is(verifyErr, os.ErrNotExist) {
				return stableRegularFile{}, false, nil
			} else if verifyErr != nil {
				return stableRegularFile{}, false, fmt.Errorf("reinspect %s: %w", label, verifyErr)
			}
			return stableRegularFile{}, false, fmt.Errorf("%s changed while inspecting", label)
		}
		return stableRegularFile{}, false, fmt.Errorf("inspect %s: %w", label, err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > maximum {
		return stableRegularFile{}, false, fmt.Errorf("%s must be a regular non-symlink file no larger than %d bytes", label, maximum)
	}
	file, err := os.Open(filePath)
	if err != nil {
		return stableRegularFile{}, false, fmt.Errorf("open %s: %w", label, err)
	}
	opened, statErr := file.Stat()
	if statErr != nil || !sameStableFileInfo(before, opened) {
		closeErr := file.Close()
		return stableRegularFile{}, false, errors.Join(fmt.Errorf("%s changed while opening", label), statErr, closeErr)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	afterOpened, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil {
		return stableRegularFile{}, false, errors.Join(fmt.Errorf("read %s", label), readErr, statErr, closeErr)
	}
	if int64(len(data)) > maximum {
		return stableRegularFile{}, false, fmt.Errorf("%s exceeds the supported %d-byte bound", label, maximum)
	}
	if !sameStableFileInfo(before, afterOpened) || int64(len(data)) != afterOpened.Size() {
		return stableRegularFile{}, false, fmt.Errorf("%s changed while reading", label)
	}
	afterPath, err := os.Lstat(filePath)
	if err != nil || !sameStableFileInfo(before, afterPath) {
		return stableRegularFile{}, false, errors.Join(fmt.Errorf("%s changed while reading", label), err)
	}
	return stableRegularFile{path: filePath, data: data, info: afterPath}, true, nil
}

func revalidateStableRegularFile(filePath, label string, expected os.FileInfo) error {
	observed, err := os.Lstat(filePath)
	if err != nil || !sameStableFileInfo(expected, observed) {
		return errors.Join(fmt.Errorf("%s changed after reading", label), err)
	}
	return nil
}

func revalidateMissingPath(filePath, label string) error {
	_, err := os.Lstat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reinspect absent %s: %w", label, err)
	}
	return fmt.Errorf("%s appeared while resolving repository configuration", label)
}

func sameStableFileInfo(left, right os.FileInfo) bool {
	return left != nil && right != nil &&
		left.Mode().IsRegular() && right.Mode().IsRegular() &&
		left.Mode()&os.ModeSymlink == 0 && right.Mode()&os.ModeSymlink == 0 &&
		os.SameFile(left, right) &&
		left.Size() == right.Size() &&
		left.Mode() == right.Mode() &&
		left.ModTime().Equal(right.ModTime())
}

// parseGitSectionHeader parses a Git configuration section header body, the
// text after the opening bracket, into its lowercased section name, its
// case-sensitive subsection, whether quoted subsection syntax was present,
// and the text after the closing bracket. Matching Git's own listing form
// keeps a reported key usable as written.
func parseGitSectionHeader(header string) (section, subsection string, hasQuotedSubsection bool, rest string, ok bool) {
	end := strings.IndexAny(header, " \t\"]")
	if end <= 0 {
		return "", "", false, "", false
	}
	section = strings.ToLower(header[:end])
	if header[end] == ']' {
		return section, "", false, header[end+1:], true
	}
	rest = header[end:]
	if header[end] == ' ' || header[end] == '\t' {
		rest = strings.TrimLeft(rest, " \t")
		if strings.HasPrefix(rest, "]") {
			return "", "", false, "", false
		}
	}
	if !strings.HasPrefix(rest, `"`) {
		return "", "", false, "", false
	}
	var name strings.Builder
	for index := 1; index < len(rest); index++ {
		switch rest[index] {
		case '\\':
			index++
			if index >= len(rest) || rest[index] != '\\' && rest[index] != '"' {
				return "", "", false, "", false
			}
			name.WriteByte(rest[index])
		case '"':
			if index+1 >= len(rest) || rest[index+1] != ']' {
				return "", "", false, "", false
			}
			return section, name.String(), true, rest[index+2:], true
		default:
			name.WriteByte(rest[index])
		}
	}
	return "", "", false, "", false
}

func (service *Service) validateLocalConfiguration(ctx context.Context, repositoryPath string) error {
	preflight, err := inspectRepositoryConfigPreflight(repositoryPath)
	if err != nil {
		return err
	}
	if len(preflight.includeKeys) > 0 {
		// Git resolves include directives while setting up the repository for
		// any command, including a --no-includes listing, so detected includes
		// are reported without invoking Git and without reading the target.
		return unallowlistedLocalGitConfigError(preflight.includeKeys)
	}
	if preflight.worktreeConfigEnabled {
		// Enabling this extension makes Git load config.worktree during
		// repository setup, before --local --no-includes can constrain listing.
		return unallowlistedLocalGitConfigError([]string{"extensions.worktreeconfig"})
	}
	var rejected []string
	output, err := service.gitOutput(ctx, repositoryPath, "config", "--local", "--no-includes", "--null", "--list")
	if err != nil {
		return fmt.Errorf("inspect repository-local Git configuration: %w", err)
	}
	for _, raw := range cleanNULTerminated(output) {
		// A record without a separator is a valueless implicit Git boolean;
		// validate it as the empty value Git itself resolves it to.
		key, value, _ := strings.Cut(string(raw), "\n")
		key = normalizeGitConfigKey(key)
		if !allowlistedLocalGitConfig(key, value) {
			rejected = append(rejected, key)
		}
	}
	return unallowlistedLocalGitConfigError(rejected)
}

// normalizeGitConfigKey lowercases the section and leaf of a Git configuration
// key and preserves subsection casing, which Git treats as case-sensitive, so
// a reported key remains usable verbatim with git config --local --unset-all.
func normalizeGitConfigKey(key string) string {
	first := strings.IndexByte(key, '.')
	last := strings.LastIndexByte(key, '.')
	if first < 0 {
		return strings.ToLower(key)
	}
	return strings.ToLower(key[:first]) + key[first:last+1] + strings.ToLower(key[last+1:])
}

// unallowlistedLocalGitConfigError reports every rejected key once, sorted, and
// without any configured value. The message stays on one line because host
// command rendering escapes newlines inside a reported error.
func unallowlistedLocalGitConfigError(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	slices.Sort(keys)
	quoted := make([]string, 0, len(keys))
	for _, key := range slices.Compact(keys) {
		quoted = append(quoted, strconv.Quote(key))
	}
	return fmt.Errorf("repository-local Git configuration keys are not allowlisted: %s; remove a key with: git config --local --unset-all <key>", strings.Join(quoted, ", "))
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
		// Inert branch annotations written by VS Code and the GitHub CLI. They
		// record a merge base or pull-request number and never alter Git
		// behavior, so only these exact leaves are reviewed as accepted.
		case "vscode-merge-base", "github-pr-owner-number", "gh-merge-base":
			return safeGitConfigScalar(value)
		}
	case "remote":
		switch leaf {
		case "url", "pushurl":
			return safeRemoteURL(value)
		case "fetch", "push":
			return safeGitConfigScalar(value)
		// Inert default-repository annotation written by gh repo set-default.
		case "gh-resolved":
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

func validateWorkspace(value string) error {
	_, err := model.ParseWorkspaceName(value)
	return err
}

func validateSourceBranch(value string) error {
	if value == "" || len(value) > 1024 || strings.HasPrefix(value, "refs/heads/") ||
		strings.IndexByte(value, 0) >= 0 || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("invalid source branch %q", value)
	}
	return nil
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
