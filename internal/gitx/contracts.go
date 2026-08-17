package gitx

import (
	"context"
	"io"
)

const (
	SourceBundleMode            = 0o600
	ResultBundleMode            = 0o600
	RefNamespace                = "refs/remotes/dsx/"
	MaxSourceBundleBytes  int64 = 512 << 20
	MaxSnapshotInputBytes       = MaxSourceBundleBytes
	MaxResultBundleBytes        = MaxSourceBundleBytes
)

type PathComponentIdentity struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type PhysicalPathIdentity struct {
	CanonicalPath string                  `json:"canonical_path"`
	Components    []PathComponentIdentity `json:"components"`
}

type RepositoryIdentity struct {
	ApprovedRoot PhysicalPathIdentity `json:"approved_root"`
	Worktree     PhysicalPathIdentity `json:"worktree"`
	GitDir       PhysicalPathIdentity `json:"git_dir"`
}

type Repository struct {
	Name      string             `json:"name"`
	HostPath  string             `json:"host_path"`
	GuestPath string             `json:"guest_path"`
	Identity  RepositoryIdentity `json:"identity"`
}

type SourceRequest struct {
	Repository   Repository
	ApprovedRoot string
	Workspace    string
	TempRoot     string
	Snapshot     bool
}

type UpdateSourceRequest struct {
	Repository         Repository
	Workspace          string
	TempRoot           string
	SourceBranch       string
	SourceRevision     string
	SourceHeadRevision string
	SourceTree         string
	Snapshot           bool
}

type SourceArtifact struct {
	Repository         Repository `json:"repository"`
	SourceBranch       string     `json:"source_branch"`
	SourceRevision     string     `json:"source_revision"`
	SourceSnapshot     bool       `json:"source_snapshot"`
	SourceHeadRevision string     `json:"source_head_revision"`
	SourceTree         string     `json:"source_tree"`
	TrackedFingerprint string     `json:"tracked_fingerprint"`
	WarnUntracked      bool       `json:"warn_untracked"`
	WarnIgnored        bool       `json:"warn_ignored"`
	BundlePath         string     `json:"bundle_path"`
	BundleDigest       string     `json:"bundle_digest"`
	BundleRef          string     `json:"bundle_ref"`
}

type ResultArtifact struct {
	Repository      Repository `json:"repository"`
	WorkspaceBranch string     `json:"workspace_branch"`
	ResultCommit    string     `json:"result_commit"`
	BundlePath      string     `json:"bundle_path"`
	BundleDigest    string     `json:"bundle_digest"`
}

type FetchRequest struct {
	Repository     Repository
	Workspace      string
	BundlePath     string
	Digest         string
	ExpectedCommit string
}

type FetchResult struct {
	Repository string `json:"repository"`
	HostRef    string `json:"host_ref"`
	Commit     string `json:"commit"`
}

type StatusRequest struct {
	Repository         Repository
	Workspace          string
	SourceBranch       string
	SourceRevision     string
	SourceSnapshot     bool
	WorkspaceBranch    string
	ResultCommit       string
	TrackedFingerprint string
	FetchedCommit      string
}

type Status struct {
	Repository             string `json:"repository"`
	Workspace              string `json:"workspace"`
	SourceBranch           string `json:"source_branch"`
	SourceRevision         string `json:"source_revision"`
	SourceSnapshot         bool   `json:"source_snapshot"`
	WorkspaceBranch        string `json:"workspace_branch"`
	ResultCommit           string `json:"result_commit,omitempty"`
	HostCommit             string `json:"host_commit"`
	HostTrackedFingerprint string `json:"host_tracked_fingerprint"`
	HostTrackedClean       bool   `json:"host_tracked_clean"`
	WorkspaceTrackedClean  bool   `json:"workspace_tracked_clean"`
	WorkspaceUntracked     bool   `json:"workspace_untracked"`
	RebaseInProgress       bool   `json:"rebase_in_progress"`
	WarnUntracked          bool   `json:"warn_untracked"`
	WarnIgnored            bool   `json:"warn_ignored"`
	Fetched                bool   `json:"fetched"`
	FetchedCommit          string `json:"fetched_commit,omitempty"`
}

type DiffBundle struct {
	Path   string
	Digest string
	Ref    string
}

type DiffRequest struct {
	Repository Repository
	BaseCommit string
	HeadCommit string
	MaxBytes   int
	Bundle     *DiffBundle
}

type DiffResult struct {
	Patch     []byte `json:"patch"`
	Truncated bool   `json:"truncated"`
}

type ApplyRequest struct {
	Repository         Repository
	SourceRevision     string
	SourceSnapshot     bool
	SourceHeadRevision string
	SourceTree         string
	TrackedFingerprint string
	FetchedRef         string
	ExpectedCommit     string
	TempRoot           string
}

type ApplyResult struct {
	Repository    string   `json:"repository"`
	AppliedCommit string   `json:"applied_commit"`
	Paths         []string `json:"paths"`
}

// ApplyTransaction is a prepared host mutation. PrepareApply validates every
// precondition and captures bounded rollback state. Commit revalidates at the
// mutation boundary. Rollback restores a mutation attempted by Commit.
type ApplyTransaction interface {
	Commit(context.Context) (ApplyResult, error)
	Rollback(context.Context) error
}

// HostService owns host-side Git inspection, bundle verification, fetch, diff,
// and transactional apply. Implementations execute git directly without a
// shell and never persist transfer bundles.
type HostService interface {
	ValidateRepository(context.Context, Repository) error
	PrepareSource(context.Context, SourceRequest) (SourceArtifact, error)
	PrepareUpdateSource(context.Context, UpdateSourceRequest) (SourceArtifact, error)
	VerifyBundle(context.Context, string, string) error
	FetchResult(context.Context, FetchRequest) (FetchResult, error)
	Status(context.Context, StatusRequest) (Status, error)
	Diff(context.Context, DiffRequest) (DiffResult, error)
	PrepareApply(context.Context, ApplyRequest) (ApplyTransaction, error)
	RemoveArtifact(string) error
}

type Command struct {
	Argv   []string
	Dir    string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// StdoutMaxBytes makes stdout a fail-closed producer stream. A process
	// attempting to write beyond the cap is terminated by the runner.
	StdoutMaxBytes int64
}

type Exit struct {
	Code   int
	Signal string
}

// Runner is the no-shell subprocess seam used by host Git operations.
// Implementations must fail and terminate production when StdoutMaxBytes is set
// and stdout would cross that cap.
type Runner interface {
	Run(context.Context, Command) (Exit, error)
}
