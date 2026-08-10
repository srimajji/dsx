package state

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/srimajji/dsx/internal/model"
)

const ApprovalRecordVersion = 1

type ApprovalMode string

const (
	ApprovalModeInteractive    ApprovalMode = "interactive"
	ApprovalModeNonInteractive ApprovalMode = "noninteractive"
)

// ApprovalRequest describes one executable-configuration authorization. Record
// contains the current plan hash and the metadata to persist after an explicit
// interactive final confirmation. ApprovedHash is comparison-only input from
// --approve-config and is never persisted by noninteractive authorization.
type ApprovalRequest struct {
	Mode           ApprovalMode
	Record         ApprovalRecord
	ApprovedHash   string
	FinalConfirmed bool
	Force          bool
}

// AuthorizeApproval fails closed unless the current executable hash has already
// been approved, is exactly supplied noninteractively, or is explicitly
// final-confirmed interactively. Force is intentionally not an authorization
// input and cannot change the result.
func AuthorizeApproval(ctx context.Context, repository ApprovalRepository, request ApprovalRequest) error {
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "authorize configuration", err)
	}
	if err := validateProjectAndHash(request.Record.ProjectID, request.Record.Hash); err != nil {
		return model.NewError(model.CodeUnapproved, "executable configuration has no valid current approval hash", err)
	}

	switch request.Mode {
	case ApprovalModeNonInteractive:
		if !equalHash(request.ApprovedHash, request.Record.Hash) {
			return model.NewError(model.CodeUnapproved, "--approve-config must exactly match the current executable configuration hash", nil)
		}
		return nil
	case ApprovalModeInteractive:
		if repository == nil {
			return model.NewError(model.CodeInternal, "interactive approval repository is not configured", nil)
		}
		existing, found, err := repository.LoadApproval(ctx, request.Record.ProjectID)
		if err != nil {
			return err
		}
		if found && equalHash(existing.Hash, request.Record.Hash) {
			return nil
		}
		if !request.FinalConfirmed {
			return model.NewError(model.CodeUnapproved, "executable configuration requires final interactive approval", nil)
		}
		if err := ValidateApprovalRecord(request.Record); err != nil {
			return model.NewError(model.CodeUnapproved, "cannot persist invalid executable configuration approval", err)
		}
		if err := repository.SaveApproval(ctx, normalizedApprovalRecord(request.Record)); err != nil {
			return err
		}
		return nil
	default:
		return model.NewError(model.CodeInvalidInput, fmt.Sprintf("unknown approval mode %q", request.Mode), nil)
	}
}

// ValidateApprovalRecord enforces the on-disk approval record contract.
func ValidateApprovalRecord(record ApprovalRecord) error {
	if record.Version != ApprovalRecordVersion {
		return fmt.Errorf("approval record version is %d, want %d", record.Version, ApprovalRecordVersion)
	}
	if err := validateProjectAndHash(record.ProjectID, record.Hash); err != nil {
		return err
	}
	if record.ApprovedAt.IsZero() {
		return fmt.Errorf("approval timestamp is missing")
	}
	if strings.TrimSpace(record.DSXVersion) == "" {
		return fmt.Errorf("DSX version is missing")
	}
	if !validSHA256(record.ConfigContentDigest) {
		return fmt.Errorf("configuration content digest is not a lowercase SHA-256 digest")
	}
	seenPaths := make(map[string]struct{}, len(record.ImportedContentDigests))
	for _, imported := range record.ImportedContentDigests {
		if imported.Path == "" {
			return fmt.Errorf("imported content path is missing")
		}
		if _, exists := seenPaths[imported.Path]; exists {
			return fmt.Errorf("duplicate imported content path %q", imported.Path)
		}
		seenPaths[imported.Path] = struct{}{}
		if !validSHA256(imported.Digest) {
			return fmt.Errorf("imported content digest for %q is not a lowercase SHA-256 digest", imported.Path)
		}
	}
	return nil
}

func validateProjectAndHash(projectID model.ProjectID, hash string) error {
	parsed, err := model.ParseProjectID(string(projectID))
	if err != nil || parsed != projectID {
		if err == nil {
			err = fmt.Errorf("project ID is not canonical")
		}
		return err
	}
	if !validSHA256(hash) {
		return fmt.Errorf("executable hash is not a lowercase SHA-256 digest")
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func equalHash(left, right string) bool {
	if !validSHA256(left) || !validSHA256(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func normalizedApprovalRecord(record ApprovalRecord) ApprovalRecord {
	record.ApprovedAt = record.ApprovedAt.UTC()
	record.ImportedContentDigests = append([]ContentDigest(nil), record.ImportedContentDigests...)
	sort.Slice(record.ImportedContentDigests, func(i, j int) bool {
		if record.ImportedContentDigests[i].Path != record.ImportedContentDigests[j].Path {
			return record.ImportedContentDigests[i].Path < record.ImportedContentDigests[j].Path
		}
		return record.ImportedContentDigests[i].Digest < record.ImportedContentDigests[j].Digest
	})
	return record
}
