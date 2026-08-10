package guest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/model"
	"golang.org/x/sys/unix"
)

func TestStageSecretEnvironmentCreatesExclusivePrivateFile(t *testing.T) {
	name := secretEnvironmentTestPath(t, "00000000000000000000000000000009")
	contents := []byte("TOKEN=secret\x00")
	if err := StageSecretEnvironment(name, bytes.NewReader(contents)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("staged environment mode = %v", info.Mode())
	}
	if err := StageSecretEnvironment(name, bytes.NewReader(contents)); err == nil {
		t.Fatal("existing secret environment was overwritten")
	}
	overlay, err := loadSecretEnvironment(name)
	if err != nil || !reflect.DeepEqual(overlay, []string{"TOKEN=secret"}) {
		t.Fatalf("loaded staged environment = %#v, %v", overlay, err)
	}
	if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed environment remains: %v", err)
	}

	invalid := secretEnvironmentTestPath(t, "0000000000000000000000000000000a")
	if err := StageSecretEnvironment(invalid, bytes.NewReader([]byte("TOKEN=unterminated"))); err == nil {
		t.Fatal("malformed secret environment was staged")
	}
	if _, err := os.Lstat(invalid); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected staged environment remains: %v", err)
	}
}

func TestSecretEnvironmentIsValidatedUnlinkedAndOverlaid(t *testing.T) {
	name := secretEnvironmentTestPath(t, "00000000000000000000000000000001")
	writeSecretEnvironmentTestFile(t, name, []byte("HEADER=Bearer secret\x00OPENCODE_CONFIG_CONTENT={secret}\x00"), 0o600)

	overlay, err := loadSecretEnvironment(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("environment file exists before child: %v", err)
	}
	got := overlayEnvironment([]string{"PATH=/usr/bin", "HEADER=old", "ORDINARY=yes"}, overlay)
	want := []string{"PATH=/usr/bin", "ORDINARY=yes", "HEADER=Bearer secret", "OPENCODE_CONFIG_CONTENT={secret}"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlayEnvironment() = %#v, want %#v", got, want)
	}
}

func TestSecretEnvironmentRejectsUnsafeFilesAndContents(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		name := secretEnvironmentTestPath(t, "00000000000000000000000000000002")
		target := filepath.Join(filepath.Dir(name), "target")
		writeSecretEnvironmentTestFile(t, target, []byte("TOKEN=secret\x00"), 0o600)
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSecretEnvironment(name); err == nil {
			t.Fatal("accepted symlink")
		}
		if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejected symlink remains: %v", err)
		}
	})
	t.Run("world-readable", func(t *testing.T) {
		name := secretEnvironmentTestPath(t, "00000000000000000000000000000003")
		writeSecretEnvironmentTestFile(t, name, []byte("TOKEN=secret\x00"), 0o644)
		if _, err := loadSecretEnvironment(name); err == nil {
			t.Fatal("accepted world-readable file")
		}
	})
	t.Run("not-regular", func(t *testing.T) {
		name := secretEnvironmentTestPath(t, "00000000000000000000000000000004")
		if err := unix.Mkfifo(name, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSecretEnvironment(name); err == nil {
			t.Fatal("accepted non-regular file")
		}
	})
	for _, test := range []struct {
		name     string
		leaf     string
		contents []byte
	}{
		{name: "duplicate", leaf: "00000000000000000000000000000005", contents: []byte("TOKEN=one\x00TOKEN=two\x00")},
		{name: "embedded NUL", leaf: "00000000000000000000000000000006", contents: []byte("TOKEN=one\x00injected\x00")},
		{name: "missing terminator", leaf: "00000000000000000000000000000007", contents: []byte("TOKEN=secret")},
		{name: "oversize", leaf: "00000000000000000000000000000008", contents: append([]byte("TOKEN="), append([]byte(strings.Repeat("x", maxSecretEnvironmentBytes)), 0)...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := secretEnvironmentTestPath(t, test.leaf)
			writeSecretEnvironmentTestFile(t, name, test.contents, 0o600)
			if _, err := loadSecretEnvironment(name); err == nil {
				t.Fatalf("accepted %s environment", test.name)
			}
			if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected file was not unlinked: %v", err)
			}
		})
	}
}

func TestSecretEnvironmentRejectsUnauthorizedPathsAndWrongOwner(t *testing.T) {
	for _, name := range []string{
		"/tmp/dsx-run/../env-00000000000000000000000000000000",
		"/tmp/not-dsx-run/00000000-0000-7000-8000-000000000000/env-00000000000000000000000000000000",
		"/tmp/dsx-run/00000000-0000-4000-8000-000000000000/env-00000000000000000000000000000000",
		"/tmp/dsx-run/00000000-0000-7000-8000-000000000000/other",
	} {
		if err := validateSecretEnvironmentPath(name); err == nil {
			t.Fatalf("accepted unauthorized path %q", name)
		}
	}
	metadata := unix.Stat_t{Mode: unix.S_IFREG | 0o600, Uid: 41, Gid: 43, Size: 10}
	if err := validateSecretEnvironmentMetadata(metadata, 42, 43); err == nil {
		t.Fatal("accepted wrong owner")
	}
}

func secretEnvironmentTestPath(t *testing.T, leaf string) string {
	t.Helper()
	runID, err := model.NewRunID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("/tmp/dsx-run", string(runID))
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	harnessRoot := filepath.Join(guestTemporaryRootPath, "dsx-run")
	if err := os.Chown(harnessRoot, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(harnessRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return filepath.Join(root, "env-"+leaf)
}

func writeSecretEnvironmentTestFile(t *testing.T, name string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, contents, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}
