package guest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/model"
)

func producerTestResultPath(t *testing.T) (string, string) {
	t.Helper()
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
	return guestDirectory + "/result-0.bundle", filepath.Join(runRoot, "tmp", "result-0.bundle")
}

func TestProduceRunFileTerminatesOversizedProducerAndRemovesPartialArtifact(t *testing.T) {
	executable, err := exec.LookPath("yes")
	if err != nil {
		t.Skip("yes is unavailable")
	}
	guestPath, hostPath := producerTestResultPath(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = ProduceRunFile(ctx, guestPath, 32, guestproto.CommandSpec{Argv: []string{executable, "bundle"}, Cwd: t.TempDir()})
	if !errors.Is(err, errProducerOutputLimit) {
		t.Fatalf("ProduceRunFile() error = %v, want output limit", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("producer was not terminated promptly: %v", ctx.Err())
	}
	if _, err := os.Lstat(hostPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial result artifact remains: %v", err)
	}
	if err := ExportRunFile(guestPath, ExportResult, 33, &bytes.Buffer{}); !errors.Is(err, ErrRunArtifactMissing) {
		t.Fatalf("later export observed failed production: %v", err)
	}
}

func TestProduceRunFileCreatesPrivateDeterministicExport(t *testing.T) {
	executable, err := exec.LookPath("printf")
	if err != nil {
		t.Skip("printf is unavailable")
	}
	guestPath, hostPath := producerTestResultPath(t)
	contents := []byte("deterministic-bundle")
	command := guestproto.CommandSpec{Argv: []string{executable, "%s", string(contents)}, Cwd: t.TempDir()}
	if err := ProduceRunFile(context.Background(), guestPath, int64(len(contents)), command); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != int64(len(contents)) {
		t.Fatalf("produced result metadata = mode %v size %d", info.Mode(), info.Size())
	}
	var first bytes.Buffer
	if err := ExportRunFile(guestPath, ExportResult, int64(len(contents)), &first); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), contents) {
		t.Fatalf("produced result = %q", first.Bytes())
	}
	if err := RemoveRunExportFile(guestPath); err != nil {
		t.Fatal(err)
	}
	if err := ProduceRunFile(context.Background(), guestPath, int64(len(contents)), command); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := ExportRunFile(guestPath, ExportResult, int64(len(contents)), &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("repeated production changed output: first=%q second=%q", first.Bytes(), second.Bytes())
	}
}
