package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/buildinfo"
	projectinspect "github.com/srimajji/dsx/internal/inspect"
	"github.com/srimajji/dsx/internal/plan"
)

func TestInspectConfiguredProjectBuildsHashedPlan(t *testing.T) {
	root := t.TempDir()
	configDirectory := filepath.Join(root, ".dsx")
	if err := os.Mkdir(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := `{
  // comments are permitted
  "schemaVersion": 1,
  "workspace": {"root": "."},
  "image": {"ref": "ghcr.io/example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "agents": {"default": "codex", "allowed": ["codex"]}
}
`
	if err := os.WriteFile(filepath.Join(configDirectory, "config.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}

	service := NewInspectionServiceWithDependencies(InspectionDependencies{
		InspectProject: func(string) (projectinspect.Facts, error) {
			return projectinspect.Facts{WorkspaceRoot: root, GitRoots: []string{"."}}, nil
		},
		Resolver: plan.NewResolver(),
	})
	result, err := service.Inspect(context.Background(), InspectRequest{Root: root})
	if err != nil {
		t.Fatalf("inspect: %v, diagnostics: %#v", err, result.Diagnostics)
	}
	if !result.Facts.ConfigExists || result.Plan.Image.Reference == "" || result.Plan.Agents.Default != "codex" {
		t.Fatalf("incomplete result: %#v", result)
	}
	if len(result.Plan.ExecutableHash) != 64 || result.Plan.Provenance["/agents/default"].Kind != "project" {
		t.Fatalf("hash/provenance missing: %#v", result.Plan)
	}
	if result.Plan.Browser == nil || result.Plan.Browser.ImageReference != standardBrowserImageReference || result.Plan.Browser.ImageDigest != standardBrowserImageDigest {
		t.Fatalf("browser authority missing: %#v", result.Plan.Browser)
	}
}

func TestBrowserImageAuthorityUsesPublishedReleasePinAndFailsClosed(t *testing.T) {
	previousVersion, previousBrowser := buildinfo.Version, buildinfo.BrowserImage
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.BrowserImage = previousVersion, previousBrowser
	})
	buildinfo.Version = "1.2.3"
	buildinfo.BrowserImage = "ghcr.io/srimajji/dsx-browser@sha256:" + strings.Repeat("b", 64)
	reference, digest, err := browserImageAuthority()
	if err != nil {
		t.Fatal(err)
	}
	if reference != buildinfo.BrowserImage || digest != strings.Repeat("b", 64) {
		t.Fatalf("browser authority = %q %q", reference, digest)
	}
	for name, value := range map[string]string{
		"missing":  "unknown",
		"local":    "dsx.local/browser:mvp@sha256:" + strings.Repeat("c", 64),
		"unpinned": "ghcr.io/srimajji/dsx-browser:latest",
	} {
		t.Run(name, func(t *testing.T) {
			buildinfo.BrowserImage = value
			if _, _, err := browserImageAuthority(); err == nil {
				t.Fatalf("release browser image %q was accepted", value)
			}
		})
	}
}

func TestInspectWithoutConfigDoesNotInventPlan(t *testing.T) {
	root := t.TempDir()
	service := NewInspectionServiceWithDependencies(InspectionDependencies{
		InspectProject: func(string) (projectinspect.Facts, error) {
			return projectinspect.Facts{WorkspaceRoot: root, Containerfiles: []string{"Dockerfile"}}, nil
		},
		Resolver: plan.NewResolver(),
	})
	result, err := service.Inspect(context.Background(), InspectRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan.ContractVersion != "" || result.Facts.ConfigExists {
		t.Fatalf("invented executable plan: %#v", result.Plan)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "incomplete_plan" || !strings.Contains(result.Diagnostics[0].Message, "no executable plan") {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
}

func TestHashBuildInputBytesChangePlan(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".dsx"), 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := []byte(`{
  "schemaVersion": 1,
  "workspace": {"root": "."},
  "image": {"build": {"context": ".", "file": "Dockerfile"}},
  "agents": {"default": "codex", "allowed": ["codex"]}
}`)
	if err := os.WriteFile(filepath.Join(root, ".dsx", "config.jsonc"), configuration, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewInspectionService(plan.NewResolver())
	first, err := service.Inspect(context.Background(), InspectRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\nLABEL changed=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := service.Inspect(context.Background(), InspectRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if first.Plan.ExecutableHash == second.Plan.ExecutableHash || first.Plan.Image.InputDigest == second.Plan.Image.InputDigest {
		t.Fatalf("Dockerfile byte change did not alter build digest/hash: first=%s second=%s", first.Plan.Image.InputDigest, second.Plan.Image.InputDigest)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := service.Inspect(context.Background(), InspectRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if second.Plan.ExecutableHash == third.Plan.ExecutableHash || second.Plan.Image.InputDigest == third.Plan.Image.InputDigest {
		t.Fatalf("context byte change did not alter build digest/hash")
	}
}

func TestResolveHostMountRejectsNonstandardCurrentHome(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	source := filepath.Join(home, "credentials")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveHostMount(source); err == nil || !strings.Contains(err.Error(), "current user home") {
		t.Fatalf("resolveHostMount(%q) error = %v, want home-policy refusal", source, err)
	}
}
