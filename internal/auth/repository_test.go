package auth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/harness"
	harnessclaude "github.com/srimajji/dsx/internal/harness/claude"
	harnessomp "github.com/srimajji/dsx/internal/harness/omp"
	"github.com/srimajji/dsx/internal/model"
)

var testLayout = harness.AuthLayout{Backend: harness.StorageFile, CredentialArtifacts: []string{"auth.json"}, MaxArtifactBytes: 1 << 20, Environment: map[string]string{"CODEX_HOME": "."}}

type staticSeeder struct {
	layout   harness.AuthLayout
	validate func(harness.SeedRequest) error
}

func (seeder staticSeeder) AuthLayout() harness.AuthLayout {
	return seeder.layout
}

func (seeder staticSeeder) Seed(ctx context.Context, request harness.SeedRequest) error {
	if seeder.validate != nil {
		if err := seeder.validate(request); err != nil {
			return err
		}
	}
	return harness.SeedArtifacts(ctx, request)
}

var testAuthSeeder = staticSeeder{layout: testLayout}

func TestRepositoryIndependentCopiesCASConflictAndPurge(t *testing.T) {
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	writeCredential(t, source, "seed")
	profile := Profile{Harness: harness.Codex, Name: "default"}
	if _, err := repository.Import(context.Background(), profile, source, testAuthSeeder); err != nil {
		t.Fatal(err)
	}
	first := prepareCopy(t, repository, profile, "00000000-0000-7000-8000-000000000001")
	second := prepareCopy(t, repository, profile, "00000000-0000-7000-8000-000000000002")
	firstInfo, err := os.Stat(filepath.Join(first.Root, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(filepath.Join(second.Root, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(firstInfo, secondInfo) {
		t.Fatal("concurrent auth copies share an inode")
	}
	writeCredential(t, first.Root, "first-refresh")
	promoted, err := repository.Promote(context.Background(), first, testAuthSeeder)
	if err != nil || promoted.Conflict || promoted.Digest == "" {
		t.Fatalf("first promote = %#v, %v", promoted, err)
	}
	writeCredential(t, second.Root, "second-refresh")
	conflict, err := repository.Promote(context.Background(), second, testAuthSeeder)
	if err != nil || !conflict.Conflict || conflict.CandidateRoot == "" {
		t.Fatalf("stale promote = %#v, %v", conflict, err)
	}
	assertCredential(t, conflict.CandidateRoot, "second-refresh")
	third := prepareCopy(t, repository, profile, "00000000-0000-7000-8000-000000000003")
	assertCredential(t, third.Root, "first-refresh")
	if err := repository.RemoveRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.Root); !os.IsNotExist(err) {
		t.Fatalf("run root remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repository.root, "runs", string(first.RunID))); !os.IsNotExist(err) {
		t.Fatalf("empty run hierarchy remains: %v", err)
	}
	fourth := prepareCopy(t, repository, profile, "00000000-0000-7000-8000-000000000004")
	assertCredential(t, fourth.Root, "first-refresh")
	for _, copy := range []Copy{second, third, fourth} {
		if err := repository.RemoveRun(context.Background(), copy); err != nil {
			t.Fatal(err)
		}
	}

	otherSource := t.TempDir()
	writeCredential(t, otherSource, "other")
	other := Profile{Harness: harness.Claude, Name: "work"}
	if _, err := repository.Import(context.Background(), other, otherSource, testAuthSeeder); err != nil {
		t.Fatal(err)
	}
	otherCandidate := repository.candidateRoot(other, second.RunID)
	writeCredential(t, otherCandidate, "other-conflict")
	otherPointer := filepath.Join(repository.profileRoot(other), currentFile)
	otherBefore, err := os.ReadFile(otherPointer)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Purge(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(conflict.CandidateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("matching conflict candidate survived purge: %v", err)
	}
	otherAfter, err := os.ReadFile(otherPointer)
	if err != nil || string(otherAfter) != string(otherBefore) {
		t.Fatalf("other profile bytes changed: before=%q after=%q err=%v", otherBefore, otherAfter, err)
	}
	assertCredential(t, otherCandidate, "other-conflict")
	if _, err := repository.Prepare(context.Background(), profile, model.RunID("00000000-0000-7000-8000-000000000005"), testAuthSeeder); err == nil {
		t.Fatal("purged profile remained usable")
	}
	otherCopy := prepareCopy(t, repository, other, "00000000-0000-7000-8000-000000000006")
	assertCredential(t, otherCopy.Root, "other")
}

func TestRemoveRunPreservesConcurrentProfileHierarchy(t *testing.T) {
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	writeCredential(t, source, "seed")
	codex := Profile{Harness: harness.Codex, Name: "default"}
	claude := Profile{Harness: harness.Claude, Name: "default"}
	for _, profile := range []Profile{codex, claude} {
		if _, err := repository.Import(context.Background(), profile, source, testAuthSeeder); err != nil {
			t.Fatal(err)
		}
	}
	runID := "00000000-0000-7000-8000-000000000010"
	first := prepareCopy(t, repository, codex, runID)
	second := prepareCopy(t, repository, claude, runID)
	if err := repository.RemoveRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	assertCredential(t, second.Root, "seed")
	if _, err := os.Stat(filepath.Join(repository.root, "runs", runID)); err != nil {
		t.Fatalf("concurrent run hierarchy removed: %v", err)
	}
	if err := repository.RemoveRun(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository.root, "runs", runID)); !os.IsNotExist(err) {
		t.Fatalf("empty concurrent run hierarchy remains: %v", err)
	}
}

func TestRepositoryRejectsUnsafeAuthorityAndCorruptSeed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "auth")
	repository, err := NewRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	profile := Profile{Harness: harness.Codex, Name: "default"}
	source := t.TempDir()
	external := filepath.Join(t.TempDir(), "external")
	writeCredential(t, filepath.Dir(external), "outside")
	if err := os.Symlink(external, filepath.Join(source, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Import(context.Background(), profile, source, testAuthSeeder); err == nil {
		t.Fatal("import accepted symlink credential")
	}
	unsafeSeeder := staticSeeder{layout: harness.AuthLayout{Backend: harness.StorageFile, CredentialArtifacts: []string{"../auth.json"}, MaxArtifactBytes: 1 << 20}}
	if _, err := repository.Import(context.Background(), profile, source, unsafeSeeder); err == nil {
		t.Fatal("import accepted escaping allowlist")
	}
	if err := os.Remove(filepath.Join(source, "auth.json")); err != nil {
		t.Fatal(err)
	}
	writeCredential(t, source, "valid")
	digest, err := repository.Import(context.Background(), profile, source, testAuthSeeder)
	if err != nil {
		t.Fatal(err)
	}
	generationFile := filepath.Join(root, "profiles", "codex", "default", "generations", digest, "auth.json")
	if err := os.WriteFile(generationFile, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Prepare(context.Background(), profile, model.RunID("00000000-0000-7000-8000-000000000007"), testAuthSeeder); err == nil {
		t.Fatal("corrupt credential seed was accepted")
	}
	if err := os.WriteFile(generationFile, []byte("valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "profiles", "codex", "default", currentFile), []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Prepare(context.Background(), profile, model.RunID("00000000-0000-7000-8000-000000000008"), testAuthSeeder); err == nil {
		t.Fatal("corrupt profile pointer was accepted")
	}
	copy := Copy{Profile: profile, RunID: model.RunID("00000000-0000-7000-8000-000000000009"), Root: external, BaselineDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := repository.RemoveRun(context.Background(), copy); err == nil {
		t.Fatal("arbitrary auth copy path was removable")
	}
}
func TestRepositoryPurgeRejectsActiveMatchingCopy(t *testing.T) {
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	writeCredential(t, source, "seed")
	profile := Profile{Harness: harness.Codex, Name: "default"}
	if _, err := repository.Import(context.Background(), profile, source, testAuthSeeder); err != nil {
		t.Fatal(err)
	}
	active := prepareCopy(t, repository, profile, "00000000-0000-7000-8000-000000000011")
	if err := repository.Purge(context.Background(), profile); err == nil {
		t.Fatal("purge accepted a profile with an active run copy")
	}
	assertCredential(t, active.Root, "seed")
	future := prepareCopy(t, repository, profile, "00000000-0000-7000-8000-000000000012")
	assertCredential(t, future.Root, "seed")
}

func TestRepositoryUsesAdapterArtifactLimitForFingerprintAndPromotion(t *testing.T) {
	layout := harness.AuthLayout{
		Backend: harness.StorageSQLite, CredentialArtifacts: []string{"agent.db"}, MaxArtifactBytes: 16 << 20,
	}
	seeder := staticSeeder{layout: layout}
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	profile := Profile{Harness: harness.OMP, Name: "large"}
	source := t.TempDir()
	large := make([]byte, 2<<20)
	if err := os.WriteFile(filepath.Join(source, "agent.db"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Import(context.Background(), profile, source, seeder); err != nil {
		t.Fatalf("import OMP artifact above one MiB: %v", err)
	}
	copy, err := repository.Prepare(context.Background(), profile, model.RunID("00000000-0000-7000-8000-000000000042"), seeder)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(copy.Root, "agent.db"))
	if err != nil || info.Size() != int64(len(large)) {
		t.Fatalf("prepared OMP artifact = %v, %v", info, err)
	}
	large[len(large)-1] = 1
	if err := os.WriteFile(filepath.Join(copy.Root, "agent.db"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	promotion, err := repository.Promote(context.Background(), copy, seeder)
	if err != nil || promotion.Digest == "" || promotion.Conflict {
		t.Fatalf("promote OMP artifact above one MiB = %#v, %v", promotion, err)
	}

	oversizedSource := t.TempDir()
	if err := os.WriteFile(filepath.Join(oversizedSource, "agent.db"), make([]byte, (16<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	oversizedProfile := Profile{Harness: harness.OMP, Name: "oversized"}
	if _, err := repository.Import(context.Background(), oversizedProfile, oversizedSource, seeder); err == nil {
		t.Fatal("import accepted OMP artifact larger than 16 MiB")
	}
}

func TestRepositoryRefreshIsCoherentAndDeletedArtifactsStayAbsent(t *testing.T) {
	layout := harness.AuthLayout{
		Backend:             harness.StorageSQLite,
		CredentialArtifacts: []string{"auth.json", "auth.json-wal"},
		MaxArtifactBytes:    16 << 20,
	}
	seeder := staticSeeder{
		layout: layout,
		validate: func(request harness.SeedRequest) error {
			info, err := os.Stat(request.DestinationRoot)
			if err != nil || info.Mode().Perm() != 0o700 {
				return errors.New("staging root is not private")
			}
			data, err := os.ReadFile(filepath.Join(request.SourceRoot, "auth.json"))
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if string(data) == "corrupt" {
				return errors.New("corrupt coherent snapshot")
			}
			return nil
		},
	}
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	writePrivateArtifact(t, source, "auth.json", "seed")
	writePrivateArtifact(t, source, "auth.json-wal", "old-wal")
	profile := Profile{Harness: harness.OMP, Name: "default"}
	if _, err := repository.Import(context.Background(), profile, source, seeder); err != nil {
		t.Fatal(err)
	}
	copy, err := repository.Prepare(context.Background(), profile, model.RunID("00000000-0000-7000-8000-000000000013"), seeder)
	if err != nil {
		t.Fatal(err)
	}

	partial := t.TempDir()
	writePrivateArtifact(t, partial, "auth.json", "corrupt")
	if err := repository.Refresh(context.Background(), copy, partial, seeder); err == nil {
		t.Fatal("corrupt partial refresh succeeded")
	}
	assertCredential(t, copy.Root, "seed")
	wal, err := os.ReadFile(filepath.Join(copy.Root, "auth.json-wal"))
	if err != nil || string(wal) != "old-wal" {
		t.Fatalf("failed refresh mutated WAL: %q, %v", wal, err)
	}

	writePrivateArtifact(t, partial, "auth.json", "refreshed")
	if err := repository.Refresh(context.Background(), copy, partial, seeder); err != nil {
		t.Fatal(err)
	}
	assertCredential(t, copy.Root, "refreshed")
	if _, err := os.Stat(filepath.Join(copy.Root, "auth.json-wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed-absent WAL remained after refresh: %v", err)
	}
	loggedOut := t.TempDir()
	if err := repository.Refresh(context.Background(), copy, loggedOut, seeder); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range layout.CredentialArtifacts {
		if _, err := os.Stat(filepath.Join(copy.Root, artifact)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("logged-out artifact %q survived refresh: %v", artifact, err)
		}
	}
}

func TestRepositorySandboxSeedPersistsUntilExactSandboxPurge(t *testing.T) {
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	projectID := model.ProjectID("aaaaaaaaaaaaaaaaaaaa")
	global := Profile{Harness: harness.Codex, Name: "ephemeral"}
	globalSource := t.TempDir()
	writeCredential(t, globalSource, "global")
	if _, err := repository.Import(context.Background(), global, globalSource, testAuthSeeder); err != nil {
		t.Fatal(err)
	}
	firstProfile := SandboxProfile(global, projectID, "first")
	siblingProfile := SandboxProfile(global, projectID, "sibling")
	first := prepareSandboxCopy(t, repository, firstProfile, "00000000-0000-7000-8000-000000000014")
	if _, err := os.Stat(filepath.Join(first.Root, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new sandbox inherited global credentials: %v", err)
	}
	writeCredential(t, first.Root, "sandbox-only")
	promotion, err := repository.Promote(context.Background(), first, testAuthSeeder)
	if err != nil || promotion.Conflict || promotion.Digest == "" {
		t.Fatalf("sandbox promotion = %#v, %v", promotion, err)
	}
	if err := repository.RemoveRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	later := prepareSandboxCopy(t, repository, firstProfile, "00000000-0000-7000-8000-000000000015")
	assertCredential(t, later.Root, "sandbox-only")
	sibling := prepareSandboxCopy(t, repository, siblingProfile, "00000000-0000-7000-8000-000000000016")
	if _, err := os.Stat(filepath.Join(sibling.Root, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sibling sandbox inherited credentials: %v", err)
	}
	if err := repository.PurgeSandbox(context.Background(), projectID, "first"); !errors.Is(err, ErrActiveCopies) {
		t.Fatalf("purge with active invocation = %v", err)
	}
	if err := repository.RemoveRun(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	if err := repository.PurgeSandbox(context.Background(), projectID, "first"); err != nil {
		t.Fatal(err)
	}
	if err := repository.PurgeSandbox(context.Background(), projectID, "first"); err != nil {
		t.Fatalf("idempotent exact purge: %v", err)
	}
	recreated := prepareSandboxCopy(t, repository, firstProfile, "00000000-0000-7000-8000-000000000017")
	if _, err := os.Stat(filepath.Join(recreated.Root, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purged sandbox seed survived: %v", err)
	}
	globalCopy := prepareCopy(t, repository, global, "00000000-0000-7000-8000-000000000018")
	assertCredential(t, globalCopy.Root, "global")
	if _, err := os.Stat(sibling.Root); err != nil {
		t.Fatalf("exact purge removed sibling sandbox: %v", err)
	}
}

func TestRepositoryCleanedSandboxPurgeRemovesAbandonedRunCopies(t *testing.T) {
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	projectID := model.ProjectID("aaaaaaaaaaaaaaaaaaaa")
	base := Profile{Harness: harness.Codex, Name: "ephemeral"}
	target := SandboxProfile(base, projectID, "target")
	sibling := SandboxProfile(base, projectID, "sibling")
	abandoned := prepareSandboxCopy(t, repository, target, "00000000-0000-7000-8000-000000000019")
	siblingCopy := prepareSandboxCopy(t, repository, sibling, "00000000-0000-7000-8000-000000000020")

	if err := repository.PurgeCleanedSandbox(context.Background(), projectID, "target"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandoned.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned run copy survived cleaned sandbox purge: %v", err)
	}
	if _, err := os.Stat(siblingCopy.Root); err != nil {
		t.Fatalf("cleaned sandbox purge removed sibling run copy: %v", err)
	}
	if err := repository.PurgeCleanedSandbox(context.Background(), projectID, "target"); err != nil {
		t.Fatalf("idempotent cleaned sandbox purge: %v", err)
	}
}
func TestRepositoryScopedGlobalConflictSurvivesCleanAndIsRemovedByExactPurge(t *testing.T) {
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	global := Profile{Harness: harness.Codex, Name: "default"}
	source := t.TempDir()
	writeCredential(t, source, "seed")
	if _, err := repository.Import(context.Background(), global, source, testAuthSeeder); err != nil {
		t.Fatal(err)
	}
	projectID := model.ProjectID("aaaaaaaaaaaaaaaaaaaa")
	otherProjectID := model.ProjectID("bbbbbbbbbbbbbbbbbbbb")
	first := prepareScopedGlobalCopy(t, repository, global, "00000000-0000-7000-8000-000000000030", projectID, "target")
	stale := prepareScopedGlobalCopy(t, repository, global, "00000000-0000-7000-8000-000000000031", projectID, "target")
	sibling := prepareScopedGlobalCopy(t, repository, global, "00000000-0000-7000-8000-000000000032", projectID, "sibling")
	otherProject := prepareScopedGlobalCopy(t, repository, global, "00000000-0000-7000-8000-000000000033", otherProjectID, "target")
	expectedRoot := filepath.Join(repository.root, "runs", string(first.RunID), "sandboxes", string(projectID), "target", "codex", "default")
	if first.Root != expectedRoot || first.Profile != global || first.OwnerProjectID != projectID || first.OwnerSandbox != "target" {
		t.Fatalf("scoped global copy authority = %#v, root %q", first, expectedRoot)
	}

	tamperedOwner := first
	tamperedOwner.OwnerSandbox = "sibling"
	if err := repository.RemoveRun(context.Background(), tamperedOwner); err == nil {
		t.Fatal("copy with mismatched ownership was removable")
	}
	tamperedProfile := first
	tamperedProfile.Profile = SandboxProfile(global, projectID, "target")
	if _, err := repository.Promote(context.Background(), tamperedProfile, testAuthSeeder); err == nil || !strings.Contains(err.Error(), "durable scope index") {
		t.Fatalf("copy with mismatched profile scope promotion error = %v", err)
	}

	writeCredential(t, first.Root, "promoted")
	promotion, err := repository.Promote(context.Background(), first, testAuthSeeder)
	if err != nil || promotion.Conflict || promotion.Digest == "" {
		t.Fatalf("scoped global promotion = %#v, %v", promotion, err)
	}
	writeCredential(t, stale.Root, "stale")
	conflict, err := repository.Promote(context.Background(), stale, testAuthSeeder)
	expectedCandidate := filepath.Join(repository.profileRoot(global), "conflicts", string(stale.RunID))
	if err != nil || !conflict.Conflict || conflict.CandidateRoot != expectedCandidate {
		t.Fatalf("scoped global conflict = %#v, %v; root %q", conflict, err, expectedCandidate)
	}
	assertCredential(t, conflict.CandidateRoot, "stale")
	if err := repository.Purge(context.Background(), global); !errors.Is(err, ErrActiveCopies) {
		t.Fatalf("global purge with scoped copies = %v", err)
	}

	// Simulate SIGKILL: neither target copy's normal RemoveRun defer executes.
	if err := repository.PurgeCleanedSandbox(context.Background(), projectID, "target"); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{first.Root, stale.Root} {
		if _, err := os.Stat(removed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleaned sandbox run artifact survived at %q: %v", removed, err)
		}
	}
	assertCredential(t, conflict.CandidateRoot, "stale")
	for _, preserved := range []Copy{sibling, otherProject} {
		assertCredential(t, preserved.Root, "seed")
	}
	if err := repository.Purge(context.Background(), global); !errors.Is(err, ErrActiveCopies) {
		t.Fatalf("global purge ignored surviving active copies after clean: %v", err)
	}
	if err := repository.PurgeCleanedSandbox(context.Background(), projectID, "target"); err != nil {
		t.Fatalf("idempotent cleaned sandbox purge: %v", err)
	}
	assertCredential(t, conflict.CandidateRoot, "stale")
	later := prepareScopedGlobalCopy(t, repository, global, "00000000-0000-7000-8000-000000000034", projectID, "target")
	assertCredential(t, later.Root, "promoted")

	for _, copy := range []Copy{sibling, otherProject, later} {
		if err := repository.RemoveRun(context.Background(), copy); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Purge(context.Background(), global); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(conflict.CandidateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact profile purge retained conflict candidate: %v", err)
	}
}

func TestRepositoryLegacyUnscopedOrphanBlocksGlobalPurgeWithDiagnostic(t *testing.T) {
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	profile := Profile{Harness: harness.Codex, Name: "default"}
	source := t.TempDir()
	writeCredential(t, source, "seed")
	if _, err := repository.Import(context.Background(), profile, source, testAuthSeeder); err != nil {
		t.Fatal(err)
	}
	orphan := prepareCopy(t, repository, profile, "00000000-0000-7000-8000-000000000035")
	err = repository.Purge(context.Background(), profile)
	if !errors.Is(err, ErrActiveCopies) || !strings.Contains(err.Error(), "legacy unscoped") || !strings.Contains(err.Error(), "exact run copy") {
		t.Fatalf("legacy orphan purge error = %v", err)
	}
	assertCredential(t, orphan.Root, "seed")
	if err := repository.RemoveRun(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}
	if err := repository.Purge(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryStoresReviewedConfigOutsideCredentialDigest(t *testing.T) {
	layout := harness.AuthLayout{
		Backend:             harness.StorageFile,
		CredentialArtifacts: []string{"auth.json"},
		ReadOnlyConfig:      []string{"settings.json"},
		MaxArtifactBytes:    1 << 20,
		Environment:         map[string]string{"CLAUDE_CONFIG_DIR": "."},
	}
	seeder := staticSeeder{layout: layout}
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	writeCredential(t, source, "credential")
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(`{"theme":"reviewed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := Profile{Harness: harness.Claude, Name: "reviewed"}
	digest, err := repository.Import(context.Background(), profile, source, seeder)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := repository.Prepare(context.Background(), profile, model.RunID("00000000-0000-7000-8000-000000000019"), seeder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(copy.Root, "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reviewed config entered writable credential copy: %v", err)
	}
	settings := filepath.Join(copy.ReadOnlyRoot, "settings.json")
	info, err := os.Stat(settings)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("reviewed config mode = %v, %v", info, err)
	}
	data, err := os.ReadFile(settings)
	if err != nil || string(data) != `{"theme":"reviewed"}` {
		t.Fatalf("reviewed config = %q, %v", data, err)
	}
	if copy.BaselineDigest != digest {
		t.Fatalf("credential digest changed by reviewed config: %q != %q", copy.BaselineDigest, digest)
	}
}

func TestRepositoryInvalidPromotionLeavesCurrentPointerUnchanged(t *testing.T) {
	seeder := staticSeeder{
		layout: testLayout,
		validate: func(request harness.SeedRequest) error {
			data, err := os.ReadFile(filepath.Join(request.SourceRoot, "auth.json"))
			if err != nil {
				return err
			}
			if string(data) == "truncated" {
				return errors.New("invalid credential object")
			}
			return nil
		},
	}
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	writeCredential(t, source, "valid")
	profile := Profile{Harness: harness.Claude, Name: "strict"}
	if _, err := repository.Import(context.Background(), profile, source, seeder); err != nil {
		t.Fatal(err)
	}
	copy, err := repository.Prepare(context.Background(), profile, model.RunID("00000000-0000-7000-8000-000000000020"), seeder)
	if err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(repository.profileRoot(profile), currentFile)
	before, err := os.ReadFile(pointer)
	if err != nil {
		t.Fatal(err)
	}
	writeCredential(t, copy.Root, "truncated")
	if _, err := repository.Promote(context.Background(), copy, seeder); err == nil {
		t.Fatal("invalid promotion succeeded")
	}
	after, err := os.ReadFile(pointer)
	if err != nil || string(after) != string(before) {
		t.Fatalf("invalid promotion changed current pointer: before=%q after=%q err=%v", before, after, err)
	}
}

func TestRepositorySemanticAdapterFailuresLeavePointersUnchanged(t *testing.T) {
	tests := []struct {
		name       string
		runID      model.RunID
		profile    Profile
		seeder     Seeder
		writeValid func(*testing.T, string)
		corrupt    func(*testing.T, string)
	}{
		{
			name:    "claude-truncated-json",
			runID:   "00000000-0000-7000-8000-000000000021",
			profile: Profile{Harness: harness.Claude, Name: "strict-json"},
			seeder:  harnessclaude.New(),
			writeValid: func(t *testing.T, root string) {
				writePrivateArtifact(t, root, ".credentials.json", `{"claudeAiOauth":{"accessToken":"access","refreshToken":"refresh","expiresAt":1900000000000,"scopes":[]}}`)
			},
			corrupt: func(t *testing.T, root string) {
				writePrivateArtifact(t, root, ".credentials.json", `{"claudeAiOauth":`)
			},
		},
		{
			name:    "omp-header-only-sqlite",
			runID:   "00000000-0000-7000-8000-000000000022",
			profile: Profile{Harness: harness.OMP, Name: "strict-sqlite"},
			seeder:  harnessomp.New(),
			writeValid: func(t *testing.T, root string) {
				database := filepath.Join(root, "agent.db")
				const schema = "CREATE TABLE auth_schema_version (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL);" +
					"INSERT INTO auth_schema_version(id, version) VALUES (1, 7);" +
					"CREATE TABLE auth_credentials (id INTEGER PRIMARY KEY AUTOINCREMENT, provider TEXT NOT NULL, credential_type TEXT NOT NULL, data TEXT NOT NULL, disabled_cause TEXT DEFAULT NULL, identity_key TEXT DEFAULT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);"
				command := exec.Command("/usr/bin/sqlite3", "-batch", database, schema)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("create SQLite fixture: %v: %s", err, output)
				}
				if err := os.Chmod(database, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			corrupt: func(t *testing.T, root string) {
				writePrivateArtifact(t, root, "agent.db", "SQLite format 3\x00")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
			if err != nil {
				t.Fatal(err)
			}
			source := t.TempDir()
			test.writeValid(t, source)
			if _, err := repository.Import(context.Background(), test.profile, source, test.seeder); err != nil {
				t.Fatal(err)
			}
			copy, err := repository.Prepare(context.Background(), test.profile, test.runID, test.seeder)
			if err != nil {
				t.Fatal(err)
			}
			pointer := filepath.Join(repository.profileRoot(test.profile), currentFile)
			before, err := os.ReadFile(pointer)
			if err != nil {
				t.Fatal(err)
			}
			test.corrupt(t, copy.Root)
			if _, err := repository.Promote(context.Background(), copy, test.seeder); err == nil {
				t.Fatal("semantically invalid credential snapshot was promoted")
			}
			after, err := os.ReadFile(pointer)
			if err != nil || string(after) != string(before) {
				t.Fatalf("invalid snapshot changed pointer: before=%q after=%q err=%v", before, after, err)
			}
		})
	}
}

func prepareCopy(t *testing.T, repository *Repository, profile Profile, runID string) Copy {
	t.Helper()
	copy, err := repository.Prepare(context.Background(), profile, model.RunID(runID), testAuthSeeder)
	if err != nil {
		t.Fatal(err)
	}
	return copy
}

func prepareSandboxCopy(t *testing.T, repository *Repository, profile Profile, runID string) Copy {
	t.Helper()
	copy, err := repository.PrepareSandbox(context.Background(), profile, model.RunID(runID), testAuthSeeder)
	if err != nil {
		t.Fatal(err)
	}
	return copy
}

func prepareScopedGlobalCopy(t *testing.T, repository *Repository, profile Profile, runID string, projectID model.ProjectID, sandbox string) Copy {
	t.Helper()
	copy, err := repository.PrepareGlobalSandbox(context.Background(), profile, model.RunID(runID), projectID, sandbox, testAuthSeeder)
	if err != nil {
		t.Fatal(err)
	}
	return copy
}

func writeCredential(t *testing.T, root, value string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}
func writePrivateArtifact(t *testing.T, root, relative, value string) {
	t.Helper()
	destination := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertCredential(t *testing.T, root, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "auth.json"))
	if err != nil || string(data) != want {
		t.Fatalf("credential = %q, %v; want %q", data, err, want)
	}
}
