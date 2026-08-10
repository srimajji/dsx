package app

import (
	"context"
	"io"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

type CloneRunRequest struct {
	Root           string
	ApproveConfig  string
	Sandbox        string
	Agent          string
	Profile        string
	Prompt         string
	Browser        bool
	MCPServers     []harness.MCPServer
	Environment    map[string]string
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
	RunInteractive InteractiveChildRunner
}

type cloneWorkspaceAccess struct {
	RestartStopped bool
}

type CloneRunResult struct {
	ProjectID    model.ProjectID    `json:"project_id"`
	Sandbox      model.SandboxName  `json:"sandbox"`
	RunID        model.RunID        `json:"run_id"`
	State        model.SandboxState `json:"state"`
	Agent        harness.Name       `json:"agent"`
	Exit         runtime.Exit       `json:"exit"`
	Repositories []gitx.Status      `json:"repositories"`
	URLs         []string           `json:"urls,omitempty"`
	Warnings     []string           `json:"warnings,omitempty"`
}

type GitStatusRequest struct {
	Root       string
	Sandbox    string
	Repository string
}

type GitStatusResult struct {
	ProjectID    model.ProjectID   `json:"project_id"`
	Sandbox      model.SandboxName `json:"sandbox"`
	Repositories []gitx.Status     `json:"repositories"`
}

type GitDiffRequest struct {
	Root       string
	Sandbox    string
	MaxBytes   int
	Repository string
}

type RepositoryDiff struct {
	Repository string `json:"repository"`
	Patch      []byte `json:"patch"`
	Truncated  bool   `json:"truncated"`
}

type GitDiffResult struct {
	ProjectID model.ProjectID   `json:"project_id"`
	Sandbox   model.SandboxName `json:"sandbox"`
	Diffs     []RepositoryDiff  `json:"diffs"`
}

type GitFetchRequest struct {
	Root       string
	Sandbox    string
	Repository string
}

type GitFetchResult struct {
	ProjectID    model.ProjectID    `json:"project_id"`
	Sandbox      model.SandboxName  `json:"sandbox"`
	Repositories []gitx.FetchResult `json:"repositories"`
}

type GitApplyRequest struct {
	Root       string
	Sandbox    string
	Repository string
}

type GitApplyResult struct {
	ProjectID    model.ProjectID    `json:"project_id"`
	Sandbox      model.SandboxName  `json:"sandbox"`
	Repositories []gitx.ApplyResult `json:"repositories"`
}

type CloneManager interface {
	RunClone(context.Context, CloneRunRequest) (CloneRunResult, error)
	GitStatus(context.Context, GitStatusRequest) (GitStatusResult, error)
	GitDiff(context.Context, GitDiffRequest) (GitDiffResult, error)
	GitFetch(context.Context, GitFetchRequest) (GitFetchResult, error)
	GitApply(context.Context, GitApplyRequest) (GitApplyResult, error)
}
