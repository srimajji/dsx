package auth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/harness/omp"
	"github.com/srimajji/dsx/internal/model"
)

func TestProjectCredentialsWorkspaceCopiesAndConcurrentPromotion(t *testing.T) {
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil { t.Fatal(err) }
	project := Project{ID: model.ProjectID("aaaaaaaaaaaaaaaaaaaa"), Harness: harness.Codex}
	source := t.TempDir()
	writePrivateArtifact(t, source, "auth.json", `{"token":"canonical"}`)
	digest, err := repository.ImportProject(context.Background(), project, source, false, testAuthSeeder)
	if err != nil { t.Fatal(err) }

	first, err := repository.AcquireWorkspace(context.Background(), Workspace{ProjectID: project.ID, Name: "feature-a", Harness: project.Harness}, model.RunID("00000000-0000-7000-8000-000000000101"), testAuthSeeder)
	if err != nil { t.Fatal(err) }
	second, err := repository.AcquireWorkspace(context.Background(), Workspace{ProjectID: project.ID, Name: "feature-b", Harness: project.Harness}, model.RunID("00000000-0000-7000-8000-000000000102"), testAuthSeeder)
	if err != nil { t.Fatal(err) }
	firstInfo, err := os.Stat(filepath.Join(first.Root, "auth.json")); if err != nil { t.Fatal(err) }
	secondInfo, err := os.Stat(filepath.Join(second.Root, "auth.json")); if err != nil { t.Fatal(err) }
	canonicalInfo, err := os.Stat(filepath.Join(repository.generationRoot(projectProfile(project), digest), "auth.json")); if err != nil { t.Fatal(err) }
	if os.SameFile(firstInfo, secondInfo) || os.SameFile(firstInfo, canonicalInfo) || os.SameFile(secondInfo, canonicalInfo) { t.Fatal("canonical and workspace credentials share writable file identity") }
	writeCredential(t, first.Root, "first-refresh")
	writeCredential(t, second.Root, "second-refresh")

	start := make(chan struct{})
	promotions := make(chan Promotion, 2)
	errorsFound := make(chan error, 2)
	var wait sync.WaitGroup
	for _, copy := range []WorkspaceCopy{first, second} {
		copy := copy
		wait.Add(1)
		go func() { defer wait.Done(); <-start; promotion, promoteErr := repository.PromoteWorkspace(context.Background(), copy, testAuthSeeder); promotions <- promotion; errorsFound <- promoteErr }()
	}
	close(start); wait.Wait(); close(promotions); close(errorsFound)
	for promoteErr := range errorsFound { if promoteErr != nil { t.Fatal(promoteErr) } }
	successes, conflicts := 0, 0
	for promotion := range promotions { if promotion.Conflict { conflicts++ } else if promotion.Digest != "" { successes++ } }
	if successes != 1 || conflicts != 1 { t.Fatalf("serialized promotions: successes=%d conflicts=%d", successes, conflicts) }
	if err := repository.ReleaseWorkspace(context.Background(), first); err != nil { t.Fatal(err) }
	if err := repository.ReleaseWorkspace(context.Background(), second); err != nil { t.Fatal(err) }
	if err := repository.PurgeProject(context.Background(), project); err != nil { t.Fatal(err) }
}

type barrierAuthSeeder struct {
	base Seeder
	entered chan struct{}
	release chan struct{}
	once sync.Once
}

func (seeder *barrierAuthSeeder) AuthLayout() harness.AuthLayout {
	return seeder.base.AuthLayout()
}

func (seeder *barrierAuthSeeder) Seed(ctx context.Context, request harness.SeedRequest) error {
	seeder.once.Do(func() {
		close(seeder.entered)
		select {
		case <-seeder.release:
		case <-ctx.Done():
		}
	})
	return seeder.base.Seed(ctx, request)
}

func TestProjectAcquireSerializesPurgeAndPreventsCredentialResurrection(t *testing.T) {
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil { t.Fatal(err) }
	project := Project{ID: model.ProjectID("abababababababababab"), Harness: harness.Codex}
	source := t.TempDir()
	writePrivateArtifact(t, source, "auth.json", "canonical")
	if _, err := repository.ImportProject(context.Background(), project, source, false, testAuthSeeder); err != nil { t.Fatal(err) }

	workspace := Workspace{ProjectID: project.ID, Name: "feature", Harness: project.Harness}
	seeder := &barrierAuthSeeder{base: testAuthSeeder, entered: make(chan struct{}), release: make(chan struct{})}
	acquired := make(chan WorkspaceCopy, 1)
	acquireErrors := make(chan error, 1)
	go func() {
		copy, acquireErr := repository.AcquireWorkspace(context.Background(), workspace, model.RunID("00000000-0000-7000-8000-000000000109"), seeder)
		acquired <- copy
		acquireErrors <- acquireErr
	}()
	select {
	case <-seeder.entered:
	case <-time.After(time.Second):
		t.Fatal("workspace acquisition did not reach the post-lease seed barrier")
	}

	purged := make(chan error, 1)
	go func() { purged <- repository.PurgeProject(context.Background(), project) }()
	select {
	case purgeErr := <-purged:
		t.Fatalf("purge completed while workspace acquisition held authority: %v", purgeErr)
	case <-time.After(50 * time.Millisecond):
	}
	close(seeder.release)
	copy := <-acquired
	if err := <-acquireErrors; err != nil { t.Fatal(err) }
	if err := <-purged; !errors.Is(err, ErrActiveCopies) {
		t.Fatalf("purge after acquisition = %v, want active-copies refusal", err)
	}

	writeCredential(t, copy.Root, "promoted")
	if promotion, err := repository.PromoteWorkspace(context.Background(), copy, testAuthSeeder); err != nil || promotion.Conflict {
		t.Fatalf("active workspace promotion = %#v, %v", promotion, err)
	}
	if err := repository.ReleaseWorkspace(context.Background(), copy); err != nil { t.Fatal(err) }
	if err := repository.PurgeProject(context.Background(), project); err != nil { t.Fatal(err) }
	if _, err := repository.PromoteWorkspace(context.Background(), copy, testAuthSeeder); err == nil {
		t.Fatal("released workspace copy promoted after acknowledged purge")
	}
	status, err := repository.ProjectStatus(context.Background(), project, testAuthSeeder)
	if err != nil { t.Fatal(err) }
	if status.Configured {
		t.Fatal("canonical credentials resurrected after purge")
	}
}

func TestProjectPurgeBlockedByActiveWorkspaceAndCopyPersists(t *testing.T) {
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth")); if err != nil { t.Fatal(err) }
	project := Project{ID: model.ProjectID("bbbbbbbbbbbbbbbbbbbb"), Harness: harness.Codex}
	source := t.TempDir(); writePrivateArtifact(t, source, "auth.json", "seed")
	if _, err := repository.ImportProject(context.Background(), project, source, false, testAuthSeeder); err != nil { t.Fatal(err) }
	workspace := Workspace{ProjectID: project.ID, Name: "feature", Harness: project.Harness}
	copy, err := repository.AcquireWorkspace(context.Background(), workspace, model.RunID("00000000-0000-7000-8000-000000000103"), testAuthSeeder); if err != nil { t.Fatal(err) }
	if _, err := repository.AcquireWorkspace(context.Background(), workspace, model.RunID("00000000-0000-7000-8000-000000000104"), testAuthSeeder); !errors.Is(err, ErrActiveCopies) { t.Fatalf("second active copy error = %v", err) }
	if err := repository.PurgeProject(context.Background(), project); !errors.Is(err, ErrActiveCopies) { t.Fatalf("active purge error = %v", err) }
	writeCredential(t, copy.Root, "persisted")
	if _, err := repository.PromoteWorkspace(context.Background(), copy, testAuthSeeder); err != nil { t.Fatal(err) }
	if err := repository.ReleaseWorkspace(context.Background(), copy); err != nil { t.Fatal(err) }
	next, err := repository.AcquireWorkspace(context.Background(), workspace, model.RunID("00000000-0000-7000-8000-000000000105"), testAuthSeeder); if err != nil { t.Fatal(err) }
	assertCredential(t, next.Root, "persisted")
	if err := repository.ReleaseWorkspace(context.Background(), next); err != nil { t.Fatal(err) }
}

func TestHostDiscoveryExactAllowlistsAndRestrictivePersistence(t *testing.T) {
	home := t.TempDir(); codexRoot := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexRoot, 0o700); err != nil { t.Fatal(err) }
	writePrivateArtifact(t, codexRoot, "auth.json", `{"token":"portable"}`)
	writePrivateArtifact(t, codexRoot, "config.toml", "must-not-copy")
	discovery, err := NewHostDiscovery(home); if err != nil { t.Fatal(err) }
	if got := discovery.Status(context.Background(), harness.Codex, 1<<20); got != HostImportAvailable { t.Fatalf("Codex status = %q", got) }
	if got := discovery.Status(context.Background(), harness.Claude, 1<<20); got != HostImportLoginRequired { t.Fatalf("Claude status = %q", got) }
	if _, err := discovery.Discover(harness.Claude); err == nil { t.Fatal("Claude host state was exposed as portable") }

	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth")); if err != nil { t.Fatal(err) }
	project := Project{ID: model.ProjectID("cccccccccccccccccccc"), Harness: harness.Codex}
	hostSource, err := discovery.Discover(harness.Codex); if err != nil { t.Fatal(err) }
	digest, err := repository.ImportHost(context.Background(), project, hostSource, false, testAuthSeeder); if err != nil { t.Fatal(err) }
	generation := repository.generationRoot(projectProfile(project), digest)
	info, err := os.Stat(filepath.Join(generation, "auth.json")); if err != nil { t.Fatal(err) }
	if info.Mode().Perm() != 0o600 { t.Fatalf("canonical artifact mode = %#o", info.Mode().Perm()) }
	if _, err := os.Stat(filepath.Join(generation, "config.toml")); !errors.Is(err, os.ErrNotExist) { t.Fatalf("adjacent Codex config was imported: %v", err) }
	parentInfo, err := os.Stat(generation); if err != nil { t.Fatal(err) }
	if parentInfo.Mode().Perm()&0o077 != 0 { t.Fatalf("canonical directory mode = %#o", parentInfo.Mode().Perm()) }
}

func TestHostDiscoveryRejectsSymlinksSpecialFilesAndSizeLimits(t *testing.T) {
	tests := []struct { name string; setup func(*testing.T, string) }{
		{name: "symlink", setup: func(t *testing.T, root string) { external := filepath.Join(t.TempDir(), "outside"); writePrivateArtifact(t, filepath.Dir(external), filepath.Base(external), "secret-symlink-value"); if err := os.Symlink(external, filepath.Join(root, "auth.json")); err != nil { t.Fatal(err) } }},
		{name: "fifo", setup: func(t *testing.T, root string) { if err := unix.Mkfifo(filepath.Join(root, "auth.json"), 0o600); err != nil { t.Fatal(err) } }},
		{name: "oversized", setup: func(t *testing.T, root string) { writePrivateArtifact(t, root, "auth.json", strings.Repeat("secret-size-value", 128)) }},
	}
	for _, test := range tests { t.Run(test.name, func(t *testing.T) {
		home := t.TempDir(); root := filepath.Join(home, ".codex"); if err := os.MkdirAll(root, 0o700); err != nil { t.Fatal(err) }; test.setup(t, root)
		discovery, err := NewHostDiscovery(home); if err != nil { t.Fatal(err) }
		if got := discovery.Status(context.Background(), harness.Codex, 64); got != HostImportInvalid { t.Fatalf("status = %q", got) }
		hostSource, err := discovery.Discover(harness.Codex); if err != nil { t.Fatal(err) }
		if err := hostSource.validate(context.Background(), 64); err == nil { t.Fatal("unsafe host artifact validated") } else if strings.Contains(err.Error(), "secret-") { t.Fatalf("error leaked secret content: %v", err) }
	}) }
}

func TestOMPHostImportRejectsActiveAndIncoherentWAL(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sqlite3"); err != nil { t.Skip("/usr/bin/sqlite3 is required by the pinned OMP snapshot validator") }
	home := t.TempDir(); root := filepath.Join(home, ".omp", "agent"); if err := os.MkdirAll(root, 0o700); err != nil { t.Fatal(err) }
	const schema = "PRAGMA journal_mode=DELETE;" + "CREATE TABLE auth_schema_version (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL);" + "INSERT INTO auth_schema_version(id, version) VALUES (1, 7);" + "CREATE TABLE auth_credentials (id INTEGER PRIMARY KEY AUTOINCREMENT, provider TEXT NOT NULL, credential_type TEXT NOT NULL, data TEXT NOT NULL, disabled_cause TEXT DEFAULT NULL, identity_key TEXT DEFAULT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);"
	command := exec.Command("/usr/bin/sqlite3", "-batch", filepath.Join(root, "agent.db"), schema)
	if output, err := command.CombinedOutput(); err != nil { t.Fatalf("create OMP fixture: %v: %s", err, output) }
	if err := os.Chmod(filepath.Join(root, "agent.db"), 0o600); err != nil { t.Fatal(err) }
	discovery, err := NewHostDiscovery(home); if err != nil { t.Fatal(err) }; hostSource, err := discovery.Discover(harness.OMP); if err != nil { t.Fatal(err) }
	repository, err := NewRepository(filepath.Join(t.TempDir(), "auth")); if err != nil { t.Fatal(err) }
	project := Project{ID: model.ProjectID("dddddddddddddddddddd"), Harness: harness.OMP}
	if err := os.WriteFile(filepath.Join(root, "agent.db-shm"), []byte("active"), 0o600); err != nil { t.Fatal(err) }
	if _, err := repository.ImportHost(context.Background(), project, hostSource, false, omp.New()); err == nil { t.Fatal("active OMP database was imported") }
	if err := os.Remove(filepath.Join(root, "agent.db-shm")); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(root, "agent.db-wal"), make([]byte, 32), 0o600); err != nil { t.Fatal(err) }
	if _, err := repository.ImportHost(context.Background(), project, hostSource, false, omp.New()); err == nil { t.Fatal("incoherent OMP WAL was imported") }
	if err := os.Remove(filepath.Join(root, "agent.db-wal")); err != nil { t.Fatal(err) }
	if _, err := repository.ImportHost(context.Background(), project, hostSource, false, omp.New()); err != nil { t.Fatal(err) }
}
