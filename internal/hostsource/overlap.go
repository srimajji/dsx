package hostsource

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrOverlap    = errors.New("sensitive read-only host source overlaps a writable host source")
	ErrUnprovable = errors.New("sensitive read-only host source isolation cannot be established")
)

// ValidateReadOnlyIsolation proves that no filesystem object reachable through
// sensitive is also reachable through a writable bind source. It follows
// symlinks and compares physical file identities so path aliases and hardlinks
// cannot turn a read-only grant into writable authority.
func ValidateReadOnlyIsolation(sensitive string, writable []string) error {
	sensitiveRoot, err := canonicalExisting(sensitive)
	if err != nil {
		return ErrUnprovable
	}
	sensitiveObjects := make([]fs.FileInfo, 0, 8)
	if err := walkPhysicalRange(sensitiveRoot, func(_ string, info fs.FileInfo) error {
		sensitiveObjects = append(sensitiveObjects, info)
		return nil
	}); err != nil {
		return ErrUnprovable
	}

	for _, source := range writable {
		writableRoot, err := canonicalExisting(source)
		if err != nil {
			return ErrUnprovable
		}
		if pathsOverlap(sensitiveRoot, writableRoot) {
			return ErrOverlap
		}
		err = walkPhysicalRange(writableRoot, func(_ string, candidate fs.FileInfo) error {
			for _, protected := range sensitiveObjects {
				if os.SameFile(protected, candidate) {
					return ErrOverlap
				}
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, ErrOverlap) {
				return ErrOverlap
			}
			return ErrUnprovable
		}
	}
	return nil
}

func canonicalExisting(source string) (string, error) {
	if source == "" || !filepath.IsAbs(source) || filepath.Clean(source) != source {
		return "", ErrUnprovable
	}
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	canonical = filepath.Clean(canonical)
	if _, err := os.Stat(canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

func pathsOverlap(left, right string) bool {
	return pathContains(left, right) || pathContains(right, left)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func walkPhysicalRange(root string, visit func(string, fs.FileInfo) error) error {
	visitedDirectories := make([]fs.FileInfo, 0, 16)
	var walk func(string) error
	walk = func(current string) error {
		info, err := os.Stat(current)
		if err != nil {
			return err
		}
		if err := visit(current, info); err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		for _, visited := range visitedDirectories {
			if os.SameFile(visited, info) {
				return nil
			}
		}
		visitedDirectories = append(visitedDirectories, info)
		entries, err := os.ReadDir(current)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			child := filepath.Join(current, entry.Name())
			if entry.Type()&os.ModeSymlink != 0 {
				child, err = filepath.EvalSymlinks(child)
				if err != nil {
					return err
				}
			}
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root)
}
