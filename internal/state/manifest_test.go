package state

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
)

func TestValidateManifestGitRecords(t *testing.T) {
	valid := cloneManifestFixture(t)
	if err := ValidateManifest(valid); err != nil {
		t.Fatalf("valid clone manifest rejected: %v", err)
	}

	tests := map[string]func(*Manifest){
		"live manifest owns no git": func(manifest *Manifest) { manifest.Mode = model.ModeLive },
		"clone requires repository": func(manifest *Manifest) { manifest.Git = nil },
		"repository is complete":    func(manifest *Manifest) { manifest.Git[0].Repository = "" },
		"repository is safe":        func(manifest *Manifest) { manifest.Git[0].Repository = "api\x1b[2J" },
		"host path is complete":     func(manifest *Manifest) { manifest.Git[0].HostPath = "" },
		"host path stays in project": func(manifest *Manifest) {
			manifest.Git[0].HostPath = "/tmp/other-project"
		},
		"identity is complete": func(manifest *Manifest) {
			manifest.Git[0].Identity = gitx.RepositoryIdentity{}
		},
		"identity worktree matches host": func(manifest *Manifest) {
			manifest.Git[0].Identity.Worktree.CanonicalPath = manifest.CanonicalRoot
		},
		"identity gitdir stays in project": func(manifest *Manifest) {
			manifest.Git[0].Identity.GitDir = manifestPhysicalIdentity("/tmp/other-project/.git")
		},
		"guest path is complete": func(manifest *Manifest) { manifest.Git[0].GuestPath = "" },
		"guest path stays in workspace": func(manifest *Manifest) {
			manifest.Git[0].GuestPath = "/etc/project"
		},
		"source ref is complete": func(manifest *Manifest) { manifest.Git[0].SourceRef = "" },
		"source ref is safe":     func(manifest *Manifest) { manifest.Git[0].SourceRef = "main\nother" },
		"source commit is complete": func(manifest *Manifest) {
			manifest.Git[0].SourceCommit = ""
		},
		"result commit is complete": func(manifest *Manifest) {
			manifest.Git[0].ResultCommit = "abcd"
		},
		"fetched commit is complete": func(manifest *Manifest) {
			manifest.Git[0].FetchedCommit = "abcd"
		},
		"tracked fingerprint is complete": func(manifest *Manifest) {
			manifest.Git[0].TrackedFingerprint = ""
		},
		"source bundle digest is complete": func(manifest *Manifest) {
			manifest.Git[0].SourceBundleDigest = ""
		},
		"result bundle digest is complete": func(manifest *Manifest) {
			manifest.Git[0].ResultBundleDigest = "abcd"
		},
		"result branch belongs to sandbox": func(manifest *Manifest) {
			manifest.Git[0].ResultBranch = "dsx/other"
		},
		"result commit requires bundle digest": func(manifest *Manifest) {
			manifest.Git[0].ResultBundleDigest = ""
		},
		"result digest requires commit": func(manifest *Manifest) {
			manifest.Git[0].ResultCommit = ""
		},
		"fetched commit requires host ref": func(manifest *Manifest) {
			manifest.Git[0].FetchedHostRef = ""
		},
		"fetched ref belongs to sandbox": func(manifest *Manifest) {
			manifest.Git[0].FetchedHostRef = gitx.RefNamespace + "other"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			manifest := cloneManifestFixture(t)
			mutate(&manifest)
			if err := ValidateManifest(manifest); err == nil {
				t.Fatal("invalid git record accepted")
			}
		})
	}
}

func TestValidateManifestHostBindingsAreExactAndUnique(t *testing.T) {
	valid := cloneManifestFixture(t)
	valid.HostBindings = []HostBindingRecord{
		{Name: "api", HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: 49152, GuestPort: 3000, Protocol: "tcp"},
		{Name: "public", HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 49153, GuestPort: 3001, Protocol: "tcp"},
	}
	if err := ValidateManifest(valid); err != nil {
		t.Fatalf("valid exact host bindings rejected: %v", err)
	}
	tests := map[string]func(*Manifest){
		"duplicate name": func(manifest *Manifest) { manifest.HostBindings[1].Name = "api" },
		"duplicate listener": func(manifest *Manifest) {
			manifest.HostBindings[1].HostIP = manifest.HostBindings[0].HostIP
			manifest.HostBindings[1].HostPort = manifest.HostBindings[0].HostPort
		},
		"zero host port":  func(manifest *Manifest) { manifest.HostBindings[0].HostPort = 0 },
		"zero guest port": func(manifest *Manifest) { manifest.HostBindings[0].GuestPort = 0 },
		"non-TCP":         func(manifest *Manifest) { manifest.HostBindings[0].Protocol = "udp" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.HostBindings = append([]HostBindingRecord(nil), valid.HostBindings...)
			mutate(&candidate)
			if err := ValidateManifest(candidate); err == nil {
				t.Fatal("invalid host binding evidence accepted")
			}
		})
	}
}

func TestValidateManifestRejectsDuplicateGitRecordOwnership(t *testing.T) {
	tests := map[string]func(*GitRecord){
		"repository": func(record *GitRecord) {
			record.HostPath = "/tmp/dsx-git-manifest/services/api-copy"
			record.GuestPath = "/workspace/services/api-copy"
		},
		"host path": func(record *GitRecord) {
			record.Repository = "api-copy"
			record.GuestPath = "/workspace/services/api-copy"
		},
		"guest path": func(record *GitRecord) {
			record.Repository = "api-copy"
			record.HostPath = "/tmp/dsx-git-manifest/services/api-copy"
		},
	}
	for name, makeDuplicate := range tests {
		t.Run(name, func(t *testing.T) {
			manifest := cloneManifestFixture(t)
			duplicate := manifest.Git[0]
			makeDuplicate(&duplicate)
			manifest.Git = append(manifest.Git, duplicate)
			if err := ValidateManifest(manifest); err == nil {
				t.Fatalf("duplicate %s accepted", name)
			}
		})
	}
}

func TestDeletedCloneManifestRequiresFetchedResults(t *testing.T) {
	manifest := cloneManifestFixture(t)
	manifest.State = model.StateDeleted
	manifest.Operation = "clean"
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("deleted fetched clone rejected: %v", err)
	}
	manifest.Git[0].FetchedCommit = ""
	manifest.Git[0].FetchedHostRef = ""
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("deleted clone with unfetched result accepted")
	}
}

func TestValidateManifestUncapturedWorkLifecycle(t *testing.T) {
	manifest := cloneManifestFixture(t)
	manifest.State = model.StateCreating
	manifest.UncapturedWork = true
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("creating clone with uncaptured work rejected: %v", err)
	}
	manifest.Operation = "capture"
	manifest.State = model.StateFailed
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("failed clone with uncaptured work rejected: %v", err)
	}
	manifest.State = model.StateCleaning
	manifest.Operation = "clean"
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("cleaning clone with uncaptured work rejected: %v", err)
	}
	manifest.State = model.StateRunning
	manifest.Operation = "create"
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("running clone with uncaptured work accepted")
	}
	manifest.State = model.StateDeleted
	manifest.Operation = "clean"
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("deleted clone with uncaptured work accepted")
	}
	manifest.State = model.StateCreating
	manifest.Mode = model.ModeLive
	manifest.Git = nil
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("live manifest with uncaptured work accepted")
	}
}

func TestGitRecordResultPredicates(t *testing.T) {
	commit := strings.Repeat("b", 40)
	for name, test := range map[string]struct {
		record    GitRecord
		hasResult bool
		isFetched bool
	}{
		"no result":          {record: GitRecord{}},
		"unfetched result":   {record: GitRecord{ResultCommit: commit}, hasResult: true},
		"mismatched fetched": {record: GitRecord{ResultCommit: commit, FetchedCommit: strings.Repeat("c", 40)}, hasResult: true},
		"fetched result":     {record: GitRecord{ResultCommit: commit, FetchedCommit: commit}, hasResult: true, isFetched: true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := test.record.HasResultWork(); got != test.hasResult {
				t.Fatalf("HasResultWork() = %t, want %t", got, test.hasResult)
			}
			if got := test.record.ResultFetched(); got != test.isFetched {
				t.Fatalf("ResultFetched() = %t, want %t", got, test.isFetched)
			}
		})
	}
}

func TestCloneManifestFetchThenCleanupSmoke(t *testing.T) {
	// Pure smoke of the durable cleanup protocol: a composite clone is blocked
	// when any member has unfetched result work and becomes eligible only after
	// every current result commit is recorded as fetched in its host namespace.
	manifest := cloneManifestFixture(t)
	second := manifest.Git[0]
	second.Repository = "web"
	second.HostPath = "/tmp/dsx-git-manifest/services/web"
	second.GuestPath = "/workspace/services/web"
	second.Identity = manifestRepositoryIdentity(manifest.CanonicalRoot, second.HostPath)
	second.ResultCommit = ""
	second.ResultBundleDigest = ""
	second.FetchedCommit = ""
	second.FetchedHostRef = ""
	manifest.Git = append(manifest.Git, second)
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("initial composite manifest: %v", err)
	}
	if cloneManifestHasUnfetchedResult(manifest) {
		t.Fatal("no-result member made fully fetched manifest ineligible")
	}

	manifest.Git[1].ResultCommit = strings.Repeat("d", 40)
	manifest.Git[1].ResultBundleDigest = strings.Repeat("e", 64)
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("unfetched result manifest: %v", err)
	}
	if !cloneManifestHasUnfetchedResult(manifest) {
		t.Fatal("unfetched composite member did not block cleanup")
	}

	manifest.Git[1].FetchedCommit = manifest.Git[1].ResultCommit
	manifest.Git[1].FetchedHostRef = gitx.RefNamespace + string(manifest.Sandbox)
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("fetched result manifest: %v", err)
	}
	if cloneManifestHasUnfetchedResult(manifest) {
		t.Fatal("fully fetched composite manifest remained blocked")
	}
}

func cloneManifestHasUnfetchedResult(manifest Manifest) bool {
	for _, record := range manifest.Git {
		if record.HasResultWork() && !record.ResultFetched() {
			return true
		}
	}
	return false
}

func cloneManifestFixture(t *testing.T) Manifest {
	t.Helper()
	root := "/tmp/dsx-git-manifest"
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	commit := strings.Repeat("b", 40)
	return Manifest{
		Version:       ManifestVersion,
		Generation:    1,
		ProjectID:     projectID,
		CanonicalRoot: root,
		Sandbox:       "review",
		RunID:         "01890f5c-7b00-7000-8000-000000000001",
		Mode:          model.ModeClone,
		PlanHash:      strings.Repeat("a", 64),
		State:         model.StatePlanned,
		Operation:     "create",
		Git: []GitRecord{{
			Repository:         "api",
			HostPath:           root + "/services/api",
			GuestPath:          "/workspace/services/api",
			Identity:           manifestRepositoryIdentity(root, root+"/services/api"),
			SourceRef:          "refs/heads/main",
			SourceCommit:       strings.Repeat("1", 40),
			TrackedFingerprint: strings.Repeat("2", 64),
			ResultBranch:       "dsx/review",
			ResultCommit:       commit,
			SourceBundleDigest: strings.Repeat("3", 64),
			ResultBundleDigest: strings.Repeat("4", 64),
			FetchedCommit:      commit,
			FetchedHostRef:     gitx.RefNamespace + "review",
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestCanonicalResourceNameBoundsLongIdentitiesWithoutCollisions(t *testing.T) {
	projectID := model.ProjectID("aaaaaaaaaaaaaaaaaaaa")
	first := CanonicalResourceName(projectID, model.SandboxName("abcdefghijklmnopqrstuvwx"), "volume-clone-workspace")
	second := CanonicalResourceName(projectID, model.SandboxName("abcdefghijklmnopqrstuvwx"), "volume-clone-workspacf")
	if len(first) != 63 || len(second) != 63 {
		t.Fatalf("canonical name lengths = %d, %d", len(first), len(second))
	}
	if first == second {
		t.Fatalf("distinct ownership identities collided at %q", first)
	}
	if again := CanonicalResourceName(projectID, model.SandboxName("abcdefghijklmnopqrstuvwx"), "volume-clone-workspace"); again != first {
		t.Fatalf("canonical name is unstable: %q != %q", again, first)
	}
}

func manifestRepositoryIdentity(root, worktree string) gitx.RepositoryIdentity {
	return gitx.RepositoryIdentity{
		ApprovedRoot: manifestPhysicalIdentity(root),
		Worktree:     manifestPhysicalIdentity(worktree),
		GitDir:       manifestPhysicalIdentity(worktree + "/.git"),
	}
}

func manifestPhysicalIdentity(value string) gitx.PhysicalPathIdentity {
	parts := strings.Split(strings.TrimPrefix(value, "/"), "/")
	components := []gitx.PathComponentIdentity{{Path: "/", Device: 1, Inode: 1}}
	current := ""
	for index, part := range parts {
		current += "/" + part
		components = append(components, gitx.PathComponentIdentity{Path: current, Device: 1, Inode: uint64(index + 2)})
	}
	return gitx.PhysicalPathIdentity{CanonicalPath: value, Components: components}
}
