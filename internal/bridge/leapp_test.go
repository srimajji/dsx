package bridge

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestResolveLeappDirectoryRequiresCanonicalPhysicalDirectory(t *testing.T) {
	directory := leappFixture(t, "[default]\nregion=eu-west-1\n", "[default]\naws_access_key_id=secret\n")
	authority, err := ResolveLeappDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if authority.DeclaredPath != directory || authority.CanonicalPath != directory || !strings.Contains(authority.Identity, "dev=") || !strings.Contains(authority.Identity, ";ino=") {
		t.Fatalf("authority = %#v", authority)
	}

	for name, source := range map[string]string{
		"relative": "relative/aws",
		"unclean":  directory + string(filepath.Separator),
		"missing":  filepath.Join(filepath.Dir(directory), "missing-aws"),
		"regular":  filepath.Join(directory, leappConfigFile),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveLeappDirectory(source); err == nil {
				t.Fatalf("ResolveLeappDirectory(%q) succeeded", source)
			}
		})
	}

	symlink := filepath.Join(filepath.Dir(directory), "linked-aws")
	if err := os.Symlink(directory, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveLeappDirectory(symlink); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestResolveLeappDirectoryRejectsMissingAndSymlinkStandardFiles(t *testing.T) {
	missing := canonicalTemporaryDirectory(t)
	if err := os.WriteFile(filepath.Join(missing, leappConfigFile), []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveLeappDirectory(missing); err == nil || !strings.Contains(err.Error(), leappCredentialsFile) {
		t.Fatalf("missing credentials error = %v", err)
	}

	target := filepath.Join(filepath.Dir(missing), "outside-credentials")
	if err := os.WriteFile(target, []byte("never-print-this-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(missing, leappCredentialsFile)); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveLeappDirectory(missing)
	if err == nil || !strings.Contains(err.Error(), leappCredentialsFile) {
		t.Fatalf("credential symlink error = %v", err)
	}
	if strings.Contains(err.Error(), "never-print-this-credential") {
		t.Fatalf("credential value appeared in error: %v", err)
	}

	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		t.Fatal(homeErr)
	}
	physicalHome, homeErr := filepath.EvalSymlinks(home)
	if homeErr != nil {
		t.Fatal(homeErr)
	}
	if _, err := ResolveLeappDirectory(physicalHome); err == nil || !strings.Contains(err.Error(), "current user home") {
		t.Fatalf("home mount error = %v", err)
	}
}

func TestOpenApprovedLeappDirectoryRejectsReplacementIdentity(t *testing.T) {
	directory := leappFixture(t, "config-old", "credentials-old")
	approved, err := ResolveLeappDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	original := directory + ".original"
	if err := os.Rename(directory, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLeappFiles(t, directory, "config-new", "credentials-new")

	replacement, err := ResolveLeappDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Identity == approved.Identity {
		t.Fatalf("replacement retained identity %q", approved.Identity)
	}
	if opened, err := OpenApprovedLeappDirectory(approved); err == nil {
		_ = opened.Close()
		t.Fatal("replacement directory retained approval")
	}
}

func TestLeappGrantIsExactReadOnlyStandardAWSGrant(t *testing.T) {
	directory := leappFixture(t, "config-secret-value", "credential-secret-value")
	authority, err := ResolveLeappDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := LeappGrant(authority, "engineering")
	if err != nil {
		t.Fatal(err)
	}
	wantEnvironment := []string{
		"AWS_CONFIG_FILE=/run/dsx/aws/current/config",
		"AWS_SHARED_CREDENTIALS_FILE=/run/dsx/aws/current/credentials",
		"AWS_PROFILE=engineering",
	}
	if grant.Source != directory || grant.Target != "/run/dsx/aws" || !grant.ReadOnly || !reflect.DeepEqual(grant.Environment, wantEnvironment) {
		t.Fatalf("grant = %#v", grant)
	}
	if !strings.Contains(grant.Warning, "every profile") || !strings.Contains(grant.Warning, "not credential isolation") {
		t.Fatalf("warning = %q", grant.Warning)
	}
	serialized := grant.Warning + strings.Join(grant.Environment, "\n")
	if strings.Contains(serialized, "config-secret-value") || strings.Contains(serialized, "credential-secret-value") {
		t.Fatalf("grant exposed credential contents: %q", serialized)
	}

	withoutProfile, err := LeappGrant(authority, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutProfile.Environment) != 2 {
		t.Fatalf("optional profile environment = %#v", withoutProfile.Environment)
	}
}

func TestOpenedLeappDirectoryObservesCompleteAtomicRename(t *testing.T) {
	oldBytes := strings.Repeat("old-credential-block\n", 128)
	newBytes := strings.Repeat("new-credential-block\n", 128)
	directory := leappFixture(t, "config", oldBytes)
	authority, err := ResolveLeappDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenApprovedLeappDirectory(authority)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	if got := readOpenedLeappFile(t, opened.fd, leappCredentialsFile); got != oldBytes {
		t.Fatalf("initial bytes were partial: got %d want %d", len(got), len(oldBytes))
	}
	temporary := filepath.Join(directory, ".credentials.next")
	if err := os.WriteFile(temporary, []byte(newBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, filepath.Join(directory, leappCredentialsFile)); err != nil {
		t.Fatal(err)
	}
	if got := readOpenedLeappFile(t, opened.fd, leappCredentialsFile); got != newBytes {
		t.Fatalf("rotated bytes were partial: got %d want %d", len(got), len(newBytes))
	}
}

func leappFixture(t *testing.T, config, credentials string) string {
	t.Helper()
	directory := canonicalTemporaryDirectory(t)
	writeLeappFiles(t, directory, config, credentials)
	return directory
}

func canonicalTemporaryDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory = filepath.Clean(directory)
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func writeLeappFiles(t *testing.T, directory, config, credentials string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, leappConfigFile), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, leappCredentialsFile), []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readOpenedLeappFile(t *testing.T, directoryFD int, name string) string {
	t.Helper()
	fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		t.Fatal("create file from descriptor")
	}
	defer file.Close()
	contents, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
