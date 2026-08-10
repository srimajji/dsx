package state_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/state"
)

type approvalMemoryRepository struct {
	record    state.ApprovalRecord
	found     bool
	loadErr   error
	saveErr   error
	loadCalls int
	saveCalls int
}

func (repository *approvalMemoryRepository) LoadApproval(context.Context, model.ProjectID) (state.ApprovalRecord, bool, error) {
	repository.loadCalls++
	return repository.record, repository.found, repository.loadErr
}

func (repository *approvalMemoryRepository) SaveApproval(_ context.Context, record state.ApprovalRecord) error {
	repository.saveCalls++
	repository.record = record
	repository.found = true
	return repository.saveErr
}
func (repository *approvalMemoryRepository) DeleteApproval(context.Context, model.ProjectID) error {
	repository.record = state.ApprovalRecord{}
	repository.found = false
	return nil
}

func TestApprovalNoninteractiveExactComparisonNeverPersists(t *testing.T) {
	record := validApprovalRecord()
	repository := &approvalMemoryRepository{loadErr: errors.New("must not load")}
	err := state.AuthorizeApproval(context.Background(), repository, state.ApprovalRequest{
		Mode: state.ApprovalModeNonInteractive, Record: record, ApprovedHash: record.Hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if repository.loadCalls != 0 || repository.saveCalls != 0 {
		t.Fatalf("noninteractive exact authorization touched repository: loads=%d saves=%d", repository.loadCalls, repository.saveCalls)
	}
}

func TestApprovalNoninteractiveRejectsWrongMissingStaleAndForceOnly(t *testing.T) {
	record := validApprovalRecord()
	for name, approvedHash := range map[string]string{
		"missing": "",
		"wrong":   hashOf("b"),
		"stale":   hashOf("c"),
	} {
		t.Run(name, func(t *testing.T) {
			repository := &approvalMemoryRepository{}
			err := state.AuthorizeApproval(context.Background(), repository, state.ApprovalRequest{
				Mode: state.ApprovalModeNonInteractive, Record: record, ApprovedHash: approvedHash, Force: true,
			})
			if model.ErrorCodeOf(err) != model.CodeUnapproved {
				t.Fatalf("error = %v (code %q), want unapproved", err, model.ErrorCodeOf(err))
			}
			if repository.loadCalls != 0 || repository.saveCalls != 0 {
				t.Fatalf("rejected noninteractive request touched repository: loads=%d saves=%d", repository.loadCalls, repository.saveCalls)
			}
		})
	}
}

func TestApprovalInteractiveUsesExactExistingRecord(t *testing.T) {
	record := validApprovalRecord()
	repository := &approvalMemoryRepository{record: record, found: true}
	if err := state.AuthorizeApproval(context.Background(), repository, state.ApprovalRequest{
		Mode: state.ApprovalModeInteractive, Record: record,
	}); err != nil {
		t.Fatal(err)
	}
	if repository.loadCalls != 1 || repository.saveCalls != 0 {
		t.Fatalf("existing authorization calls: loads=%d saves=%d", repository.loadCalls, repository.saveCalls)
	}
}

func TestApprovalInteractiveUnconfirmedNeverPersists(t *testing.T) {
	current := validApprovalRecord()
	for name, repository := range map[string]*approvalMemoryRepository{
		"missing": {},
		"stale":   {record: approvalRecordWithHash(hashOf("b")), found: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := state.AuthorizeApproval(context.Background(), repository, state.ApprovalRequest{
				Mode: state.ApprovalModeInteractive, Record: current, Force: true,
			})
			if model.ErrorCodeOf(err) != model.CodeUnapproved {
				t.Fatalf("error = %v (code %q), want unapproved", err, model.ErrorCodeOf(err))
			}
			if repository.saveCalls != 0 {
				t.Fatalf("unconfirmed interactive request saved %d records", repository.saveCalls)
			}
		})
	}
}

func TestApprovalInteractiveFinalConfirmationPersistsOneValidRecord(t *testing.T) {
	record := validApprovalRecord()
	record.ApprovedAt = time.Date(2026, 8, 9, 3, 4, 5, 0, time.FixedZone("offset", 2*60*60))
	record.ImportedContentDigests = []state.ContentDigest{
		{Path: "z.json", Digest: hashOf("f")},
		{Path: "a.json", Digest: hashOf("e")},
	}
	repository := &approvalMemoryRepository{record: approvalRecordWithHash(hashOf("b")), found: true}
	if err := state.AuthorizeApproval(context.Background(), repository, state.ApprovalRequest{
		Mode: state.ApprovalModeInteractive, Record: record, FinalConfirmed: true,
	}); err != nil {
		t.Fatal(err)
	}
	if repository.loadCalls != 1 || repository.saveCalls != 1 {
		t.Fatalf("final confirmation calls: loads=%d saves=%d", repository.loadCalls, repository.saveCalls)
	}
	if err := state.ValidateApprovalRecord(repository.record); err != nil {
		t.Fatalf("persisted invalid record: %v", err)
	}
	if repository.record.ApprovedAt.Location() != time.UTC {
		t.Fatalf("persisted timestamp location = %v, want UTC", repository.record.ApprovedAt.Location())
	}
	if repository.record.ImportedContentDigests[0].Path != "a.json" {
		t.Fatalf("imported digests not normalized: %#v", repository.record.ImportedContentDigests)
	}
}

func TestApprovalInteractiveRejectsInvalidFinalRecordWithoutPersistence(t *testing.T) {
	record := validApprovalRecord()
	record.ConfigContentDigest = ""
	repository := &approvalMemoryRepository{}
	err := state.AuthorizeApproval(context.Background(), repository, state.ApprovalRequest{
		Mode: state.ApprovalModeInteractive, Record: record, FinalConfirmed: true,
	})
	if model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("error = %v (code %q), want unapproved", err, model.ErrorCodeOf(err))
	}
	if repository.saveCalls != 0 {
		t.Fatalf("invalid final record saved %d times", repository.saveCalls)
	}
}

func validApprovalRecord() state.ApprovalRecord {
	return state.ApprovalRecord{
		Version:                state.ApprovalRecordVersion,
		ProjectID:              model.ProjectID("aaaaaaaaaaaaaaaaaaaa"),
		Hash:                   hashOf("a"),
		ApprovedAt:             time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC),
		DSXVersion:             "1.0.0",
		ConfigContentDigest:    hashOf("d"),
		ImportedContentDigests: []state.ContentDigest{},
	}
}

func approvalRecordWithHash(hash string) state.ApprovalRecord {
	record := validApprovalRecord()
	record.Hash = hash
	return record
}

func hashOf(character string) string {
	value := ""
	for len(value) < 64 {
		value += character
	}
	return value[:64]
}
