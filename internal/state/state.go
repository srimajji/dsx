package state

import (
	"context"
	"time"

	"github.com/srimajji/dsx/internal/model"
)

type ApprovalRecord struct {
	Version                int             `json:"version"`
	ProjectID              model.ProjectID `json:"project_id"`
	Hash                   string          `json:"hash"`
	ApprovedAt             time.Time       `json:"approved_at"`
	DSXVersion             string          `json:"dsx_version"`
	ConfigContentDigest    string          `json:"config_content_digest"`
	ImportedContentDigests []ContentDigest `json:"imported_content_digests"`
}

type ContentDigest struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type ApprovalRepository interface {
	LoadApproval(context.Context, model.ProjectID) (ApprovalRecord, bool, error)
	SaveApproval(context.Context, ApprovalRecord) error
	DeleteApproval(context.Context, model.ProjectID) error
}

type ProjectLock interface {
	Unlock() error
}

// LockRepository provides bounded, context-cancelable process leases.
// Sandbox leases are scoped to the exact project and sandbox pair.
type LockRepository interface {
	LockProject(context.Context, model.ProjectID) (ProjectLock, error)
	LockSandbox(context.Context, model.ProjectID, model.SandboxName) (ProjectLock, error)
}
