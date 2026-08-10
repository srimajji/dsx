package apple

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverResolvesTrustedHomebrewSymlink(t *testing.T) {
	root := t.TempDir()
	cellar := filepath.Join(root, "opt", "homebrew", "Cellar", "container")
	resolved := filepath.Join(cellar, "1.2.2", "bin", "container")
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolved, []byte("container"), 0o555); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(root, "opt", "homebrew", "bin", "container")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "Cellar", "container", "1.2.2", "bin", "container"), candidate); err != nil {
		t.Fatal(err)
	}

	got, err := discoverContainerExecutable([]string{candidate}, []string{cellar})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if got != resolved {
		t.Fatalf("resolved executable = %q, want %q", got, resolved)
	}
}

func TestDiscoverIgnoresAmbientPathAndEnvironment(t *testing.T) {
	root := t.TempDir()
	fake := filepath.Join(root, "bin", "container")
	if err := os.MkdirAll(filepath.Dir(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake))
	t.Setenv("DSX_APPLE_CONTAINER", fake)

	_, err := discoverContainerExecutable([]string{filepath.Join(root, "supported", "container")}, []string{filepath.Join(root, "Cellar", "container")})
	if err == nil {
		t.Fatal("ambient executable was accepted")
	}
}

func TestDiscoverRejectsUntrustedAbsolutePayload(t *testing.T) {
	root := t.TempDir()
	payload := filepath.Join(root, "payload", "container")
	if err := os.MkdirAll(filepath.Dir(payload), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payload, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveTrustedContainerExecutable(payload, []string{filepath.Join(root, "Cellar", "container")}); err == nil {
		t.Fatal("untrusted absolute executable was accepted")
	}
}

func TestDiscoverRequiresExecutableRegularFile(t *testing.T) {
	root := t.TempDir()
	cellar := filepath.Join(root, "Cellar", "container")
	nonExecutable := filepath.Join(cellar, "1.2.2", "bin", "container")
	if err := os.MkdirAll(filepath.Dir(nonExecutable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nonExecutable, []byte("container"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTrustedContainerExecutable(nonExecutable, []string{cellar}); err == nil {
		t.Fatal("non-executable regular file was accepted")
	}

	directory := filepath.Join(cellar, "1.2.3", "bin", "container")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTrustedContainerExecutable(directory, []string{cellar}); err == nil {
		t.Fatal("directory was accepted as an executable")
	}
}

func TestDiscoverCurrentHomebrewInstall(t *testing.T) {
	const candidate = "/opt/homebrew/bin/container"
	if _, err := os.Lstat(candidate); err != nil {
		if os.IsNotExist(err) {
			t.Skip("Homebrew container is not installed")
		}
		t.Fatal(err)
	}
	resolved, err := DiscoverContainerExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "/opt/homebrew/Cellar/container/1.2.2/bin/container" {
		t.Fatalf("resolved current Homebrew executable = %q", resolved)
	}
}
