package state

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
)

func TestManifestV2ValidatesWorkspaceGitAndIndependentVolumes(t *testing.T) {
	manifest := manifestV2Fixture(t)
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	roles := map[string]bool{}
	for _, resource := range manifest.Resources {
		roles[resource.Role] = true
	}
	for _, role := range []string{"source", "auth", "session", "deps", "service"} {
		if !roles[role] {
			t.Fatalf("missing independent %s volume", role)
		}
	}
	manifest.Git[0].WorkspaceBranch = "dsx/other"
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("accepted workspace branch outside workspace namespace")
	}
}

func TestManifestActiveSessionIsBoundedAndIdentityChecked(t *testing.T) {
	manifest := manifestV2Fixture(t)
	manifest.ActiveSession = &SessionRecord{
		SessionID: model.RunID("01890f5c-7b00-7000-8000-000000000090"),
		Kind:      "agent", Agent: "codex", StartedAt: manifest.UpdatedAt,
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("valid active session rejected: %v", err)
	}
	manifest.ActiveSession.BrowserResource = "missing-browser"
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("active session accepted a missing browser ownership record")
	}
	manifest.ActiveSession = &SessionRecord{
		SessionID: model.RunID("01890f5c-7b00-7000-8000-000000000091"),
		Kind:      "open", Agent: "codex", StartedAt: manifest.UpdatedAt,
	}
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("open session accepted agent-specific fields")
	}
}

func TestDecodeLegacyManifestIsCleanupOnly(t *testing.T) {
	current := manifestV2Fixture(t)
	legacyResources := append([]ResourceRecord(nil), current.Resources...)
	for index := range legacyResources {
		legacyResources[index].Labels = LegacyResourceOwnershipLabels(current.ProjectID, current.Workspace, current.RunID, legacyResources[index].Kind, legacyResources[index].Role)
	}
	wire := legacyManifestV1{
		Version: LegacyManifestVersion, Generation: 1, ProjectID: current.ProjectID,
		CanonicalRoot: current.CanonicalRoot, Sandbox: current.Workspace, RunID: current.RunID,
		Mode: "clone", PlanHash: current.PlanHash, State: model.StateStopped, Operation: "clean",
		Resources: legacyResources, CreatedAt: current.CreatedAt, UpdatedAt: current.UpdatedAt,
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Legacy || decoded.Version != LegacyManifestVersion || decoded.Workspace != current.Workspace {
		t.Fatalf("legacy decode = %#v", decoded)
	}
	if err := ValidateManifest(decoded); err == nil {
		t.Fatal("legacy manifest was accepted for current lifecycle")
	}
	decoded.Resources[0].Labels[0].Value = "false"
	if err := ValidateLegacyManifestForCleanup(decoded); err == nil {
		t.Fatal("legacy cleanup accepted mismatched ownership labels")
	}
}

func manifestV2Fixture(t *testing.T) Manifest {
	t.Helper()
	root := t.TempDir()
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := model.WorkspaceName("review")
	runID := model.RunID("01890f5c-7b00-7000-8000-000000000001")
	manifest := Manifest{
		Version: ManifestVersion, Generation: 1, ProjectID: projectID, CanonicalRoot: root,
		Workspace: workspace, RunID: runID, PlanHash: strings.Repeat("a", 64), DefaultAgent: "codex",
		State: model.StatePlanned, Operation: "create", CreatedAt: time.Unix(1_700_000_000, 0).UTC(), UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	for _, item := range []struct{ kind, role string }{{"network", "network"}, {"volume", "source"}, {"volume", "auth"}, {"volume", "session"}, {"volume", "deps"}, {"volume", "service"}, {"workspace", "workspace"}} {
		name, nameErr := CanonicalResourceName(root, workspace, item.role)
		if nameErr != nil {
			t.Fatal(nameErr)
		}
		manifest.Resources = append(manifest.Resources, ResourceRecord{Kind: item.kind, Role: item.role, Name: name, ExpectedID: name, Labels: ResourceOwnershipLabels(projectID, workspace, runID, item.kind, item.role)})
	}
	manifest.Git = []GitRecord{{
		Repository: "workspace", HostPath: root, GuestPath: "/workspace", Identity: manifestRepositoryIdentity(root),
		SourceBranch: "refs/heads/main", SourceRevision: strings.Repeat("1", 40), TrackedFingerprint: strings.Repeat("2", 64),
		WorkspaceBranch: "dsx/review", SourceBundleDigest: strings.Repeat("3", 64),
	}}
	return manifest
}

func manifestRepositoryIdentity(root string) gitx.RepositoryIdentity {
	return gitx.RepositoryIdentity{
		ApprovedRoot: manifestPhysicalIdentity(root),
		Worktree:     manifestPhysicalIdentity(root),
		GitDir:       manifestPhysicalIdentity(filepath.Join(root, ".git")),
	}
}

func manifestPhysicalIdentity(value string) gitx.PhysicalPathIdentity {
	components := []gitx.PathComponentIdentity{{Path: string(filepath.Separator), Device: 1, Inode: 1}}
	current := string(filepath.Separator)
	for index, part := range strings.Split(strings.TrimPrefix(value, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		components = append(components, gitx.PathComponentIdentity{Path: current, Device: 1, Inode: uint64(index + 2)})
	}
	return gitx.PhysicalPathIdentity{CanonicalPath: value, Components: components}
}
