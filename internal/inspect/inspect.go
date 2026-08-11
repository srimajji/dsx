// Package inspect discovers project declarations without executing or modifying them.
package inspect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// MaxEntries bounds directory traversal. Excluded dependency and build directories
	// do not contribute entries below their root.
	MaxEntries = 100_000
	// MaxDepth bounds traversal below the canonical workspace root.
	MaxDepth = 32
	// MaxFileBytes bounds every declaration that inspection considers.
	MaxFileBytes int64 = 8 << 20
)

type Severity string

const (
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

type Diagnostic struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Path     string   `json:"path"`
	Field    string   `json:"field,omitempty"`
	Message  string   `json:"message"`
}

type Lockfile struct {
	Path      string `json:"path"`
	Ecosystem string `json:"ecosystem"`
}

type Devenv struct {
	Path      string   `json:"path"`
	Processes []string `json:"processes,omitempty"`
	Services  []string `json:"services,omitempty"`
}

type Facts struct {
	WorkspaceRoot  string       `json:"workspaceRoot"`
	GitRoots       []string     `json:"gitRoots,omitempty"`
	Lockfiles      []Lockfile   `json:"lockfiles,omitempty"`
	Containerfiles []string     `json:"containerfiles,omitempty"`
	Devenv         []Devenv     `json:"devenv,omitempty"`
	Diagnostics    []Diagnostic `json:"diagnostics,omitempty"`
}

func (f Facts) HasErrors() bool {
	for _, diagnostic := range f.Diagnostics {
		if diagnostic.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Inspect returns deterministic, workspace-relative project facts. It follows the
// root argument once to establish its canonical identity, but never follows a
// symlink found within the workspace.
func Inspect(root string) (Facts, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return Facts{}, err
	}
	facts := Facts{WorkspaceRoot: canonical}
	gitRoots := make(map[string]struct{})
	entries := 0

	err = filepath.WalkDir(canonical, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("inspect %q: %w", relative(canonical, path), walkErr)
		}
		rel := relative(canonical, path)
		if rel == "." {
			return nil
		}
		entries++
		if entries > MaxEntries {
			return fmt.Errorf("inspect workspace: entry limit %d exceeded", MaxEntries)
		}
		if depth(rel) > MaxDepth {
			return fmt.Errorf("inspect %q: depth limit %d exceeded", rel, MaxDepth)
		}

		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 {
			if excludedName(name) {
				return nil
			}
			target, evalErr := filepath.EvalSymlinks(path)
			if evalErr != nil {
				return fmt.Errorf("inspect symlink %q: %w", rel, evalErr)
			}
			target, evalErr = filepath.Abs(target)
			if evalErr != nil {
				return fmt.Errorf("inspect symlink %q: %w", rel, evalErr)
			}
			if !within(canonical, target) {
				return fmt.Errorf("inspect symlink %q: target escapes workspace root", rel)
			}
			facts.Diagnostics = append(facts.Diagnostics, Diagnostic{
				Severity: SeverityWarning,
				Code:     "symlink_skipped",
				Path:     rel,
				Message:  "symlink is not followed during project inspection",
			})
			return nil
		}
		if entry.IsDir() {
			if name == ".git" {
				gitRoots[relative(canonical, filepath.Dir(path))] = struct{}{}
				return filepath.SkipDir
			}
			if excludedName(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}

		if name == ".git" {
			if err := checkBoundedFile(path, rel); err != nil {
				return err
			}
			gitRoots[relative(canonical, filepath.Dir(path))] = struct{}{}
			return nil
		}
		if ecosystem, ok := lockfileEcosystem(rel, name); ok {
			if err := checkBoundedFile(path, rel); err != nil {
				return err
			}
			facts.Lockfiles = append(facts.Lockfiles, Lockfile{Path: rel, Ecosystem: ecosystem})
		}
		if name == "Dockerfile" || name == "Containerfile" {
			if err := checkBoundedFile(path, rel); err != nil {
				return err
			}
			facts.Containerfiles = append(facts.Containerfiles, rel)
		}
		if name == "devenv.nix" {
			content, readErr := readBoundedFile(canonical, path, rel)
			if readErr != nil {
				return readErr
			}
			processes, services := declarations(content)
			facts.Devenv = append(facts.Devenv, Devenv{Path: rel, Processes: processes, Services: services})
		}
		return nil
	})
	if err != nil {
		return facts, err
	}
	for gitRoot := range gitRoots {
		facts.GitRoots = append(facts.GitRoots, gitRoot)
	}
	sortFacts(&facts)
	return facts, nil
}

func contentDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func canonicalRoot(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("inspect workspace: root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("inspect workspace root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect workspace root: %w", err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("inspect workspace root: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("inspect workspace root: not a directory")
	}
	return filepath.Clean(canonical), nil
}

func readBoundedFile(root, path, rel string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %q: %w", rel, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("inspect %q: declaration is not a regular file", rel)
	}
	if before.Size() > MaxFileBytes {
		return nil, fmt.Errorf("inspect %q: file exceeds %d-byte limit", rel, MaxFileBytes)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %q: %w", rel, err)
	}
	if !within(root, resolved) {
		return nil, fmt.Errorf("inspect %q: resolved path escapes workspace root", rel)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %q: %w", rel, err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect %q: %w", rel, err)
	}
	if !os.SameFile(before, after) {
		return nil, fmt.Errorf("inspect %q: file changed while opening", rel)
	}
	content, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("inspect %q: %w", rel, err)
	}
	if int64(len(content)) > MaxFileBytes {
		return nil, fmt.Errorf("inspect %q: file exceeds %d-byte limit", rel, MaxFileBytes)
	}
	return content, nil
}

func checkBoundedFile(path, rel string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %q: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("inspect %q: input is not a regular file", rel)
	}
	if info.Size() > MaxFileBytes {
		return fmt.Errorf("inspect %q: file exceeds %d-byte limit", rel, MaxFileBytes)
	}
	return nil
}

func relative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func depth(rel string) int {
	if rel == "." {
		return 0
	}
	return strings.Count(filepath.FromSlash(rel), string(filepath.Separator)) + 1
}

func excludedName(name string) bool {
	switch name {
	case "node_modules", ".devenv", ".direnv", ".venv", "venv", ".tox", ".mypy_cache", ".pytest_cache", "__pycache__", "vendor", "target", "dist", ".next", ".cache":
		return true
	default:
		return false
	}
}

func lockfileEcosystem(rel, name string) (string, bool) {
	switch name {
	case "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml", "bun.lock", "bun.lockb":
		return "javascript", true
	case "uv.lock", "poetry.lock", "Pipfile.lock", "pdm.lock":
		return "python", true
	case "gradle.lockfile", "dependencies.lock":
		return "java", true
	case "composer.lock":
		return "php", true
	}
	if strings.Contains(rel, "/gradle/dependency-locks/") && strings.HasSuffix(name, ".lockfile") {
		return "java", true
	}
	return "", false
}

func sortFacts(facts *Facts) {
	sort.Strings(facts.GitRoots)
	sort.Slice(facts.Lockfiles, func(i, j int) bool { return facts.Lockfiles[i].Path < facts.Lockfiles[j].Path })
	sort.Strings(facts.Containerfiles)
	sort.Slice(facts.Devenv, func(i, j int) bool { return facts.Devenv[i].Path < facts.Devenv[j].Path })
	sort.SliceStable(facts.Diagnostics, func(i, j int) bool {
		left, right := facts.Diagnostics[i], facts.Diagnostics[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		if left.Severity != right.Severity {
			return left.Severity < right.Severity
		}
		return left.Code < right.Code
	})
}
