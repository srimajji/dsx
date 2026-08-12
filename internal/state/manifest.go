package state

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

const ManifestVersion = 2

const (
	LegacyManifestVersion   = 1
	OwnershipManagedLabel   = "dev.dsx.managed"
	OwnershipContractLabel  = "dev.dsx.contract"
	OwnershipProjectLabel   = "dev.dsx.project"
	OwnershipWorkspaceLabel = "dev.dsx.workspace"
	OwnershipRunLabel       = "dev.dsx.run"
	OwnershipKindLabel      = "dev.dsx.kind"
	OwnershipRoleLabel      = "dev.dsx.role"
	OwnershipContractValue  = "dsx.ownership/v2"
	LegacyContractValue     = "dsx.ownership/v1"
	LegacySandboxLabel      = "dev.dsx.sandbox"
)

func CanonicalResourceName(canonicalRoot string, workspace model.WorkspaceName, role string) (string, error) {
	return runtime.CanonicalResourceName(canonicalRoot, workspace, role)
}

func ResourceOwnershipLabels(projectID model.ProjectID, workspace model.WorkspaceName, runID model.RunID, kind, role string) []OwnershipLabel {
	return []OwnershipLabel{
		{Key: OwnershipManagedLabel, Value: "true"},
		{Key: OwnershipContractLabel, Value: OwnershipContractValue},
		{Key: OwnershipProjectLabel, Value: string(projectID)},
		{Key: OwnershipWorkspaceLabel, Value: string(workspace)},
		{Key: OwnershipRunLabel, Value: string(runID)},
		{Key: OwnershipKindLabel, Value: kind},
		{Key: OwnershipRoleLabel, Value: role},
	}
}

func LegacyResourceOwnershipLabels(projectID model.ProjectID, workspace model.WorkspaceName, runID model.RunID, kind, role string) []OwnershipLabel {
	return []OwnershipLabel{
		{Key: OwnershipManagedLabel, Value: "true"},
		{Key: OwnershipContractLabel, Value: LegacyContractValue},
		{Key: OwnershipProjectLabel, Value: string(projectID)},
		{Key: LegacySandboxLabel, Value: string(workspace)},
		{Key: OwnershipRunLabel, Value: string(runID)},
		{Key: OwnershipKindLabel, Value: kind},
		{Key: OwnershipRoleLabel, Value: role},
	}
}

type Manifest struct {
	Version        int                  `json:"version"`
	Generation     uint64               `json:"generation"`
	ProjectID      model.ProjectID      `json:"project_id"`
	CanonicalRoot  string               `json:"canonical_root"`
	Workspace      model.WorkspaceName  `json:"workspace"`
	RunID          model.RunID          `json:"run_id"`
	PlanHash       string               `json:"plan_hash"`
	DefaultAgent   string               `json:"default_agent,omitempty"`
	AWSGrant       *AWSGrantRecord      `json:"aws_grant,omitempty"`
	State          model.WorkspaceState `json:"state"`
	Operation      string               `json:"operation"`
	Resources      []ResourceRecord     `json:"resources"`
	HostBindings   []HostBindingRecord  `json:"host_bindings,omitempty"`
	Git            []GitRecord          `json:"git"`
	UncapturedWork bool                 `json:"uncaptured_work,omitempty"`
	ActiveSession  *SessionRecord       `json:"active_session,omitempty"`
	Failure        string               `json:"failure,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
	Legacy         bool                 `json:"-"`
}

type AWSGrantRecord struct {
	Enabled bool `json:"enabled"`
}

type SessionRecord struct {
	SessionID       model.RunID `json:"session_id"`
	Kind            string      `json:"kind"`
	Agent           string      `json:"agent,omitempty"`
	BrowserResource string      `json:"browser_resource,omitempty"`
	StartedAt       time.Time   `json:"started_at"`
}

// ValidateManifest enforces the complete durable manifest contract. Callers
// may rely on a successfully validated manifest as ownership evidence, so the
// duplicated identity in each resource must agree with the manifest identity.
func ValidateManifest(manifest Manifest) error {
	if manifest.Version != ManifestVersion {
		return fmt.Errorf("manifest version is %d, want %d", manifest.Version, ManifestVersion)
	}
	if manifest.Generation == 0 {
		return fmt.Errorf("manifest generation must be positive")
	}
	projectID, err := model.ParseProjectID(string(manifest.ProjectID))
	if err != nil || projectID != manifest.ProjectID {
		return fmt.Errorf("manifest project ID: %w", err)
	}
	if manifest.CanonicalRoot == "" || !filepath.IsAbs(manifest.CanonicalRoot) || filepath.Clean(manifest.CanonicalRoot) != manifest.CanonicalRoot {
		return fmt.Errorf("manifest canonical root must be a clean absolute path")
	}
	derivedProjectID, err := model.NewProjectID(manifest.CanonicalRoot)
	if err != nil || derivedProjectID != manifest.ProjectID {
		return fmt.Errorf("manifest canonical root does not match project ID")
	}
	workspace, err := model.ParseWorkspaceName(string(manifest.Workspace))
	if err != nil || workspace != manifest.Workspace {
		return fmt.Errorf("manifest workspace: %w", err)
	}
	if manifest.Legacy {
		return fmt.Errorf("legacy manifest is cleanup-only")
	}
	runID, err := model.ParseRunID(string(manifest.RunID))
	if err != nil || runID != manifest.RunID {
		return fmt.Errorf("manifest run ID: %w", err)
	}
	if len(manifest.PlanHash) != 64 {
		return fmt.Errorf("manifest plan hash must be a lowercase SHA-256 digest")
	}
	if _, err := hex.DecodeString(manifest.PlanHash); err != nil {
		return fmt.Errorf("manifest plan hash must be a lowercase SHA-256 digest: %w", err)
	}
	for _, character := range manifest.PlanHash {
		if character >= 'A' && character <= 'F' {
			return fmt.Errorf("manifest plan hash must be a lowercase SHA-256 digest")
		}
	}
	if !manifest.State.Valid() {
		return fmt.Errorf("invalid manifest state %q", manifest.State)
	}
	switch manifest.Operation {
	case "", "create", "open", "start", "stop", "restart", "update", "remove", "capture", "aws-enable", "aws-disable":
	default:
		return fmt.Errorf("invalid manifest operation %q", manifest.Operation)
	}
	switch manifest.Operation {
	case "aws-enable":
		if manifest.AWSGrant == nil || !manifest.AWSGrant.Enabled {
			return fmt.Errorf("aws-enable operation requires an enabled AWS grant")
		}
	case "aws-disable":
		if manifest.AWSGrant == nil || manifest.AWSGrant.Enabled {
			return fmt.Errorf("aws-disable operation requires a disabled AWS grant")
		}
	}
	if manifest.UncapturedWork && manifest.State == model.StateDeleted {
		return fmt.Errorf("deleted workspace manifest has uncaptured work")
	}
	if err := validateManifestTime("created_at", manifest.CreatedAt); err != nil {
		return err
	}
	if err := validateManifestTime("updated_at", manifest.UpdatedAt); err != nil {
		return err
	}
	if manifest.ActiveSession != nil {
		sessionID, err := model.ParseRunID(string(manifest.ActiveSession.SessionID))
		if err != nil || sessionID != manifest.ActiveSession.SessionID {
			return fmt.Errorf("manifest active session ID: %w", err)
		}
		switch manifest.ActiveSession.Kind {
		case "agent":
			if !validAgentName(manifest.ActiveSession.Agent) {
				return fmt.Errorf("manifest active agent name is invalid")
			}
		case "open":
			if manifest.ActiveSession.Agent != "" || manifest.ActiveSession.BrowserResource != "" {
				return fmt.Errorf("manifest open session contains agent fields")
			}
		default:
			return fmt.Errorf("manifest active session kind is invalid")
		}
		if err := validateManifestTime("active_session.started_at", manifest.ActiveSession.StartedAt); err != nil {
			return err
		}
		if manifest.ActiveSession.BrowserResource != "" {
			found := false
			for _, resource := range manifest.Resources {
				if resource.Kind == string(runtime.ResourceBrowser) && resource.ExpectedID == manifest.ActiveSession.BrowserResource {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("manifest active session browser resource is missing")
			}
		}
	}
	if manifest.UpdatedAt.Before(manifest.CreatedAt) {
		return fmt.Errorf("manifest updated_at precedes created_at")
	}
	if len(manifest.Resources) > 4096 {
		return fmt.Errorf("manifest contains too many resources")
	}
	resourceNames := make(map[string]struct{}, len(manifest.Resources))
	for index, resource := range manifest.Resources {
		if err := validateResourceRecord(manifest, resource); err != nil {
			return fmt.Errorf("manifest resource %d: %w", index, err)
		}
		if _, duplicate := resourceNames[resource.Name]; duplicate {
			return fmt.Errorf("manifest resource %d duplicates resource name %q", index, resource.Name)
		}
		resourceNames[resource.Name] = struct{}{}
	}
	if err := validateHostBindings(manifest.HostBindings); err != nil {
		return err
	}
	if err := validateGitRecords(manifest); err != nil {
		return err
	}
	if manifest.State == model.StateDeleted {
		for index, resource := range manifest.Resources {
			if !resource.Deleted && !resource.Absent {
				return fmt.Errorf("deleted manifest resource %d has no inspected terminal postcondition", index)
			}
		}
		for index, record := range manifest.Git {
			if record.HasResultWork() && !record.ResultFetched() {
				return fmt.Errorf("deleted workspace manifest git record %d has unfetched result work", index)
			}
		}
	}
	return nil
}

func validateManifestTime(field string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("manifest %s must be set", field)
	}
	_, offset := value.Zone()
	if offset != 0 {
		return fmt.Errorf("manifest %s must be UTC", field)
	}
	return nil
}

func validateResourceRecord(manifest Manifest, resource ResourceRecord) error {
	switch resource.Kind {
	case "workspace", "browser", "network", "volume":
	default:
		return fmt.Errorf("unsupported kind %q", resource.Kind)
	}
	if !validResourceRole(resource.Role) {
		return fmt.Errorf("invalid role %q", resource.Role)
	}
	expectedName, err := CanonicalResourceName(manifest.CanonicalRoot, manifest.Workspace, resource.Role)
	if err != nil {
		return fmt.Errorf("canonical resource name: %w", err)
	}
	if resource.Name != expectedName || len(resource.Name) > 62 {
		return fmt.Errorf("name %q is not the canonical ownership name", resource.Name)
	}
	if resource.ExpectedID == "" || resource.ExpectedID != resource.Name {
		return fmt.Errorf("expected runtime ID must equal the canonical resource name")
	}
	if resource.Created && resource.RuntimeID == "" {
		return fmt.Errorf("created resource has no runtime ID")
	}
	if resource.Created && resource.RuntimeID != resource.ExpectedID {
		return fmt.Errorf("created resource runtime ID does not match its write-ahead identity")
	}
	if !resource.Created && resource.RuntimeID != "" {
		return fmt.Errorf("uncreated resource has a runtime ID")
	}
	if resource.Deleted && !resource.Created {
		return fmt.Errorf("deleted resource was never created")
	}
	if resource.Absent && (resource.Created || resource.Deleted || resource.RuntimeID != "") {
		return fmt.Errorf("proven-absent resource contains created runtime state")
	}
	expectedLabels := ResourceOwnershipLabels(manifest.ProjectID, manifest.Workspace, manifest.RunID, resource.Kind, resource.Role)
	if len(resource.Labels) != len(expectedLabels) {
		return fmt.Errorf("must contain exactly %d ownership labels", len(expectedLabels))
	}
	for index, expected := range expectedLabels {
		if resource.Labels[index] != expected {
			return fmt.Errorf("ownership label %d is {%q %q}, want {%q %q}", index, resource.Labels[index].Key, resource.Labels[index].Value, expected.Key, expected.Value)
		}
	}
	return nil
}

func validResourceRole(value string) bool {
	if len(value) == 0 || len(value) > 9 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

type HostBindingRecord struct {
	Name      string     `json:"name"`
	HostIP    netip.Addr `json:"host_ip"`
	HostPort  uint16     `json:"host_port"`
	GuestPort uint16     `json:"guest_port"`
	Protocol  string     `json:"protocol"`
}

func validateHostBindings(bindings []HostBindingRecord) error {
	if len(bindings) > 128 {
		return fmt.Errorf("manifest contains too many host bindings")
	}
	names := make(map[string]struct{}, len(bindings))
	listeners := make(map[string]struct{}, len(bindings))
	for index, binding := range bindings {
		if !validHostBindingName(binding.Name) {
			return fmt.Errorf("manifest host binding %d has invalid name", index)
		}
		if _, duplicate := names[binding.Name]; duplicate {
			return fmt.Errorf("manifest host binding %d duplicates name %q", index, binding.Name)
		}
		names[binding.Name] = struct{}{}
		if !binding.HostIP.IsValid() || binding.HostIP.Zone() != "" || binding.HostIP.IsMulticast() || binding.HostIP.Is4In6() || binding.HostPort == 0 || binding.GuestPort == 0 || binding.Protocol != "tcp" {
			return fmt.Errorf("manifest host binding %d is invalid", index)
		}
		key := binding.HostIP.String() + ":" + fmt.Sprint(binding.HostPort)
		if _, duplicate := listeners[key]; duplicate {
			return fmt.Errorf("manifest host binding %d duplicates listener %s", index, key)
		}
		listeners[key] = struct{}{}
	}
	return nil
}

func validHostBindingName(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func validAgentName(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
			continue
		}
		return false
	}
	return true
}

type OwnershipLabel struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type ResourceRecord struct {
	Kind       string           `json:"kind"`
	Role       string           `json:"role"`
	Name       string           `json:"name"`
	ExpectedID string           `json:"expected_id"`
	RuntimeID  string           `json:"runtime_id,omitempty"`
	Labels     []OwnershipLabel `json:"labels"`
	Created    bool             `json:"created"`
	Persistent bool             `json:"persistent,omitempty"`
	Deleted    bool             `json:"deleted,omitempty"`
	Absent     bool             `json:"absent,omitempty"`
}

type GitRecord struct {
	Repository             string                  `json:"repository"`
	HostPath               string                  `json:"host_path"`
	GuestPath              string                  `json:"guest_path"`
	Identity               gitx.RepositoryIdentity `json:"identity"`
	SourceBranch           string                  `json:"source_branch"`
	SourceRevision         string                  `json:"source_revision"`
	TrackedFingerprint     string                  `json:"tracked_fingerprint"`
	WarnUntracked          bool                    `json:"warn_untracked,omitempty"`
	WarnIgnored            bool                    `json:"warn_ignored,omitempty"`
	WorkspaceBranch        string                  `json:"workspace_branch"`
	ResultCommit           string                  `json:"result_commit,omitempty"`
	SourceBundleDigest     string                  `json:"source_bundle_digest"`
	ResultBundleDigest     string                  `json:"result_bundle_digest,omitempty"`
	FetchedCommit          string                  `json:"fetched_commit,omitempty"`
	FetchedHostRef         string                  `json:"fetched_host_ref,omitempty"`
	BackupRef              string                  `json:"backup_ref,omitempty"`
	Conflict               bool                    `json:"conflict,omitempty"`
	ConflictSourceRevision string                  `json:"conflict_source_revision,omitempty"`
	ConflictBundleDigest   string                  `json:"conflict_bundle_digest,omitempty"`
}

// HasResultWork reports whether a clone repository has a durable result that
// cleanup must account for.
func (record GitRecord) HasResultWork() bool {
	return record.ResultCommit != ""
}

// ResultFetched reports whether the current durable result commit was fetched.
// A repository with no result work is intentionally not reported as fetched.
func (record GitRecord) ResultFetched() bool {
	return record.HasResultWork() && record.FetchedCommit == record.ResultCommit
}

func validateGitRecords(manifest Manifest) error {
	if len(manifest.Git) == 0 {
		return fmt.Errorf("workspace manifest must contain at least one git record")
	}
	if len(manifest.Git) > 4096 {
		return fmt.Errorf("manifest contains too many git records")
	}
	repositories := make(map[string]struct{}, len(manifest.Git))
	hostPaths := make(map[string]struct{}, len(manifest.Git))
	guestPaths := make(map[string]struct{}, len(manifest.Git))
	for index, record := range manifest.Git {
		if err := validateGitRecord(manifest, record); err != nil {
			return fmt.Errorf("manifest git record %d: %w", index, err)
		}
		if _, duplicate := repositories[record.Repository]; duplicate {
			return fmt.Errorf("manifest git record %d duplicates repository %q", index, record.Repository)
		}
		if _, duplicate := hostPaths[record.HostPath]; duplicate {
			return fmt.Errorf("manifest git record %d duplicates host path", index)
		}
		if _, duplicate := guestPaths[record.GuestPath]; duplicate {
			return fmt.Errorf("manifest git record %d duplicates guest path", index)
		}
		repositories[record.Repository] = struct{}{}
		hostPaths[record.HostPath] = struct{}{}
		guestPaths[record.GuestPath] = struct{}{}
	}
	return nil
}

func validateGitRecord(manifest Manifest, record GitRecord) error {
	if !validRepositoryName(record.Repository) {
		return fmt.Errorf("repository must be a safe canonical name")
	}
	if !cleanAbsoluteHostPath(record.HostPath) || !hostPathWithin(manifest.CanonicalRoot, record.HostPath) {
		return fmt.Errorf("host path must be a clean absolute path within the project")
	}
	if err := gitx.ValidateRepositoryIdentityContract(record.Identity); err != nil {
		return fmt.Errorf("repository physical identity is invalid: %w", err)
	}
	if record.Identity.ApprovedRoot.CanonicalPath != manifest.CanonicalRoot ||
		record.Identity.Worktree.CanonicalPath != record.HostPath ||
		!hostPathWithin(manifest.CanonicalRoot, record.Identity.GitDir.CanonicalPath) {
		return fmt.Errorf("repository physical identity does not match the manifest project and host paths")
	}
	if !cleanGuestRepositoryPath(record.GuestPath) {
		return fmt.Errorf("guest path must be a clean absolute path at or below /workspace")
	}
	if !validGitRef(record.SourceBranch) {
		return fmt.Errorf("source branch is invalid")
	}
	if !validGitObjectID(record.SourceRevision) {
		return fmt.Errorf("source revision must be a lowercase Git object ID")
	}
	if !validSHA256(record.TrackedFingerprint) {
		return fmt.Errorf("tracked fingerprint must be a lowercase SHA-256 digest")
	}
	if record.WorkspaceBranch != "dsx/"+string(manifest.Workspace) {
		return fmt.Errorf("workspace branch is outside workspace namespace")
	}
	if !validSHA256(record.SourceBundleDigest) {
		return fmt.Errorf("source bundle digest must be a lowercase SHA-256 digest")
	}
	if record.BackupRef != "" && record.BackupRef != "refs/dsx/backups/"+string(manifest.Workspace) {
		return fmt.Errorf("backup ref is outside workspace namespace")
	}
	if record.Conflict != (record.ConflictSourceRevision != "" && record.ConflictBundleDigest != "") {
		return fmt.Errorf("conflict state, source revision, and bundle digest must be recorded together")
	}
	if record.ConflictSourceRevision != "" && !validGitObjectID(record.ConflictSourceRevision) {
		return fmt.Errorf("conflict source revision must be a lowercase Git object ID")
	}
	if record.ConflictBundleDigest != "" && !validSHA256(record.ConflictBundleDigest) {
		return fmt.Errorf("conflict bundle digest must be a lowercase SHA-256 digest")
	}
	if record.ResultCommit == "" {
		if record.ResultBundleDigest != "" {
			return fmt.Errorf("result bundle digest exists without result commit")
		}
		if record.FetchedCommit != "" || record.FetchedHostRef != "" {
			return fmt.Errorf("fetched state exists without result commit")
		}
		return nil
	}
	if !validGitObjectID(record.ResultCommit) {
		return fmt.Errorf("result commit must be a lowercase Git object ID")
	}
	if !validSHA256(record.ResultBundleDigest) {
		return fmt.Errorf("result bundle digest must be a lowercase SHA-256 digest")
	}
	if record.FetchedCommit == "" {
		if record.FetchedHostRef != "" {
			return fmt.Errorf("fetched host ref exists without fetched commit")
		}
		return nil
	}
	if !validGitObjectID(record.FetchedCommit) {
		return fmt.Errorf("fetched commit must be a lowercase Git object ID")
	}
	if record.FetchedHostRef != gitx.RefNamespace+string(manifest.Workspace) {
		return fmt.Errorf("fetched host ref is outside workspace namespace")
	}
	return nil
}

func validRepositoryName(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	previousSeparator := false
	for _, character := range value {
		separator := character == '-' || character == '_'
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && !separator {
			return false
		}
		if separator && previousSeparator {
			return false
		}
		previousSeparator = separator
	}
	return !previousSeparator
}

func cleanAbsoluteHostPath(value string) bool {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func hostPathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func cleanGuestRepositoryPath(value string) bool {
	if value == "" || !path.IsAbs(value) || path.Clean(value) != value {
		return false
	}
	return value == "/workspace" || strings.HasPrefix(value, "/workspace/")
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	for _, character := range value {
		if character >= 'A' && character <= 'F' {
			return false
		}
	}
	return true
}

func validGitRef(value string) bool {
	if value == "" || len(value) > 1024 || value == "@" || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.HasSuffix(value, ".") || strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.Contains(value, "//") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	for _, character := range value {
		if character <= 0x20 || character == 0x7f || strings.ContainsRune(`~^:?*[\`, character) {
			return false
		}
	}
	return true
}

type legacyManifestV1 struct {
	Version        int                  `json:"version"`
	Generation     uint64               `json:"generation"`
	ProjectID      model.ProjectID      `json:"project_id"`
	CanonicalRoot  string               `json:"canonical_root"`
	Sandbox        model.WorkspaceName  `json:"sandbox"`
	RunID          model.RunID          `json:"run_id"`
	Mode           string               `json:"mode"`
	PlanHash       string               `json:"plan_hash"`
	State          model.WorkspaceState `json:"state"`
	Operation      string               `json:"operation"`
	Resources      []ResourceRecord     `json:"resources"`
	HostBindings   []HostBindingRecord  `json:"host_bindings,omitempty"`
	Git            []legacyGitRecordV1  `json:"git,omitempty"`
	UncapturedWork bool                 `json:"uncaptured_work,omitempty"`
	Failure        string               `json:"failure,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
}

type legacyGitRecordV1 struct {
	Repository         string                  `json:"repository"`
	HostPath           string                  `json:"host_path"`
	GuestPath          string                  `json:"guest_path"`
	Identity           gitx.RepositoryIdentity `json:"identity"`
	SourceRef          string                  `json:"source_ref"`
	SourceCommit       string                  `json:"source_commit"`
	TrackedFingerprint string                  `json:"tracked_fingerprint"`
	WarnUntracked      bool                    `json:"warn_untracked,omitempty"`
	WarnIgnored        bool                    `json:"warn_ignored,omitempty"`
	ResultBranch       string                  `json:"result_branch"`
	ResultCommit       string                  `json:"result_commit,omitempty"`
	SourceBundleDigest string                  `json:"source_bundle_digest"`
	ResultBundleDigest string                  `json:"result_bundle_digest,omitempty"`
	FetchedCommit      string                  `json:"fetched_commit,omitempty"`
	FetchedHostRef     string                  `json:"fetched_host_ref,omitempty"`
}

func DecodeManifest(data []byte) (Manifest, error) {
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return Manifest{}, err
	}
	switch header.Version {
	case ManifestVersion:
		var manifest Manifest
		if err := decodeManifestJSON(data, &manifest); err != nil {
			return Manifest{}, err
		}
		if err := ValidateManifest(manifest); err != nil {
			return Manifest{}, err
		}
		return manifest, nil
	case LegacyManifestVersion:
		var legacy legacyManifestV1
		if err := decodeManifestJSON(data, &legacy); err != nil {
			return Manifest{}, err
		}
		manifest := Manifest{
			Version: legacy.Version, Generation: legacy.Generation, ProjectID: legacy.ProjectID,
			CanonicalRoot: legacy.CanonicalRoot, Workspace: legacy.Sandbox, RunID: legacy.RunID,
			PlanHash: legacy.PlanHash, State: legacy.State, Operation: legacy.Operation,
			Resources: legacy.Resources, HostBindings: legacy.HostBindings,
			UncapturedWork: legacy.UncapturedWork, Failure: legacy.Failure,
			CreatedAt: legacy.CreatedAt, UpdatedAt: legacy.UpdatedAt, Legacy: true,
		}
		manifest.Git = make([]GitRecord, len(legacy.Git))
		for index, record := range legacy.Git {
			manifest.Git[index] = GitRecord{
				Repository: record.Repository, HostPath: record.HostPath, GuestPath: record.GuestPath,
				Identity: record.Identity, SourceBranch: record.SourceRef, SourceRevision: record.SourceCommit,
				TrackedFingerprint: record.TrackedFingerprint, WarnUntracked: record.WarnUntracked,
				WarnIgnored: record.WarnIgnored, WorkspaceBranch: record.ResultBranch,
				ResultCommit: record.ResultCommit, SourceBundleDigest: record.SourceBundleDigest,
				ResultBundleDigest: record.ResultBundleDigest, FetchedCommit: record.FetchedCommit,
				FetchedHostRef: record.FetchedHostRef,
			}
		}
		if err := ValidateLegacyManifestForCleanup(manifest); err != nil {
			return Manifest{}, err
		}
		return manifest, nil
	default:
		return Manifest{}, fmt.Errorf("unsupported manifest version %d", header.Version)
	}
}

func decodeManifestJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func ValidateLegacyManifestForCleanup(manifest Manifest) error {
	if manifest.Version != LegacyManifestVersion || !manifest.Legacy {
		return fmt.Errorf("manifest is not legacy cleanup evidence")
	}
	if manifest.AWSGrant != nil {
		return fmt.Errorf("legacy manifest contains an AWS grant")
	}
	if manifest.Generation == 0 {
		return fmt.Errorf("legacy manifest generation must be positive")
	}
	if projectID, err := model.ParseProjectID(string(manifest.ProjectID)); err != nil || projectID != manifest.ProjectID {
		return fmt.Errorf("legacy manifest project ID is invalid")
	}
	if workspace, err := model.ParseWorkspaceName(string(manifest.Workspace)); err != nil || workspace != manifest.Workspace {
		return fmt.Errorf("legacy manifest workspace is invalid")
	}
	if runID, err := model.ParseRunID(string(manifest.RunID)); err != nil || runID != manifest.RunID {
		return fmt.Errorf("legacy manifest run ID is invalid")
	}
	for index, resource := range manifest.Resources {
		expected := LegacyResourceOwnershipLabels(manifest.ProjectID, manifest.Workspace, manifest.RunID, resource.Kind, resource.Role)
		if resource.ExpectedID == "" || resource.Name == "" || resource.ExpectedID != resource.Name || len(resource.Labels) != len(expected) {
			return fmt.Errorf("legacy manifest resource %d lacks exact ownership evidence", index)
		}
		for labelIndex := range expected {
			if resource.Labels[labelIndex] != expected[labelIndex] {
				return fmt.Errorf("legacy manifest resource %d ownership label %d does not match", index, labelIndex)
			}
		}
	}
	return nil
}

type ManifestRepository interface {
	CreateIntent(context.Context, Manifest) error
	LoadManifest(context.Context, model.ProjectID, model.WorkspaceName, model.RunID) (Manifest, bool, error)
	ReplaceManifest(context.Context, Manifest, uint64) error
	ListProjectManifests(context.Context, model.ProjectID) ([]Manifest, error)
	ListAllManifests(context.Context) ([]Manifest, error)
	DeleteManifest(context.Context, model.ProjectID, model.WorkspaceName, model.RunID) error
}
