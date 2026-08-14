package app

import (
	"context"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
)

type WorkspaceUpdateRequest struct {
	Root      string
	Workspace model.WorkspaceName
	Snapshot  bool
}

type GitStatusRequest struct {
	Root       string
	Workspace  model.WorkspaceName
	Repository string
}

type GitStatusResult struct {
	ProjectID    model.ProjectID     `json:"project_id"`
	Workspace    model.WorkspaceName `json:"workspace"`
	Repositories []gitx.Status       `json:"repositories"`
}

type GitDiffRequest struct {
	Root       string
	Workspace  model.WorkspaceName
	MaxBytes   int
	Repository string
}

type RepositoryDiff struct {
	Repository string `json:"repository"`
	Patch      []byte `json:"patch"`
	Truncated  bool   `json:"truncated"`
}

type GitDiffResult struct {
	ProjectID model.ProjectID     `json:"project_id"`
	Workspace model.WorkspaceName `json:"workspace"`
	Diffs     []RepositoryDiff    `json:"diffs"`
}

type GitFetchRequest struct {
	Root       string
	Workspace  model.WorkspaceName
	Repository string
}

type GitFetchResult struct {
	ProjectID    model.ProjectID     `json:"project_id"`
	Workspace    model.WorkspaceName `json:"workspace"`
	Repositories []gitx.FetchResult  `json:"repositories"`
}

type GitApplyRequest struct {
	Root       string
	Workspace  model.WorkspaceName
	Repository string
}

type GitApplyResult struct {
	ProjectID    model.ProjectID     `json:"project_id"`
	Workspace    model.WorkspaceName `json:"workspace"`
	Repositories []gitx.ApplyResult  `json:"repositories"`
}

type WorkspaceGitManager interface {
	Update(context.Context, WorkspaceUpdateRequest) (WorkspaceResult, error)
	GitStatus(context.Context, GitStatusRequest) (GitStatusResult, error)
	GitDiff(context.Context, GitDiffRequest) (GitDiffResult, error)
	GitFetch(context.Context, GitFetchRequest) (GitFetchResult, error)
	GitApply(context.Context, GitApplyRequest) (GitApplyResult, error)
}
