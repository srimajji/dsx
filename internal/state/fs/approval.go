package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/state"
)

const (
	approvalDirectory = "approvals"
	maxApprovalBytes  = 1 << 20
)

// ApprovalRepository stores one bounded approval record per validated project
// ID beneath a caller-supplied DSX state root.
type ApprovalRepository struct {
	root          string
	syncDirectory func(string) error
}

var _ state.ApprovalRepository = (*ApprovalRepository)(nil)

func NewApprovalRepository(stateRoot string) (*ApprovalRepository, error) {
	if stateRoot == "" || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return nil, model.NewError(model.CodeInvalidInput, "DSX state root must be a clean absolute path", nil)
	}
	return &ApprovalRepository{root: stateRoot, syncDirectory: syncDirectory}, nil
}

func (repository *ApprovalRepository) LoadApproval(ctx context.Context, projectID model.ProjectID) (state.ApprovalRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.ApprovalRecord{}, false, model.Wrap(model.CodeUnavailable, "load approval", err)
	}
	path, err := repository.approvalPath(projectID)
	if err != nil {
		return state.ApprovalRecord{}, false, err
	}
	if err := verifyPrivateDirectory(repository.root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state.ApprovalRecord{}, false, nil
		}
		return state.ApprovalRecord{}, false, model.Wrap(model.CodeInternal, "verify DSX state root", err)
	}
	if err := verifyPrivateDirectory(filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state.ApprovalRecord{}, false, nil
		}
		return state.ApprovalRecord{}, false, model.Wrap(model.CodeInternal, "verify approval directory", err)
	}

	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return state.ApprovalRecord{}, false, nil
	}
	if err != nil {
		return state.ApprovalRecord{}, false, model.Wrap(model.CodeInternal, "inspect approval record", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return state.ApprovalRecord{}, false, model.NewError(model.CodeInternal, "approval record is not a mode-0600 regular file", nil)
	}

	file, err := os.Open(path)
	if err != nil {
		return state.ApprovalRecord{}, false, model.Wrap(model.CodeInternal, "open approval record", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxApprovalBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return state.ApprovalRecord{}, false, model.Wrap(model.CodeInternal, "read approval record", readErr)
	}
	if closeErr != nil {
		return state.ApprovalRecord{}, false, model.Wrap(model.CodeInternal, "close approval record", closeErr)
	}
	if len(data) > maxApprovalBytes {
		return state.ApprovalRecord{}, false, model.NewError(model.CodeInternal, "approval record exceeds size limit", nil)
	}
	if err := validateStrictJSON(data); err != nil {
		return state.ApprovalRecord{}, false, corruptApproval(err)
	}

	var record state.ApprovalRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return state.ApprovalRecord{}, false, corruptApproval(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return state.ApprovalRecord{}, false, corruptApproval(err)
	}
	if record.ProjectID != projectID {
		return state.ApprovalRecord{}, false, corruptApproval(fmt.Errorf("record project ID %q does not match filename project ID %q", record.ProjectID, projectID))
	}
	if err := state.ValidateApprovalRecord(record); err != nil {
		return state.ApprovalRecord{}, false, corruptApproval(err)
	}
	return record, true, nil
}

func (repository *ApprovalRepository) SaveApproval(ctx context.Context, record state.ApprovalRecord) error {
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "save approval", err)
	}
	if err := state.ValidateApprovalRecord(record); err != nil {
		return model.NewError(model.CodeInvalidInput, "invalid approval record", err)
	}
	path, err := repository.approvalPath(record.ProjectID)
	if err != nil {
		return err
	}
	record = normalizeRecord(record)
	data, err := json.Marshal(record)
	if err != nil {
		return model.Wrap(model.CodeInternal, "encode approval record", err)
	}
	data = append(data, '\n')
	if len(data) > maxApprovalBytes {
		return model.NewError(model.CodeInvalidInput, "approval record exceeds size limit", nil)
	}
	if err := ensurePrivateDirectory(repository.root); err != nil {
		return model.Wrap(model.CodeInternal, "create DSX state root", err)
	}
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return model.Wrap(model.CodeInternal, "create approval directory", err)
	}
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "save approval", err)
	}
	if err := atomicReplace(directory, path, string(record.ProjectID), data, repository.syncDirectory); err != nil {
		return model.Wrap(model.CodeInternal, "replace approval record", err)
	}
	return nil
}

// DeleteApproval removes exactly the approval derived from projectID. Missing
// repositories and records are already in the requested state and succeed.
func (repository *ApprovalRepository) DeleteApproval(ctx context.Context, projectID model.ProjectID) error {
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "delete approval", err)
	}
	path, err := repository.approvalPath(projectID)
	if err != nil {
		return err
	}
	if err := verifyPrivateDirectory(repository.root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return model.Wrap(model.CodeInternal, "verify DSX state root", err)
	}
	directory := filepath.Dir(path)
	if err := verifyPrivateDirectory(directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return model.Wrap(model.CodeInternal, "verify approval directory", err)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return model.Wrap(model.CodeInternal, "inspect approval record for deletion", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return model.NewError(model.CodeInternal, "approval record for deletion is not a mode-0600 regular file", nil)
	}
	if err := os.Remove(path); err != nil {
		return model.Wrap(model.CodeInternal, "delete approval record", err)
	}
	if err := repository.syncDirectory(directory); err != nil {
		return model.Wrap(model.CodeInternal, "sync approval directory after deletion", err)
	}
	return nil
}

func (repository *ApprovalRepository) approvalPath(projectID model.ProjectID) (string, error) {
	parsed, err := model.ParseProjectID(string(projectID))
	if err != nil || parsed != projectID {
		return "", model.NewError(model.CodeInvalidInput, "invalid approval project ID", err)
	}
	directory := filepath.Join(repository.root, approvalDirectory)
	path := filepath.Join(directory, string(projectID)+".json")
	if filepath.Dir(path) != directory {
		return "", model.NewError(model.CodeInvalidInput, "approval path escapes state root", nil)
	}
	return path, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a real directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return nil
}

func verifyPrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a real directory", path)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s has mode %04o, want 0700", path, info.Mode().Perm())
	}
	return nil
}

func atomicReplace(directory, destination, projectPrefix string, data []byte, syncParent func(string) error) (result error) {
	temporary, err := os.CreateTemp(directory, "."+projectPrefix+".approval-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporary != nil {
			_ = temporary.Close()
		}
		if result != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	temporary = nil
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	return syncParent(directory)
}
func syncDirectory(path string) error {
	parent, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := parent.Sync()
	closeErr := parent.Close()
	return errors.Join(syncErr, closeErr)
}

func corruptApproval(err error) error {
	return model.NewError(model.CodeInternal, "approval record is corrupt", err)
}

func normalizeRecord(record state.ApprovalRecord) state.ApprovalRecord {
	// Authorization already normalizes interactive records; repository callers
	// get the same deterministic ordering and UTC timestamp directly.
	record.ApprovedAt = record.ApprovedAt.UTC()
	record.ImportedContentDigests = append([]state.ContentDigest(nil), record.ImportedContentDigests...)
	sort.Slice(record.ImportedContentDigests, func(i, j int) bool {
		if record.ImportedContentDigests[i].Path != record.ImportedContentDigests[j].Path {
			return record.ImportedContentDigests[i].Path < record.ImportedContentDigests[j].Path
		}
		return record.ImportedContentDigests[i].Digest < record.ImportedContentDigests[j].Digest
	})
	return record
}

func validateStrictJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		names := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("object member name is not a string")
			}
			if _, duplicate := names[name]; duplicate {
				return fmt.Errorf("duplicate object member %q", name)
			}
			names[name] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
