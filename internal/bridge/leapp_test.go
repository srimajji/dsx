package bridge

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestResolveHostAWSDirectoryRequiresCanonicalPhysicalDirectory(t *testing.T) {
	directory := hostAWSFixture(t, "[default]\nregion=eu-west-1\n", "[default]\naws_access_key_id=secret\n")
	authority, err := ResolveHostAWSDirectory(directory)
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
		"regular":  filepath.Join(directory, hostAWSConfigFile),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveHostAWSDirectory(source); err == nil {
				t.Fatalf("ResolveHostAWSDirectory(%q) succeeded", source)
			}
		})
	}

	symlink := filepath.Join(filepath.Dir(directory), "linked-aws")
	if err := os.Symlink(directory, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveHostAWSDirectory(symlink); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestResolveHostAWSDirectoryAuthorityIgnoresCredentialAvailabilityAndContent(t *testing.T) {
	directory := canonicalTemporaryDirectory(t)
	authority, err := ResolveHostAWSDirectory(directory)
	if err != nil {
		t.Fatalf("resolve unavailable host default directory: %v", err)
	}
	if authority.CanonicalPath != directory || authority.Identity == "" {
		t.Fatalf("directory authority = %#v", authority)
	}
	if opened, err := OpenApprovedHostAWSDirectory(authority); err == nil {
		_ = opened.Close()
		t.Fatal("runtime opened unavailable credential source")
	} else if !errors.Is(err, ErrHostAWSSourceUnsafe) {
		t.Fatalf("runtime unavailable source error = %v", err)
	}

	configSecret := "never-print-this-config-secret"
	credentialSecret := "never-print-this-credential-secret"
	if err := os.WriteFile(filepath.Join(directory, hostAWSConfigFile), []byte(configSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, hostAWSCredentialsFile), []byte(credentialSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	withCredentials, err := ResolveHostAWSDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if withCredentials != authority {
		t.Fatalf("credential availability/content changed directory authority: unavailable=%#v available=%#v", authority, withCredentials)
	}

	if err := os.Remove(filepath.Join(directory, hostAWSCredentialsFile)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(directory), "outside-credentials")
	if err := os.WriteFile(target, []byte(credentialSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, hostAWSCredentialsFile)); err != nil {
		t.Fatal(err)
	}
	unsafeAuthority, err := ResolveHostAWSDirectory(directory)
	if err != nil || unsafeAuthority != authority {
		t.Fatalf("source authority inspected credential availability/content: authority=%#v err=%v", unsafeAuthority, err)
	}
	if opened, err := OpenApprovedHostAWSDirectory(authority); err == nil {
		_ = opened.Close()
		t.Fatal("runtime opened unsafe credential source")
	} else if strings.Contains(err.Error(), credentialSecret) {
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
	if _, err := ResolveHostAWSDirectory(physicalHome); err == nil || !strings.Contains(err.Error(), "current user home") {
		t.Fatalf("home mount error = %v", err)
	}
}

func TestOpenApprovedHostAWSDirectoryRejectsReplacementIdentity(t *testing.T) {
	directory := hostAWSFixture(t, "config-old", "credentials-old")
	approved, err := ResolveHostAWSDirectory(directory)
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
	writeHostAWSFiles(t, directory, "config-new", "credentials-new")

	replacement, err := ResolveHostAWSDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Identity == approved.Identity {
		t.Fatalf("replacement retained identity %q", approved.Identity)
	}
	if opened, err := OpenApprovedHostAWSDirectory(approved); err == nil {
		_ = opened.Close()
		t.Fatal("replacement directory retained approval")
	}
}

func TestHostDefaultGrantIsReadOnlyAndDoesNotSetAWSProfile(t *testing.T) {
	directory := hostAWSFixture(t, "config-secret-value", "credential-secret-value")
	authority, err := ResolveHostAWSDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := HostDefaultGrant(authority)
	if err != nil {
		t.Fatal(err)
	}
	wantEnvironment := []string{
		"AWS_CONFIG_FILE=" + HostAWSConfigGuestPath,
		"AWS_SHARED_CREDENTIALS_FILE=" + HostAWSCredentialsGuestPath,
	}
	if grant.Source != directory || grant.Target != HostAWSGuestDirectory || !grant.ReadOnly || !reflect.DeepEqual(grant.Environment, wantEnvironment) {
		t.Fatalf("grant = %#v", grant)
	}
	for _, entry := range grant.Environment {
		if strings.HasPrefix(entry, "AWS_PROFILE=") {
			t.Fatalf("default-only grant set AWS_PROFILE: %#v", grant.Environment)
		}
	}
	warning := strings.ToLower(grant.Warning)
	if !strings.Contains(warning, "default only") ||
		!strings.Contains(warning, "named profiles") ||
		strings.Contains(warning, "every profile") {
		t.Fatalf("default-only warning = %q", grant.Warning)
	}
	serialized := grant.Warning + strings.Join(grant.Environment, "\n")
	if strings.Contains(serialized, "config-secret-value") || strings.Contains(serialized, "credential-secret-value") {
		t.Fatalf("grant exposed credential contents: %q", serialized)
	}
}

func TestOpenedHostAWSDirectoryObservesCompleteAtomicRename(t *testing.T) {
	oldBytes := strings.Repeat("old-credential-block\n", 128)
	newBytes := strings.Repeat("new-credential-block\n", 128)
	directory := hostAWSFixture(t, "config", oldBytes)
	authority, err := ResolveHostAWSDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenApprovedHostAWSDirectory(authority)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	if got := readOpenedHostAWSFile(t, opened.fd, hostAWSCredentialsFile); got != oldBytes {
		t.Fatalf("initial bytes were partial: got %d want %d", len(got), len(oldBytes))
	}
	temporary := filepath.Join(directory, ".credentials.next")
	if err := os.WriteFile(temporary, []byte(newBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, filepath.Join(directory, hostAWSCredentialsFile)); err != nil {
		t.Fatal(err)
	}
	if got := readOpenedHostAWSFile(t, opened.fd, hostAWSCredentialsFile); got != newBytes {
		t.Fatalf("rotated bytes were partial: got %d want %d", len(got), len(newBytes))
	}
}

func hostAWSTemporaryCredentials(generation string) string {
	return "[default]\n" +
		"aws_access_key_id = access-" + generation + "\n" +
		"aws_secret_access_key = secret-" + generation + "\n" +
		"aws_session_token = token-" + generation + "\n"
}

func hostAWSFixture(t *testing.T, config, credentials string) string {
	t.Helper()
	directory := canonicalTemporaryDirectory(t)
	writeHostAWSFiles(t, directory, config, credentials)
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

func writeHostAWSFiles(t *testing.T, directory, config, credentials string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, hostAWSConfigFile), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, hostAWSCredentialsFile), []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readOpenedHostAWSFile(t *testing.T, directoryFD int, name string) string {
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
