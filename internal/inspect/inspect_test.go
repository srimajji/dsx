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
	if len(first.DevContainers) != 1 {
		t.Fatalf("DevContainers = %#v, want one", first.DevContainers)
	}
	container := first.DevContainers[0]
	if container.Path != ".devcontainer/devcontainer.json" || container.Name != "composite" || container.Build.Dockerfile != "../Dockerfile" || container.Build.Context != ".." || container.WorkspaceFolder != "/workspace" {
		t.Errorf("DevContainer = %#v", container)
	}
	if got, want := container.ForwardPorts, []string{"3000", "localhost:8080"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ForwardPorts = %v, want %v", got, want)
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
	if len(first.Diagnostics) != 1 || first.Diagnostics[0].Severity != SeverityWarning || first.Diagnostics[0].Field != "/customizations" {
		t.Errorf("Diagnostics = %#v, want unsupported customizations warning", first.Diagnostics)
	}
	if after := snapshotTree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("inspection changed filesystem state:\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestInspectDevContainerUnsupportedAndMalformedInputExplicit(t *testing.T) {
	t.Run("unsupported fields are classified", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, root, ".devcontainer/devcontainer.json", `{
			"name": "unsafe",
			"initializeCommand": ["sh", "-c", "exit 9"],
			"mounts": ["source=/,target=/host,type=bind"],
			"hostRequirements": {"cpus": 2}
		}`)
		facts, err := Inspect(root)
		if err != nil {
			t.Fatalf("Inspect() error = %v", err)
		}
		severities := make(map[string]Severity)
		for _, diagnostic := range facts.Diagnostics {
			severities[diagnostic.Field] = diagnostic.Severity
		}
		if severities["/initializeCommand"] != SeverityError || severities["/mounts"] != SeverityError {
			t.Errorf("security diagnostics = %#v, want errors", facts.Diagnostics)
		}
		if severities["/hostRequirements"] != SeverityWarning {
			t.Errorf("hostRequirements severity = %q, want warning", severities["/hostRequirements"])
		}
		if !facts.HasErrors() {
			t.Error("Facts.HasErrors() = false, want true")
		}
	})

	for name, content := range map[string]string{
		"syntax":    `{"name":`,
		"duplicate": `{"image":"one", "image":"two"}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, root, ".devcontainer/devcontainer.json", content)
			facts, err := Inspect(root)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if len(facts.Diagnostics) != 1 || facts.Diagnostics[0].Code != "malformed_devcontainer" || facts.Diagnostics[0].Severity != SeverityError {
				t.Fatalf("Diagnostics = %#v, want one explicit malformed error", facts.Diagnostics)
			}
		})
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
		path := filepath.Join(root, ".devcontainer", "devcontainer.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
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

func TestImportedDevContainerContentDigestFacts(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".devcontainer/devcontainer.json", `{"image":"example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	first, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.DevContainers) != 1 || len(first.DevContainers[0].ContentDigest) != 64 {
		t.Fatalf("DevContainer digest fact = %#v", first.DevContainers)
	}
	writeTestFile(t, root, ".devcontainer/devcontainer.json", "{\n  \"image\": \"example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"\n}\n")
	second, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if first.DevContainers[0].ContentDigest == second.DevContainers[0].ContentDigest {
		t.Fatal("DevContainer byte change did not alter content digest fact")
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
