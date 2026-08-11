package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/buildinfo"
	projectinspect "github.com/srimajji/dsx/internal/inspect"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/ports"
	"github.com/srimajji/dsx/internal/runtime"
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
  "agents": {"default": "codex", "allowed": ["codex"]},
  "browser": {"enabled": true}
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
	if !result.Facts.ConfigExists || result.Plan.Image.Reference == "" || result.Plan.Agent != "codex" {
		t.Fatalf("incomplete result: %#v", result)
	}
	if len(result.Plan.ExecutableHash) != 64 || result.Plan.Provenance["/agent"].Kind != "project" {
		t.Fatalf("hash/provenance missing: %#v", result.Plan)
	}
	if result.Plan.Browser == nil || !result.Plan.Browser.Enabled || result.Plan.Browser.ImageReference != standardBrowserImageReference || result.Plan.Browser.ImageDigest != standardBrowserImageDigest {
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

func TestInspectClonePortResolutionIsDynamicAndHashed(t *testing.T) {
	root := t.TempDir()
	configDirectory := filepath.Join(root, ".dsx")
	if err := os.Mkdir(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := `{
  "schemaVersion": 1,
  "workspace": {"root": "."},
  "image": {"ref": "ghcr.io/example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "ports": [{"name": "web", "guest": 3000, "host": 3100, "protocol": "tcp", "bind": "127.0.0.1"}]
}`
	if err := os.WriteFile(filepath.Join(configDirectory, "config.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewInspectionServiceWithDependencies(InspectionDependencies{
		InspectProject: func(string) (projectinspect.Facts, error) {
			return projectinspect.Facts{WorkspaceRoot: root, GitRoots: []string{"."}}, nil
		},
		Resolver: plan.NewResolver(),
	})
	first, err := service.Inspect(context.Background(), InspectRequest{Root: root, Mode: string(model.ModeClone), SandboxName: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Inspect(context.Background(), InspectRequest{Root: root, Mode: string(model.ModeClone), SandboxName: "second"})
	if err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]InspectResult{"first": first, "second": second} {
		if len(result.Plan.Ports) != 1 || result.Plan.Ports[0].HostPort != nil || result.Plan.Ports[0].GuestPort != 3000 || result.Plan.Ports[0].Protocol != "tcp" || result.Plan.Ports[0].HostIP.String() != "127.0.0.1" {
			t.Fatalf("%s clone inspection port = %#v", name, result.Plan.Ports)
		}
	}
	if first.Plan.ExecutableHash != second.Plan.ExecutableHash {
		t.Fatal("sandbox display identity unexpectedly changed executable approval hash")
	}

	capabilities := runtime.Capabilities{FixedPublication: true, MachineReadableInspection: true}
	firstPublication, err := ports.Plan(first.Plan.Ports, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	defer firstPublication.Abort()
	secondPublication, err := ports.Plan(second.Plan.Ports, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	defer secondPublication.Abort()
	firstRequests, err := firstPublication.RequestedBindings()
	if err != nil {
		t.Fatal(err)
	}
	secondRequests, err := secondPublication.RequestedBindings()
	if err != nil {
		t.Fatal(err)
	}
	if firstRequests[0].HostPort == nil || secondRequests[0].HostPort == nil || *firstRequests[0].HostPort == *secondRequests[0].HostPort {
		t.Fatalf("clone publications cannot coexist: first=%#v second=%#v", firstRequests, secondRequests)
	}

	live, err := service.Inspect(context.Background(), InspectRequest{Root: root, Mode: string(model.ModeLive), SandboxName: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if live.Plan.Ports[0].HostPort == nil || *live.Plan.Ports[0].HostPort != 3100 {
		t.Fatalf("live inspection did not preserve fixed host port: %#v", live.Plan.Ports)
	}
	if live.Plan.ExecutableHash == first.Plan.ExecutableHash {
		t.Fatal("clone dynamic port transformation is absent from inspection hash")
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
  "image": {"build": {"context": ".", "file": "Dockerfile"}}
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

func TestHostMountSymlinkSwapRefused(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	mountPath := filepath.Join(canonicalRoot, "reviewed")
	if err := os.Mkdir(mountPath, 0o700); err != nil {
		t.Fatal(err)
	}
	rendered := []byte(fmt.Sprintf(`{
  "schemaVersion": 1,
  "workspace": {"root": "."},
  "image": {"ref": "example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "mounts": [{"source": {"type": "host", "path": %q}, "target": "/reviewed", "readOnly": true}]
}`, mountPath))
	service := NewSetupService(NewInspectionService(plan.NewResolver()), nil, nil)
	first, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: canonicalRoot, RenderedConfig: rendered})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Plan.Mounts) != 1 || first.Plan.Mounts[0].Source != mountPath || first.Plan.Mounts[0].SourceIdentity == "" {
		t.Fatalf("host mount authority missing from plan: %#v", first.Plan.Mounts)
	}
	original := mountPath + ".original"
	if err := os.Rename(mountPath, original); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home, mountPath); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PreviewSetup(context.Background(), SetupPreviewRequest{Root: canonicalRoot, RenderedConfig: rendered}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("post-preview symlink swap error = %v, want refusal", err)
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
