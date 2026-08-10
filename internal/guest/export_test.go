package guest

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/model"
	"golang.org/x/sys/unix"
)

func exportTestAuthPath(t *testing.T, leaf string) (string, string) {
	t.Helper()
	runID, err := model.NewRunID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ensureTestHarnessRoot(t)
	runRoot := filepath.Join(guestTemporaryRootPath, "dsx-run", string(runID))
	t.Cleanup(func() { _ = os.RemoveAll(runRoot) })
	guestDirectory := filepath.ToSlash(filepath.Join("/tmp/dsx-run", string(runID), "auth"))
	if err := EnsureRunDirectory(guestDirectory); err != nil {
		t.Fatal(err)
	}
	return guestDirectory + "/" + leaf, filepath.Join(runRoot, "auth", leaf)
}

func TestExportMetadataTimeChangedDetectsCTimeMutation(t *testing.T) {
	before := unix.Stat_t{Ctim: unix.Timespec{Sec: 42, Nsec: 7}}
	if exportMetadataTimeChanged(before, before) {
		t.Fatal("unchanged ctime was reported as changed")
	}

	secondsChanged := before
	secondsChanged.Ctim.Sec++
	if !exportMetadataTimeChanged(before, secondsChanged) {
		t.Fatal("ctime seconds mutation was not detected")
	}

	nanosecondsChanged := before
	nanosecondsChanged.Ctim.Nsec++
	if !exportMetadataTimeChanged(before, nanosecondsChanged) {
		t.Fatal("ctime nanoseconds mutation was not detected")
	}
}

func TestExportRunFileExactLimitAndOptionalAbsence(t *testing.T) {
	guestPath, hostPath := exportTestAuthPath(t, "auth.json")
	contents := []byte(`{"token":"exact"}`)
	if err := os.WriteFile(hostPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ExportRunFile(guestPath, ExportAuth, int64(len(contents)), &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), contents) {
		t.Fatalf("export = %q", output.Bytes())
	}
	missing, _ := exportTestAuthPath(t, "missing.json")
	if err := ExportRunFile(missing, ExportAuth, 64, io.Discard); !errors.Is(err, ErrRunArtifactMissing) {
		t.Fatalf("missing error = %v", err)
	}
}

func TestExportRunResultAllowsBoundedBinaryBundleAtExactPath(t *testing.T) {
	runID, err := model.NewRunID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ensureTestHarnessRoot(t)
	runRoot := filepath.Join(guestTemporaryRootPath, "dsx-run", string(runID))
	t.Cleanup(func() { _ = os.RemoveAll(runRoot) })
	guestDirectory := filepath.ToSlash(filepath.Join("/tmp/dsx-run", string(runID), "tmp"))
	if err := EnsureRunDirectory(guestDirectory); err != nil {
		t.Fatal(err)
	}
	guestPath := guestDirectory + "/result-0.bundle"
	contents := []byte{'b', 0, 'u', 'n', 'd', 'l', 'e'}
	if err := os.WriteFile(filepath.Join(runRoot, "tmp", "result-0.bundle"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ExportRunFile(guestPath, ExportResult, int64(len(contents)), &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), contents) {
		t.Fatalf("bundle export = %q", output.Bytes())
	}
	if err := ExportRunFile(guestDirectory+"/other.bundle", ExportResult, 64, io.Discard); err == nil {
		t.Fatal("non-allowlisted result path was exported")
	}
	if err := RemoveRunExportFile(guestPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(runRoot, "tmp", "result-0.bundle")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("result cleanup left bundle: %v", err)
	}
}

func TestExportRunFileRejectsUnsafeFileTypesLinksSizeAndNUL(t *testing.T) {
	t.Run("symlink to large file", func(t *testing.T) {
		guestPath, hostPath := exportTestAuthPath(t, "auth.json")
		large := filepath.Join(t.TempDir(), "large")
		if err := os.WriteFile(large, bytes.Repeat([]byte{'x'}, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(large, hostPath); err != nil {
			t.Fatal(err)
		}
		if err := ExportRunFile(guestPath, ExportAuth, 32, io.Discard); err == nil {
			t.Fatal("symlink was exported")
		}
	})
	t.Run("hardlink from outside", func(t *testing.T) {
		guestPath, hostPath := exportTestAuthPath(t, "auth.json")
		outside := filepath.Join(filepath.Dir(filepath.Dir(hostPath)), "outside")
		if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(outside, hostPath); err != nil {
			t.Fatal(err)
		}
		if err := ExportRunFile(guestPath, ExportAuth, 32, io.Discard); err == nil {
			t.Fatal("multiply-linked file was exported")
		}
	})
	t.Run("FIFO", func(t *testing.T) {
		guestPath, hostPath := exportTestAuthPath(t, "auth.json")
		if err := unix.Mkfifo(hostPath, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ExportRunFile(guestPath, ExportAuth, 32, io.Discard); err == nil {
			t.Fatal("FIFO was exported")
		}
	})
	t.Run("device metadata", func(t *testing.T) {
		metadata := unix.Stat_t{Mode: unix.S_IFCHR | 0o600, Uid: uint32(os.Geteuid()), Gid: uint32(os.Getegid()), Nlink: 1}
		if err := validateExportFile(metadata, metadata.Uid, metadata.Gid, 32); err == nil {
			t.Fatal("device metadata was accepted")
		}
	})
	t.Run("wrong owner or mode metadata", func(t *testing.T) {
		uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
		wrongOwner := unix.Stat_t{Mode: unix.S_IFREG | 0o600, Uid: uid + 1, Gid: gid, Nlink: 1}
		if err := validateExportFile(wrongOwner, uid, gid, 32); err == nil {
			t.Fatal("wrong owner was accepted")
		}
		wrongMode := unix.Stat_t{Mode: unix.S_IFREG | 0o644, Uid: uid, Gid: gid, Nlink: 1}
		if err := validateExportFile(wrongMode, uid, gid, 32); err == nil {
			t.Fatal("permissive mode was accepted")
		}
	})
	t.Run("size plus one", func(t *testing.T) {
		guestPath, hostPath := exportTestAuthPath(t, "auth.json")
		if err := os.WriteFile(hostPath, []byte("123456789"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ExportRunFile(guestPath, ExportAuth, 8, io.Discard); err == nil {
			t.Fatal("oversized file was exported")
		}
	})
	t.Run("NUL", func(t *testing.T) {
		guestPath, hostPath := exportTestAuthPath(t, "auth.json")
		if err := os.WriteFile(hostPath, []byte{'a', 0, 'b'}, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ExportRunFile(guestPath, ExportAuth, 8, io.Discard); err == nil {
			t.Fatal("NUL credential was exported")
		}
	})
}

func TestExportRunFileDetectsGrowthAndTruncationDuringRead(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string) error
	}{
		{name: "growth", mutate: func(name string) error {
			file, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			_, writeErr := file.Write([]byte("growth"))
			return errors.Join(writeErr, file.Close())
		}},
		{name: "short read", mutate: func(name string) error { return os.Truncate(name, 0) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			guestPath, hostPath := exportTestAuthPath(t, "auth.json")
			contents := bytes.Repeat([]byte{'x'}, 128<<10)
			if err := os.WriteFile(hostPath, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			output := &mutatingExportWriter{name: hostPath, mutate: test.mutate}
			if err := ExportRunFile(guestPath, ExportAuth, int64(len(contents)+16), output); err == nil {
				t.Fatal("concurrent file mutation was not detected")
			}
		})
	}
}

type mutatingExportWriter struct {
	name    string
	mutate  func(string) error
	mutated bool
}

func (writer *mutatingExportWriter) Write(contents []byte) (int, error) {
	if !writer.mutated {
		writer.mutated = true
		if err := writer.mutate(writer.name); err != nil {
			return 0, err
		}
	}
	return len(contents), nil
}
