package apple

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var containerExecutableCandidates = [...]string{
	"/opt/homebrew/bin/container",
	"/usr/local/bin/container",
}

var trustedContainerCellars = [...]string{
	"/opt/homebrew/Cellar/container",
	"/usr/local/Cellar/container",
}

// DiscoverContainerExecutable finds a container CLI only at fixed installation
// entry points and returns its verified, fully resolved executable path. Ambient
// PATH and environment variables are deliberately not authority.
func DiscoverContainerExecutable() (string, error) {
	return discoverContainerExecutable(containerExecutableCandidates[:], trustedContainerCellars[:])
}

func discoverContainerExecutable(candidates, trustedCellars []string) (string, error) {
	var rejected []string
	for _, candidate := range candidates {
		resolved, err := resolveTrustedContainerExecutable(candidate, trustedCellars)
		if err == nil {
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			rejected = append(rejected, fmt.Sprintf("%s: %v", candidate, err))
		}
	}
	if len(rejected) != 0 {
		return "", fmt.Errorf("no trusted Apple container executable found (%s)", strings.Join(rejected, "; "))
	}
	return "", errors.New("no trusted Apple container executable found at a supported installation path")
}

func resolveTrustedContainerExecutable(candidate string, trustedCellars []string) (string, error) {
	if candidate == "" || !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
		return "", fmt.Errorf("container executable candidate %q is not a clean absolute path", candidate)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve container executable %q: %w", candidate, err)
	}
	if !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", fmt.Errorf("resolved container executable %q is not a clean absolute path", resolved)
	}
	if !isTrustedCellarExecutable(resolved, trustedCellars) {
		return "", fmt.Errorf("resolved container executable %q is outside a trusted container Cellar", resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect container executable %q: %w", resolved, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("container executable %q must be an executable regular file", resolved)
	}
	return resolved, nil
}

func isTrustedCellarExecutable(resolved string, trustedCellars []string) bool {
	for _, cellar := range trustedCellars {
		if cellar == "" || !filepath.IsAbs(cellar) || filepath.Clean(cellar) != cellar {
			continue
		}
		resolvedCellar, err := filepath.EvalSymlinks(cellar)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(resolvedCellar, resolved)
		if err != nil {
			continue
		}
		parts := strings.Split(relative, string(filepath.Separator))
		if len(parts) == 3 && parts[0] != "" && parts[0] != "." && parts[0] != ".." && parts[1] == "bin" && parts[2] == "container" {
			return true
		}
	}
	return false
}
