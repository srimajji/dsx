package fs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/state"
)

func TestManifestRepositoryPersistsThreeWorkspacePeersDeterministically(t *testing.T) {
	root := t.TempDir()
	repository, err := NewManifestRepository(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	for index, name := range []model.WorkspaceName{"tests", "api", "feature-a"} {
		manifest := fsManifestFixture(t, root, name, model.RunID([]string{
			"01890f5c-7b00-7000-8000-000000000001",
			"01890f5c-7b00-7000-8000-000000000002",
			"01890f5c-7b00-7000-8000-000000000003",
		}[index]))
		if err := repository.CreateIntent(context.Background(), manifest); err != nil {
			t.Fatal(err)
		}
	}
	projectID, _ := model.NewProjectID(root)
	manifests, err := repository.ListProjectManifests(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 3 || manifests[0].Workspace != "api" || manifests[1].Workspace != "feature-a" || manifests[2].Workspace != "tests" {
		t.Fatalf("workspace listing = %#v", manifests)
	}
}

func TestManifestRepositoryLegacyDecodeIsImmutable(t *testing.T) {
	// Legacy decoding is covered in state; repository replacement must never adopt it.
	root := t.TempDir()
	repository, err := NewManifestRepository(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := fsManifestFixture(t, root, "legacy", "01890f5c-7b00-7000-8000-000000000004")
	manifest.Legacy = true
	manifest.Version = state.LegacyManifestVersion
	if err := repository.ReplaceManifest(context.Background(), manifest, 1); err == nil {
		t.Fatal("legacy manifest replacement was accepted")
	}
}

func fsManifestFixture(t *testing.T, root string, workspace model.WorkspaceName, runID model.RunID) state.Manifest {
	t.Helper()
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := state.Manifest{
		Version: state.ManifestVersion, Generation: 1, ProjectID: projectID, CanonicalRoot: root,
		Workspace: workspace, RunID: runID, PlanHash: strings.Repeat("a", 64), State: model.StatePlanned,
		Operation: "create", CreatedAt: time.Unix(1_700_000_000, 0).UTC(), UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	for _, item := range []struct{ kind, role string }{{"network", "network"}, {"volume", "source"}, {"volume", "auth"}, {"volume", "session"}, {"volume", "deps"}, {"volume", "service"}, {"workspace", "workspace"}} {
		name, nameErr := state.CanonicalResourceName(root, workspace, item.role)
		if nameErr != nil {
			t.Fatal(nameErr)
		}
		manifest.Resources = append(manifest.Resources, state.ResourceRecord{Kind: item.kind, Role: item.role, Name: name, ExpectedID: name, Labels: state.ResourceOwnershipLabels(projectID, workspace, runID, item.kind, item.role)})
	}
	identity := fsManifestPhysicalIdentity(root)
	gitDir := fsManifestPhysicalIdentity(filepath.Join(root, ".git"))
	manifest.Git = []state.GitRecord{{Repository: "workspace", HostPath: root, GuestPath: "/workspace", Identity: gitx.RepositoryIdentity{ApprovedRoot: identity, Worktree: identity, GitDir: gitDir}, SourceBranch: "refs/heads/main", SourceRevision: strings.Repeat("1", 40), TrackedFingerprint: strings.Repeat("2", 64), WorkspaceBranch: "dsx/" + string(workspace), SourceBundleDigest: strings.Repeat("3", 64)}}
	return manifest
}

func fsManifestPhysicalIdentity(value string) gitx.PhysicalPathIdentity {
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
