package harness

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestValidateRootsExecAndAuthRejectAmbientOrEscapingContracts(t *testing.T) {
	roots := RunRoots{Workspace: "/workspace", Home: "/run/dsx/home", Auth: "/run/dsx/auth", Config: "/run/dsx/config", ReadOnlyConfig: "/run/dsx/readonly-config", Data: "/run/dsx/data", Cache: "/run/dsx/cache", Temporary: "/run/dsx/tmp"}
	if err := ValidateRoots(roots); err != nil {
		t.Fatal(err)
	}
	roots.Auth = roots.Home
	if err := ValidateRoots(roots); err == nil {
		t.Fatal("accepted aliased writable roots")
	}
	if err := ValidateExecSpec(ExecSpec{Argv: []string{"codex", "exec", "hello"}, Cwd: "/workspace", Env: map[string]string{"CODEX_HOME": "/run/dsx/auth"}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExecSpec(ExecSpec{Argv: []string{"codex"}, Cwd: "workspace"}); err == nil {
		t.Fatal("accepted relative working directory")
	}
	if err := ValidateAuthLayout(AuthLayout{Backend: StorageFile, CredentialArtifacts: []string{"../auth.json"}, MaxArtifactBytes: 1 << 20}); err == nil {
		t.Fatal("accepted escaping credential artifact")
	}
	if err := ValidateAuthLayout(AuthLayout{Backend: StorageFile, CredentialArtifacts: []string{"auth.json"}}); err == nil {
		t.Fatal("accepted missing auth artifact size limit")
	}
	if err := ValidateAuthLayout(AuthLayout{Backend: StorageFile, CredentialArtifacts: []string{"auth.json"}, MaxArtifactBytes: MaxAuthArtifactBytes + 1}); err == nil {
		t.Fatal("accepted unsupported auth artifact size limit")
	}
}

func TestSeedArtifactsCopiesOnlyAllowlistPrivatelyAndRejectsSymlink(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "session.json"), []byte("session"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedArtifacts(context.Background(), SeedRequest{SourceRoot: source, DestinationRoot: destination, Artifacts: []string{"auth.json"}, MaxArtifactBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(destination, "auth.json"))
	if err != nil || string(copied) != "token" {
		t.Fatalf("seeded auth = %q, %v", copied, err)
	}
	if info, err := os.Stat(filepath.Join(destination, "auth.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("seed mode = %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "session.json")); !os.IsNotExist(err) {
		t.Fatalf("non-allowlisted session copied: %v", err)
	}
	if err := os.Symlink(filepath.Join(source, "auth.json"), filepath.Join(source, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if err := SeedArtifacts(context.Background(), SeedRequest{SourceRoot: source, DestinationRoot: destination, Artifacts: []string{"linked.json"}, MaxArtifactBytes: 1 << 20}); err == nil {
		t.Fatal("accepted symlink seed")
	}
	oversized := filepath.Join(source, "oversized.json")
	if err := os.WriteFile(oversized, make([]byte, (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedArtifacts(context.Background(), SeedRequest{SourceRoot: source, DestinationRoot: destination, Artifacts: []string{"oversized.json"}, MaxArtifactBytes: 1 << 20}); err == nil {
		t.Fatal("accepted oversized seed")
	}
	if _, err := os.Lstat(filepath.Join(destination, "oversized.json")); !os.IsNotExist(err) {
		t.Fatalf("oversized destination exists: %v", err)
	}
}

func TestSortedEnvironmentIsDeterministic(t *testing.T) {
	got := SortedEnvironment(map[string]string{"Z": "last", "A": "first"})
	if !reflect.DeepEqual(got, []string{"A=first", "Z=last"}) {
		t.Fatalf("SortedEnvironment() = %#v", got)
	}
}
