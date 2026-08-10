package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRepositoryIdentityRejectsAncestorAndGitDirectoryReplacement(t *testing.T) {
	t.Run("unchanged repository", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		artifact := prepareSourceTest(t, &fixture, "identity")
		defer fixture.service.RemoveArtifact(artifact.BundlePath)
		if err := fixture.service.ValidateRepository(context.Background(), artifact.Repository); err != nil {
			t.Fatalf("ValidateRepository() error = %v", err)
		}
		assertNoPrivateSourceRefs(t, fixture.path)
	})

	t.Run("ancestor replaced by symlink", func(t *testing.T) {
		approvedRoot, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		repositoryPath := filepath.Join(approvedRoot, "repository")
		fixture := newRepositoryAt(t, repositoryPath)
		artifact := prepareSourceAt(t, &fixture, approvedRoot)
		if err := fixture.service.RemoveArtifact(artifact.BundlePath); err != nil {
			t.Fatal(err)
		}
		movedRoot := approvedRoot + "-moved"
		if err := os.Rename(approvedRoot, movedRoot); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(movedRoot, approvedRoot); err != nil {
			t.Fatal(err)
		}
		if err := fixture.service.ValidateRepository(context.Background(), artifact.Repository); err == nil {
			t.Fatal("ValidateRepository accepted a replaced symlink ancestor")
		}
		if err := os.Remove(approvedRoot); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(movedRoot, approvedRoot); err != nil {
			t.Fatal(err)
		}
		assertNoPrivateSourceRefs(t, repositoryPath)
	})

	t.Run("worktree replaced by new directory", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		artifact := prepareSourceTest(t, &fixture, "identity")
		if err := fixture.service.RemoveArtifact(artifact.BundlePath); err != nil {
			t.Fatal(err)
		}
		oldPath := fixture.path + "-old"
		if err := os.Rename(fixture.path, oldPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(fixture.path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := fixture.service.ValidateRepository(context.Background(), artifact.Repository); err == nil {
			t.Fatal("ValidateRepository accepted a replacement worktree directory")
		}
		if err := os.Remove(fixture.path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(oldPath, fixture.path); err != nil {
			t.Fatal(err)
		}
		assertNoPrivateSourceRefs(t, fixture.path)
	})

	t.Run("git directory retargeted", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		artifact := prepareSourceTest(t, &fixture, "identity")
		if err := fixture.service.RemoveArtifact(artifact.BundlePath); err != nil {
			t.Fatal(err)
		}
		gitDir := filepath.Join(fixture.path, ".git")
		original := filepath.Join(fixture.path, ".git-original")
		retarget := filepath.Join(fixture.path, ".git-retarget")
		if err := os.Rename(gitDir, original); err != nil {
			t.Fatal(err)
		}
		gitTest(t, "", "init", "--bare", "--quiet", retarget)
		writeFile(t, gitDir, "gitdir: .git-retarget\n")
		if err := fixture.service.ValidateRepository(context.Background(), artifact.Repository); err == nil {
			t.Fatal("ValidateRepository accepted a retargeted Git directory")
		}
		if err := os.Remove(gitDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(original, gitDir); err != nil {
			t.Fatal(err)
		}
		assertNoPrivateSourceRefs(t, fixture.path)
	})
}

func TestPrepareSourceRejectsCompositeSiblingEscapeWithoutMutation(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	approved := filepath.Join(parent, "project")
	sibling := filepath.Join(parent, "project-sibling")
	if err := os.Mkdir(approved, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := newRepositoryAt(t, sibling)
	before := gitTest(t, fixture.path, "for-each-ref", "--format=%(refname)%00%(objectname)")
	_, err = fixture.service.PrepareSource(context.Background(), SourceRequest{
		Repository: fixture.repository(), ApprovedRoot: approved, Sandbox: "sibling", TempRoot: t.TempDir(),
	})
	if err == nil {
		t.Fatal("PrepareSource accepted a sibling repository outside the approved root")
	}
	after := gitTest(t, fixture.path, "for-each-ref", "--format=%(refname)%00%(objectname)")
	if after != before {
		t.Fatalf("repository refs mutated on rejected sibling: before %q after %q", before, after)
	}
}

func TestPrepareSourceUsesExactPrivateRefAndExcludesOtherObjects(t *testing.T) {
	fixture := newRepositoryWithCommit(t)
	tree := strings.TrimSpace(gitTest(t, fixture.path, "write-tree"))
	extraCommit := strings.TrimSpace(gitTest(t, fixture.path, "commit-tree", tree, "-m", "unrelated"))
	gitTest(t, fixture.path, "update-ref", "refs/heads/unrelated", extraCommit)

	artifact := prepareSourceTest(t, &fixture, "exact")
	defer fixture.service.RemoveArtifact(artifact.BundlePath)
	heads := strings.Fields(gitTest(t, fixture.path, "bundle", "list-heads", artifact.BundlePath))
	if len(heads) != 2 || heads[0] != artifact.SourceCommit || heads[1] != artifact.BundleRef || !strings.HasPrefix(heads[1], "refs/dsx/private/source/") {
		t.Fatalf("source bundle advertised heads = %#v", heads)
	}
	assertNoPrivateSourceRefs(t, fixture.path)

	guest := filepath.Join(t.TempDir(), "guest")
	gitTest(t, "", "init", "--quiet", guest)
	gitTest(t, guest, "fetch", "--no-tags", "--no-write-fetch-head", "--", artifact.BundlePath, artifact.BundleRef)
	command := exec.Command(testGitExecutable(t), "-C", guest, "cat-file", "-e", extraCommit+"^{commit}")
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if err := command.Run(); err == nil {
		t.Fatal("source bundle included an object reachable only from an unrelated ref")
	}
}

func TestPrepareSourceRejectsConcurrentSnapshotMutationAndCleansArtifacts(t *testing.T) {
	tests := map[string]func(*testing.T, repositoryFixture){
		"source ref advance": func(t *testing.T, fixture repositoryFixture) {
			gitTest(t, fixture.path, "commit", "--allow-empty", "-m", "racing advance")
		},
		"source ref force rewrite": func(t *testing.T, fixture repositoryFixture) {
			parent := strings.TrimSpace(gitTest(t, fixture.path, "rev-parse", "HEAD^"))
			gitTest(t, fixture.path, "update-ref", "refs/heads/main", parent)
		},
		"tracked dirty race": func(t *testing.T, fixture repositoryFixture) {
			writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "racing dirty state\n")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newRepositoryWithCommit(t)
			gitTest(t, fixture.path, "commit", "--allow-empty", "-m", "second source")
			runner := &afterBundleCreateRunner{delegate: OSRunner{}, after: func() { mutate(t, fixture) }}
			service, err := NewService(runner, testGitExecutable(t))
			if err != nil {
				t.Fatal(err)
			}
			fixture.service = service
			tempRoot := t.TempDir()
			_, err = service.PrepareSource(context.Background(), SourceRequest{
				Repository: fixture.repository(), ApprovedRoot: fixture.path, Sandbox: "race", TempRoot: tempRoot,
			})
			if err == nil {
				t.Fatal("PrepareSource accepted a repository mutation during bundle creation")
			}
			assertNoPrivateSourceRefs(t, fixture.path)
			entries, readErr := os.ReadDir(tempRoot)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("temporary source artifacts remain after failure: %#v", entries)
			}
		})
	}
}

type afterBundleCreateRunner struct {
	delegate Runner
	after    func()
	once     sync.Once
}

func (runner *afterBundleCreateRunner) Run(ctx context.Context, command Command) (Exit, error) {
	exit, err := runner.delegate.Run(ctx, command)
	if err == nil && exit.Code == 0 && exit.Signal == "" && containsArguments(command.Argv, "bundle", "create") {
		runner.once.Do(runner.after)
	}
	return exit, err
}

func containsArguments(arguments []string, first, second string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == first && arguments[index+1] == second {
			return true
		}
	}
	return false
}

func newRepositoryAt(t *testing.T, repositoryPath string) repositoryFixture {
	t.Helper()
	service, err := NewService(OSRunner{}, testGitExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, "", "init", "--quiet", "--initial-branch=main", repositoryPath)
	gitTest(t, repositoryPath, "config", "user.name", "DSX Test")
	gitTest(t, repositoryPath, "config", "user.email", "dsx@example.invalid")
	writeFile(t, filepath.Join(repositoryPath, "tracked.txt"), "source\n")
	gitTest(t, repositoryPath, "add", "tracked.txt")
	gitTest(t, repositoryPath, "commit", "-m", "source")
	return repositoryFixture{t: t, path: repositoryPath, service: service}
}

func prepareSourceAt(t *testing.T, fixture *repositoryFixture, approvedRoot string) SourceArtifact {
	t.Helper()
	artifact, err := fixture.service.PrepareSource(context.Background(), SourceRequest{
		Repository: fixture.repository(), ApprovedRoot: approvedRoot, Sandbox: "identity", TempRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.identity = artifact.Repository.Identity
	return artifact
}

func assertNoPrivateSourceRefs(t *testing.T, repositoryPath string) {
	t.Helper()
	if refs := strings.TrimSpace(gitTest(t, repositoryPath, "for-each-ref", "--format=%(refname)", "refs/dsx/private/source")); refs != "" {
		t.Fatalf("private source refs remain: %q", refs)
	}
}

var _ Runner = (*afterBundleCreateRunner)(nil)
