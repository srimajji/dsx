package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/state"
)

func TestApprovalRepositorySaveLoadModesAndReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewApprovalRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	first := approvalFileRecord("a")
	if err := repository.SaveApproval(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	approvalDirectoryPath := filepath.Join(root, approvalDirectory)
	approvalPath := filepath.Join(approvalDirectoryPath, string(first.ProjectID)+".json")
	assertMode(t, root, 0o700)
	assertMode(t, approvalDirectoryPath, 0o700)
	assertMode(t, approvalPath, 0o600)

	loaded, found, err := repository.LoadApproval(context.Background(), first.ProjectID)
	if err != nil || !found {
		t.Fatalf("LoadApproval() = (%#v, %v, %v), want record", loaded, found, err)
	}
	if loaded.Hash != first.Hash {
		t.Fatalf("loaded hash = %q, want %q", loaded.Hash, first.Hash)
	}

	if err := os.Chmod(approvalPath, 0o644); err != nil {
		t.Fatal(err)
	}
	second := approvalFileRecord("b")
	second.ApprovedAt = first.ApprovedAt.Add(time.Minute)
	if err := repository.SaveApproval(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	assertMode(t, approvalPath, 0o600)
	loaded, found, err = repository.LoadApproval(context.Background(), first.ProjectID)
	if err != nil || !found || loaded.Hash != second.Hash || !loaded.ApprovedAt.Equal(second.ApprovedAt) {
		t.Fatalf("replacement LoadApproval() = (%#v, %v, %v)", loaded, found, err)
	}
	entries, err := os.ReadDir(approvalDirectoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(approvalPath) {
		t.Fatalf("atomic replacement left unexpected entries: %#v", entries)
	}
}
func TestApprovalRepositorySaveReportsPostRenameSyncFailureWithRecordInstalled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewApprovalRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	syncFailure := errors.New("directory sync failed")
	repository.syncDirectory = func(string) error { return syncFailure }
	record := approvalFileRecord("a")
	if err := repository.SaveApproval(context.Background(), record); !errors.Is(err, syncFailure) {
		t.Fatalf("SaveApproval() error = %v, want sync failure", err)
	}
	loaded, found, err := repository.LoadApproval(context.Background(), record.ProjectID)
	if err != nil || !found || loaded.Hash != record.Hash {
		t.Fatalf("post-rename LoadApproval() = (%#v, %v, %v), want installed record", loaded, found, err)
	}
}

func TestApprovalRepositoryDeleteExactIdempotentAndSyncsParent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewApprovalRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	record := approvalFileRecord("a")
	if err := repository.SaveApproval(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	syncCalls := 0
	repository.syncDirectory = func(path string) error {
		syncCalls++
		return syncDirectory(path)
	}
	if err := repository.DeleteApproval(context.Background(), record.ProjectID); err != nil {
		t.Fatal(err)
	}
	if syncCalls != 1 {
		t.Fatalf("directory sync calls = %d, want 1", syncCalls)
	}
	if _, found, err := repository.LoadApproval(context.Background(), record.ProjectID); err != nil || found {
		t.Fatalf("LoadApproval() after delete = found %v, err %v", found, err)
	}
	if err := repository.DeleteApproval(context.Background(), record.ProjectID); err != nil {
		t.Fatalf("idempotent DeleteApproval() error = %v", err)
	}
	if syncCalls != 1 {
		t.Fatalf("idempotent delete synced unchanged directory: calls = %d", syncCalls)
	}
}

func TestApprovalRepositoryCorruptRecordFailsClosed(t *testing.T) {
	tests := map[string]string{
		"malformed":       `{`,
		"unknown field":   `{"version":1,"project_id":"aaaaaaaaaaaaaaaaaaaa","hash":"` + strings.Repeat("a", 64) + `","approved_at":"2026-08-09T01:02:03Z","dsx_version":"1","config_content_digest":"` + strings.Repeat("d", 64) + `","imported_content_digests":[],"unexpected":true}`,
		"duplicate field": `{"version":1,"version":1}`,
		"wrong project":   `{"version":1,"project_id":"bbbbbbbbbbbbbbbbbbbb","hash":"` + strings.Repeat("a", 64) + `","approved_at":"2026-08-09T01:02:03Z","dsx_version":"1","config_content_digest":"` + strings.Repeat("d", 64) + `","imported_content_digests":[]}`,
		"multiple values": `{} {}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			repository, projectID, path := corruptRepositoryFixture(t)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, found, err := repository.LoadApproval(context.Background(), projectID)
			if err == nil || found {
				t.Fatalf("LoadApproval() = found %v, err %v; want corrupt failure", found, err)
			}
		})
	}
}

func TestApprovalRepositoryRejectsOversizedAndInsecureRecords(t *testing.T) {
	repository, projectID, path := corruptRepositoryFixture(t)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxApprovalBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repository.LoadApproval(context.Background(), projectID); err == nil || found {
		t.Fatalf("oversized LoadApproval() = found %v, err %v", found, err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repository.LoadApproval(context.Background(), projectID); err == nil || found {
		t.Fatalf("insecure LoadApproval() = found %v, err %v", found, err)
	}
}

func TestApprovalRepositoryNoPathInjectionOrSymlinkRead(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewApprovalRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	invalidIDs := []model.ProjectID{"../../outside.json", "aaaaaaaaaaaaaaaaaaa/", "AAAAAAAAAAAAAAAAAAAA", "aaaaaaaaaaaaaaaaaaa1"}
	for _, projectID := range invalidIDs {
		if _, _, err := repository.LoadApproval(context.Background(), projectID); model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Errorf("LoadApproval(%q) error = %v (code %q), want invalid input", projectID, err, model.ErrorCodeOf(err))
		}
		record := approvalFileRecord("a")
		record.ProjectID = projectID
		if err := repository.SaveApproval(context.Background(), record); model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Errorf("SaveApproval(%q) error = %v (code %q), want invalid input", projectID, err, model.ErrorCodeOf(err))
		}
		if err := repository.DeleteApproval(context.Background(), projectID); model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Errorf("DeleteApproval(%q) error = %v (code %q), want invalid input", projectID, err, model.ErrorCodeOf(err))
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("invalid project IDs created state root: %v", err)
	}

	valid := approvalFileRecord("a")
	if err := os.MkdirAll(filepath.Join(root, approvalDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	approvalPath := filepath.Join(root, approvalDirectory, string(valid.ProjectID)+".json")
	if err := os.Symlink(external, approvalPath); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repository.LoadApproval(context.Background(), valid.ProjectID); err == nil || found {
		t.Fatalf("symlink LoadApproval() = found %v, err %v", found, err)
	}
	if err := repository.SaveApproval(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	externalData, err := os.ReadFile(external)
	if err != nil {
		t.Fatal(err)
	}
	if string(externalData) != "outside" {
		t.Fatalf("save followed symlink and changed external file to %q", externalData)
	}
	if info, err := os.Lstat(approvalPath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("approval replacement is not a regular file: info=%v err=%v", info, err)
	}
}

func TestApprovalRepositoryMissingLoadIsReadOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	repository, err := NewApprovalRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	projectID := model.ProjectID("aaaaaaaaaaaaaaaaaaaa")
	if _, found, err := repository.LoadApproval(context.Background(), projectID); err != nil || found {
		t.Fatalf("missing LoadApproval() = found %v, err %v", found, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("missing load created state: %v", err)
	}
}

func corruptRepositoryFixture(t *testing.T) (*ApprovalRepository, model.ProjectID, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	directory := filepath.Join(root, approvalDirectory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	projectID := model.ProjectID("aaaaaaaaaaaaaaaaaaaa")
	repository, err := NewApprovalRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	return repository, projectID, filepath.Join(directory, string(projectID)+".json")
}

func approvalFileRecord(hashCharacter string) state.ApprovalRecord {
	return state.ApprovalRecord{
		Version:                state.ApprovalRecordVersion,
		ProjectID:              model.ProjectID("aaaaaaaaaaaaaaaaaaaa"),
		Hash:                   strings.Repeat(hashCharacter, 64),
		ApprovedAt:             time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC),
		DSXVersion:             "1.0.0",
		ConfigContentDigest:    strings.Repeat("d", 64),
		ImportedContentDigests: []state.ContentDigest{{Path: "import.json", Digest: strings.Repeat("e", 64)}},
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
