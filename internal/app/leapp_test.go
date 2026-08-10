package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/bridge"
	projectinspect "github.com/srimajji/dsx/internal/inspect"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

type fakeLeappMirrorManager struct {
	path        string
	ensures     []bridge.LeaseIdentity
	authorities []bridge.LeappAuthority
	stops       []bridge.LeaseIdentity
	status      bridge.LeappMirrorStatus
}

func (manager *fakeLeappMirrorManager) Ensure(_ context.Context, identity bridge.LeaseIdentity, authority bridge.LeappAuthority) (string, error) {
	manager.ensures = append(manager.ensures, identity)
	manager.authorities = append(manager.authorities, authority)
	return manager.path, nil
}

func (manager *fakeLeappMirrorManager) Path(bridge.LeaseIdentity) (string, error) {
	return manager.path, nil
}

func (manager *fakeLeappMirrorManager) Stop(_ context.Context, identity bridge.LeaseIdentity) error {
	manager.stops = append(manager.stops, identity)
	return nil
}

func (manager *fakeLeappMirrorManager) Status(context.Context, bridge.LeaseIdentity) (bridge.LeappMirrorStatus, error) {
	return manager.status, nil
}

func TestLiveLeappMirrorIsMountedAndStartupRollbackStopsHelper(t *testing.T) {
	service, root, _, fakeRuntime, _ := lifecycleFixture(t)
	awsDirectory := canonicalAppTemporaryDirectory(t)
	writeAppLeappFiles(t, awsDirectory, "[profile default]\n", "[default]\naws_access_key_id=must-not-appear\n")
	configuration := `{"schemaVersion":1,"workspace":{"root":"."},"image":{"ref":"ghcr.io/example/dev@sha256:` + strings.Repeat("a", 64) + `"},"aws":{"mode":"leapp","directory":"` + filepath.ToSlash(awsDirectory) + `","profile":"default"}}`
	if err := os.WriteFile(filepath.Join(root, ".dsx", "config.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	mirror := &fakeLeappMirrorManager{path: filepath.Join(canonicalAppTemporaryDirectory(t), "mirror")}
	service.leappMirrors = mirror
	fakeRuntime.failStart = errors.New("start failed")
	hash := inspectLifecycleHash(t, service.inspection, root)
	_, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: hash, FinalConfirmed: true})
	if err == nil {
		t.Fatal("startup unexpectedly succeeded")
	}
	if len(mirror.ensures) < 2 {
		t.Fatalf("mirror ensure calls = %#v", mirror.ensures)
	}
	if len(mirror.stops) == 0 || mirror.stops[len(mirror.stops)-1] != mirror.ensures[0] {
		t.Fatalf("mirror cleanup identities = %#v, ensures %#v", mirror.stops, mirror.ensures)
	}
	var awsMount *runtime.Mount
	for index := range fakeRuntime.workspaceSpec.Mounts {
		if fakeRuntime.workspaceSpec.Mounts[index].Target == bridge.LeappGuestDirectory {
			awsMount = &fakeRuntime.workspaceSpec.Mounts[index]
		}
	}
	if awsMount == nil || awsMount.Source != mirror.path || !awsMount.ReadOnly || awsMount.Source == awsDirectory {
		t.Fatalf("workspace Leapp mount = %#v", awsMount)
	}
	if strings.Contains(err.Error(), "must-not-appear") {
		t.Fatalf("startup error exposed credential contents: %v", err)
	}
}

func TestLeappAuthorityWarningHashAndWorkspaceSpecs(t *testing.T) {
	root := canonicalAppTemporaryDirectory(t)
	awsDirectory := canonicalAppTemporaryDirectory(t)
	writeAppLeappFiles(t, awsDirectory, "[profile engineering]\nregion=eu-west-1\n", "[engineering]\naws_access_key_id=never-print-key\n")
	if err := os.Mkdir(filepath.Join(root, ".dsx"), 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := `{
  "schemaVersion": 1,
  "workspace": {"root": "."},
  "image": {"ref": "ghcr.io/example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "aws": {"mode": "leapp", "directory": "` + filepath.ToSlash(awsDirectory) + `", "profile": "engineering"}
}`
	if err := os.WriteFile(filepath.Join(root, ".dsx", "config.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewInspectionServiceWithDependencies(InspectionDependencies{
		InspectProject: func(string) (projectinspect.Facts, error) {
			return projectinspect.Facts{WorkspaceRoot: root, GitRoots: []string{"."}}, nil
		},
		Resolver: plan.NewResolver(),
	})
	first, err := service.Inspect(context.Background(), InspectRequest{Root: root})
	if err != nil {
		t.Fatalf("inspect Leapp plan: %v (diagnostics %#v)", err, first.Diagnostics)
	}
	aws := appAWSGrant(t, first.Plan)
	if aws.Destination != awsDirectory || aws.SourceIdentity == "" || !aws.ReadOnly {
		t.Fatalf("AWS plan grant = %#v", aws)
	}
	warningText := ""
	for _, diagnostic := range first.Diagnostics {
		if diagnostic.Code == "aws_all_profiles_readable" {
			warningText = diagnostic.Message
		}
	}
	if !strings.Contains(warningText, "every profile") || !strings.Contains(warningText, "not credential isolation") {
		t.Fatalf("all-profile warning = %q", warningText)
	}
	if strings.Contains(warningText, "never-print-key") {
		t.Fatalf("warning exposed credential contents: %q", warningText)
	}

	first.Plan.Sandbox.RunID = "01890f5c-7b00-7000-8000-000000000031"
	record := state.ResourceRecord{Name: "dsx-workspace"}
	helper := runtime.HostPath(filepath.Join(canonicalAppTemporaryDirectory(t), "dsx-guest"))
	mirrorSource := appLeappMirrorSource(first.Plan)
	live, err := workspaceSpecForPlan(first.Plan, runtime.Image{}, record, "dsx-network", nil, nil, "1000:1000", helper, mirrorSource)
	if err != nil {
		t.Fatal(err)
	}
	assertAppLeappWorkspaceGrant(t, live, mirrorSource, "engineering")

	cloneVolumes := map[string]string{cloneWorkspaceVolume: "dsx-clone-workspace"}
	clone, err := workspaceSpecForClone(first.Plan, runtime.Image{}, record, "dsx-network", cloneVolumes, nil, "1000:1000", helper, mirrorSource)
	if err != nil {
		t.Fatal(err)
	}
	assertAppLeappWorkspaceGrant(t, clone, mirrorSource, "engineering")

	original := awsDirectory + ".approved"
	if err := os.Rename(awsDirectory, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(awsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeAppLeappFiles(t, awsDirectory, "[profile engineering]\nregion=us-east-1\n", "[engineering]\naws_access_key_id=replacement-secret\n")
	second, err := service.Inspect(context.Background(), InspectRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if second.Plan.ExecutableHash == first.Plan.ExecutableHash {
		t.Fatalf("replacement directory retained executable approval hash %q", first.Plan.ExecutableHash)
	}
	if appAWSGrant(t, second.Plan).SourceIdentity == aws.SourceIdentity {
		t.Fatalf("replacement directory retained source identity %q", aws.SourceIdentity)
	}
}

func TestNoAWSGrantHasNoWorkspaceMountOrEnvironment(t *testing.T) {
	execution := plan.ExecutionPlan{}
	execution.Project.ID = "aaaaaaaaaaaaaaaaaaaa"
	execution.Sandbox.Name = "main"
	execution.Sandbox.RunID = "01890f5c-7b00-7000-8000-000000000032"
	spec, err := workspaceSpecForPlan(
		execution,
		runtime.Image{},
		state.ResourceRecord{Name: "dsx-workspace"},
		"dsx-network",
		nil,
		nil,
		"1000:1000",
		runtime.HostPath(filepath.Join(canonicalAppTemporaryDirectory(t), "dsx-guest")),
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, mount := range spec.Mounts {
		if mount.Target == "/run/dsx/aws" {
			t.Fatalf("AWS mount appeared without grant: %#v", spec.Mounts)
		}
	}
	for _, entry := range spec.Env {
		if strings.HasPrefix(entry, "AWS_") {
			t.Fatalf("AWS environment appeared without grant: %#v", spec.Env)
		}
	}
}

func TestLiveLeappWorkspaceRejectsProjectAWSDirectory(t *testing.T) {
	projectRoot := canonicalAppTemporaryDirectory(t)
	awsDirectory := filepath.Join(projectRoot, ".aws")
	if err := os.Mkdir(awsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeAppLeappFiles(t, awsDirectory, "[profile default]\n", "[default]\naws_access_key_id=credential-content-must-not-appear\n")
	execution := appLeappIsolationPlan(t, awsDirectory)
	execution.Project.CanonicalRoot = projectRoot
	execution.Repositories = []plan.RepositoryPlan{{Name: "workspace", HostPath: projectRoot, GuestPath: "/workspace"}}

	_, err := appLeappLiveSpec(t, execution)
	assertAppLeappIsolationError(t, err)
}

func TestLeappWorkspaceSpecsRejectWritableHostSourceAliases(t *testing.T) {
	for _, test := range []struct {
		name   string
		source func(*testing.T, string) string
	}{
		{
			name: "writable parent",
			source: func(_ *testing.T, awsDirectory string) string {
				return filepath.Dir(awsDirectory)
			},
		},
		{
			name: "writable descendant",
			source: func(_ *testing.T, awsDirectory string) string {
				return filepath.Join(awsDirectory, "credentials")
			},
		},
		{
			name: "symlink alias",
			source: func(t *testing.T, awsDirectory string) string {
				alias := filepath.Join(canonicalAppTemporaryDirectory(t), "aws-alias")
				if err := os.Symlink(awsDirectory, alias); err != nil {
					t.Fatal(err)
				}
				return alias
			},
		},
		{
			name: "same inode alias",
			source: func(t *testing.T, awsDirectory string) string {
				writable := canonicalAppTemporaryDirectory(t)
				if err := os.Link(filepath.Join(awsDirectory, "credentials"), filepath.Join(writable, "credentials-alias")); err != nil {
					t.Fatal(err)
				}
				return writable
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := canonicalAppTemporaryDirectory(t)
			awsDirectory := filepath.Join(parent, "aws")
			if err := os.Mkdir(awsDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			writeAppLeappFiles(t, awsDirectory, "[profile default]\n", "[default]\naws_access_key_id=credential-content-must-not-appear\n")
			execution := appLeappIsolationPlan(t, awsDirectory)
			execution.Mounts = []plan.ResolvedMount{{SourceType: "host", Source: test.source(t, awsDirectory), Target: "/data"}}

			_, liveErr := appLeappLiveSpec(t, execution)
			assertAppLeappIsolationError(t, liveErr)
			_, cloneErr := appLeappCloneSpec(t, execution)
			assertAppLeappIsolationError(t, cloneErr)
		})
	}
}

func TestLeappWorkspaceSpecsAllowDisjointWritableProject(t *testing.T) {
	awsDirectory := canonicalAppTemporaryDirectory(t)
	writeAppLeappFiles(t, awsDirectory, "[profile default]\n", "[default]\naws_access_key_id=credential-content-must-not-appear\n")
	execution := appLeappIsolationPlan(t, awsDirectory)
	projectRoot := canonicalAppTemporaryDirectory(t)
	execution.Project.CanonicalRoot = projectRoot
	execution.Repositories = []plan.RepositoryPlan{{Name: "workspace", HostPath: projectRoot, GuestPath: "/workspace"}}

	live, err := appLeappLiveSpec(t, execution)
	if err != nil {
		t.Fatalf("disjoint live workspace rejected: %v", err)
	}
	assertAppLeappWorkspaceGrant(t, live, appLeappMirrorSource(execution), "default")
	clone, err := appLeappCloneSpec(t, execution)
	if err != nil {
		t.Fatalf("disjoint clone workspace rejected: %v", err)
	}
	assertAppLeappWorkspaceGrant(t, clone, appLeappMirrorSource(execution), "default")
}

func appLeappIsolationPlan(t *testing.T, awsDirectory string) plan.ExecutionPlan {
	t.Helper()
	authority, err := bridge.ResolveLeappDirectory(awsDirectory)
	if err != nil {
		t.Fatal(err)
	}
	execution := plan.ExecutionPlan{}
	execution.Project.ID = "aaaaaaaaaaaaaaaaaaaa"
	execution.Sandbox.Name = "main"
	execution.Sandbox.RunID = "01890f5c-7b00-7000-8000-000000000033"
	execution.Bridges = []plan.BridgeGrant{{
		Kind:           "aws",
		Name:           "default",
		Destination:    authority.CanonicalPath,
		SourceIdentity: authority.Identity,
		ReadOnly:       true,
	}}
	return execution
}

func appLeappLiveSpec(t *testing.T, execution plan.ExecutionPlan) (runtime.WorkspaceSpec, error) {
	t.Helper()
	return workspaceSpecForPlan(execution, runtime.Image{}, state.ResourceRecord{Name: "dsx-workspace"}, "dsx-network", nil, nil, "1000:1000", runtime.HostPath(filepath.Join(canonicalAppTemporaryDirectory(t), "dsx-guest")), appLeappMirrorSource(execution))
}

func appLeappCloneSpec(t *testing.T, execution plan.ExecutionPlan) (runtime.WorkspaceSpec, error) {
	t.Helper()
	return workspaceSpecForClone(execution, runtime.Image{}, state.ResourceRecord{Name: "dsx-workspace"}, "dsx-network", map[string]string{cloneWorkspaceVolume: "dsx-clone-workspace"}, nil, "1000:1000", runtime.HostPath(filepath.Join(canonicalAppTemporaryDirectory(t), "dsx-guest")), appLeappMirrorSource(execution))
}

func appLeappMirrorSource(execution plan.ExecutionPlan) string {
	for _, grant := range execution.Bridges {
		if grant.Kind == "aws" {
			return filepath.Join(filepath.Dir(grant.Destination), ".dsx-leapp-mirror-"+string(execution.Sandbox.RunID))
		}
	}
	return ""
}

func assertAppLeappIsolationError(t *testing.T, err error) {
	t.Helper()
	if err == nil || model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("workspace isolation error = %v", err)
	}
	if strings.Contains(err.Error(), "credential-content-must-not-appear") {
		t.Fatalf("workspace isolation error exposed credential contents: %v", err)
	}
}

func appAWSGrant(t *testing.T, execution plan.ExecutionPlan) plan.BridgeGrant {
	t.Helper()
	var found *plan.BridgeGrant
	for index := range execution.Bridges {
		if execution.Bridges[index].Kind != "aws" {
			continue
		}
		if found != nil {
			t.Fatal("multiple AWS grants")
		}
		found = &execution.Bridges[index]
	}
	if found == nil {
		t.Fatalf("AWS grant missing from %#v", execution.Bridges)
	}
	return *found
}

func assertAppLeappWorkspaceGrant(t *testing.T, spec runtime.WorkspaceSpec, source, profile string) {
	t.Helper()
	var awsMounts []runtime.Mount
	for _, mount := range spec.Mounts {
		if mount.Target == "/run/dsx/aws" {
			awsMounts = append(awsMounts, mount)
		}
	}
	wantMounts := []runtime.Mount{{Source: source, Target: "/run/dsx/aws", Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityLeappMirror}}
	if !reflect.DeepEqual(awsMounts, wantMounts) {
		t.Fatalf("AWS mounts = %#v, want %#v", awsMounts, wantMounts)
	}
	wantEnvironment := []string{
		"AWS_CONFIG_FILE=/run/dsx/aws/current/config",
		"AWS_SHARED_CREDENTIALS_FILE=/run/dsx/aws/current/credentials",
		"AWS_PROFILE=" + profile,
	}
	for _, expected := range wantEnvironment {
		found := false
		for _, entry := range spec.Env {
			found = found || entry == expected
		}
		if !found {
			t.Fatalf("workspace environment %#v lacks %q", spec.Env, expected)
		}
	}
}

func canonicalAppTemporaryDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(directory)
}

func writeAppLeappFiles(t *testing.T, directory, config, credentials string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "credentials"), []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
}
