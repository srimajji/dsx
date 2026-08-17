package inspect

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestInspectCompositeWorkspaceStableRelativeFacts(t *testing.T) {
	root := copyFixture(t, "composite")
	writeTestFile(t, root, ".git", "gitdir: /nonexistent/project-root\n")
	writeTestFile(t, root, "apps/api/.git", "gitdir: /nonexistent/api-root\n")
	before := snapshotTree(t, root)

	first, err := Inspect(root)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	second, err := Inspect(root)
	if err != nil {
		t.Fatalf("second Inspect() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Inspect() is not deterministic:\nfirst:  %#v\nsecond: %#v", first, second)
	}
	if got, want := first.GitRoots, []string{".", "apps/api"}; !reflect.DeepEqual(got, want) {
		t.Errorf("GitRoots = %v, want %v", got, want)
	}
	wantLocks := []Lockfile{
		{Path: "apps/api/uv.lock", Ecosystem: "python"},
		{Path: "apps/java/gradle.lockfile", Ecosystem: "java"},
		{Path: "apps/php/composer.lock", Ecosystem: "php"},
		{Path: "apps/web/pnpm-lock.yaml", Ecosystem: "javascript"},
	}
	if !reflect.DeepEqual(first.Lockfiles, wantLocks) {
		t.Errorf("Lockfiles = %#v, want %#v", first.Lockfiles, wantLocks)
	}
	if got, want := first.Containerfiles, []string{"Dockerfile"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Containerfiles = %v, want %v", got, want)
	}
	if len(first.Devenv) != 1 {
		t.Fatalf("Devenv = %#v, want one", first.Devenv)
	}
	if got, want := first.Devenv[0].Processes, []string{"web", "worker"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Processes = %v, want %v", got, want)
	}
	if got, want := first.Devenv[0].Services, []string{"caddy", "mysql", "redis"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Services = %v, want %v", got, want)
	}
	if len(first.Diagnostics) != 0 {
		t.Errorf("Diagnostics = %#v, want none", first.Diagnostics)
	}
	if after := snapshotTree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("inspection changed filesystem state:\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestInspectIgnoresDevContainerDeclarationsCompletely(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "executed")
	writeTestFile(t, root, ".devcontainer/devcontainer.json", `{
		"initializeCommand": "touch `+sentinel+`",
		"mounts": ["source=/,target=/host,type=bind"],
		"forwardPorts": [3000]
	}`)
	facts, err := Inspect(root)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if len(facts.Diagnostics) != 0 {
		t.Fatalf("Diagnostics = %#v, want Dev Container ignored", facts.Diagnostics)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("ignored Dev Container command executed or stat failed: %v", err)
	}
}

func TestInspectRejectsSymlinkEscapeAndOversizedInput(t *testing.T) {
	t.Run("symlink escape", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "package-lock.json")
		if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "package-lock.json")); err != nil {
			t.Fatal(err)
		}
		_, err := Inspect(root)
		if err == nil || !strings.Contains(err.Error(), "escapes workspace root") {
			t.Fatalf("Inspect() error = %v, want symlink escape error", err)
		}
	})

	t.Run("oversized declaration", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "package-lock.json")
		if err := os.WriteFile(path, bytes.Repeat([]byte{' '}, int(MaxFileBytes)+1), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Inspect(root)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("Inspect() error = %v, want oversized input error", err)
		}
	})
}

func TestInspectSkipsGeneratedDevenvState(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "devenv.nix", `{ services.redis.enable = true; }`)
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".devenv"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".devenv", "generated-input")); err != nil {
		t.Fatal(err)
	}

	facts, err := Inspect(root)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if len(facts.Devenv) != 1 || facts.Devenv[0].Path != "devenv.nix" {
		t.Fatalf("Devenv = %#v, want root declaration only", facts.Devenv)
	}
}

func TestInspectHostileDevenvDeclarationNeverExecutes(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "executed")
	declaration := `{
		processes.hostile.exec = "touch ` + sentinel + `";
		services.redis.enable = true;
	}`
	writeTestFile(t, root, "devenv.nix", declaration)

	facts, err := Inspect(root)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("hostile declaration executed or stat failed: %v", err)
	}
	if len(facts.Devenv) != 1 || !slices.Equal(facts.Devenv[0].Processes, []string{"hostile"}) || !slices.Equal(facts.Devenv[0].Services, []string{"redis"}) {
		t.Fatalf("Devenv facts = %#v", facts.Devenv)
	}
}

type treeEntry struct {
	Mode os.FileMode
	Data string
}

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	source := filepath.Join("testdata", name)
	destination := t.TempDir()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return destination
}

func snapshotTree(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	result := make(map[string]treeEntry)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		state := treeEntry{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			state.Data = string(data)
		}
		result[filepath.ToSlash(rel)] = state
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func writeTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
