package bridge

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/ownership"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/runtime/apple"
)

func TestAppleHostAWSMirrorAtomicRotationReadOnlyMount(t *testing.T) {
	containerExecutable := os.Getenv("DSX_APPLE_CONTAINER")
	dsxExecutable := os.Getenv("DSX_HOST_AWS_MIRROR_DSX")
	imageReference := os.Getenv("DSX_APPLE_TEST_IMAGE")
	if containerExecutable == "" || dsxExecutable == "" || imageReference == "" {
		t.Skip("set DSX_APPLE_CONTAINER, DSX_HOST_AWS_MIRROR_DSX, and DSX_APPLE_TEST_IMAGE for the destructive opt-in Apple host AWS mirror test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	adapter, err := apple.NewAdapter(apple.OSRunner{}, containerExecutable)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := adapter.List(ctx, runtime.ResourceWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	baselineIDs := appleHostAWSMirrorResourceIDs(baseline)
	baselineNetworks, err := adapter.List(ctx, runtime.ResourceNetwork)
	if err != nil {
		t.Fatal(err)
	}
	baselineNetworkIDs := appleHostAWSMirrorResourceIDs(baselineNetworks)
	image, err := adapter.EnsureImage(ctx, runtime.ImageSpec{Reference: imageReference})
	if err != nil {
		t.Fatal(err)
	}

	stateRoot := canonicalTemporaryDirectory(t)
	source := hostAWSFixture(t, "[default]\nregion=eu-west-1\n[named]\nregion=us-east-1\n", hostAWSTemporaryCredentials("one")+"\n[named]\naws_access_key_id=named\naws_secret_access_key=named\naws_session_token=named\n")
	authority, err := ResolveHostAWSDirectory(source)
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := canonicalTemporaryDirectory(t)
	projectID, err := model.NewProjectID(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := model.ParseWorkspaceName("mirror-apple")
	if err != nil {
		t.Fatal(err)
	}
	identity := LeaseIdentity{ProjectID: projectID, CanonicalRoot: projectRoot, Workspace: workspace, RunID: model.RunID("01890f5c-7b00-7000-8000-000000000051")}
	manager, err := NewProductionHostAWSWorkspaceManager(stateRoot, dsxExecutable)
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := manager.Prepare(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}

	networkOwner, err := ownership.NewIdentity(identity.ProjectID, identity.CanonicalRoot, identity.Workspace, identity.RunID, runtime.ResourceNetwork, "network")
	if err != nil {
		t.Fatal(err)
	}
	createdNetwork, err := adapter.CreateNetwork(ctx, runtime.NetworkSpec{Name: networkOwner.Name(), Labels: networkOwner.Labels()})
	if err != nil {
		_ = manager.Remove(context.Background(), identity)
		t.Fatal(err)
	}
	networkSnapshot := runtime.ResourceSnapshot{Resource: createdNetwork, Labels: networkOwner.Labels()}
	volumeOwner, err := ownership.NewIdentity(identity.ProjectID, identity.CanonicalRoot, identity.Workspace, identity.RunID, runtime.ResourceVolume, "source")
	if err != nil {
		_ = adapter.Delete(context.Background(), networkSnapshot)
		_ = manager.Remove(context.Background(), identity)
		t.Fatal(err)
	}
	createdVolume, err := adapter.CreateVolume(ctx, runtime.VolumeSpec{Name: volumeOwner.Name(), Labels: volumeOwner.Labels()})
	if err != nil {
		_ = adapter.Delete(context.Background(), networkSnapshot)
		_ = manager.Remove(context.Background(), identity)
		t.Fatal(err)
	}
	volumeSnapshot := runtime.ResourceSnapshot{Resource: createdVolume, Labels: volumeOwner.Labels()}
	owner, err := ownership.NewIdentity(identity.ProjectID, identity.CanonicalRoot, identity.Workspace, identity.RunID, runtime.ResourceWorkspace, "workspace")
	if err != nil {
		_ = adapter.Delete(context.Background(), volumeSnapshot)
		_ = adapter.Delete(context.Background(), networkSnapshot)
		_ = manager.Remove(context.Background(), identity)
		t.Fatal(err)
	}
	created, err := adapter.CreateWorkspace(ctx, runtime.WorkspaceSpec{
		Name: owner.Name(), Image: image, Entrypoint: []string{"/bin/sh", "-lc", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"},
		HostAWSMirrorSource: runtime.HostPath(mirror),
		WorkingDir:          "/", User: "501:20", Mounts: []runtime.Mount{
			{Source: createdVolume.Name, Target: "/workspace", Type: "volume", Authority: runtime.MountAuthorityVolume},
			{Source: mirror, Target: HostAWSGuestDirectory, Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityHostAWSMirror},
		}, Networks: []string{createdNetwork.Name}, Labels: owner.Labels(),
	})
	if err != nil {
		if unexpected, inspectErr := adapter.Inspect(context.Background(), runtime.ResourceID(owner.Name())); inspectErr == nil {
			_ = adapter.Delete(context.Background(), unexpected)
		}
		_ = manager.Remove(context.Background(), identity)
		_ = adapter.Delete(context.Background(), networkSnapshot)
		_ = adapter.Delete(context.Background(), volumeSnapshot)
		t.Fatal(err)
	}
	snapshot, err := adapter.Inspect(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleaned := false
	defer func() {
		if cleaned {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if current, inspectErr := adapter.Inspect(cleanupCtx, created.ID); inspectErr == nil {
			if current.State == "running" {
				_ = adapter.Stop(cleanupCtx, current, runtime.StopPolicy{TimeoutSeconds: 10, Signal: "TERM"})
			}
			_ = adapter.Delete(cleanupCtx, current)
		}
		_ = adapter.Delete(cleanupCtx, networkSnapshot)
		_ = adapter.Delete(cleanupCtx, volumeSnapshot)
		_ = manager.Remove(cleanupCtx, identity)
	}()
	if secondMirror, err := manager.Enable(ctx, identity, authority); err != nil || secondMirror != mirror {
		t.Fatalf("post-create exact reattach = %q, %v", secondMirror, err)
	}
	if err := adapter.StartWorkspace(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot, err = adapter.Inspect(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	profileExit, profileErr := adapter.Exec(ctx, snapshot, runtime.ExecSpec{Argv: []string{"/bin/sh", "-c", `test -z "${AWS_PROFILE+x}"`}, User: "501:20"}, runtime.ExecIO{})
	if profileErr != nil || profileExit.Code == nil || *profileExit.Code != 0 {
		t.Fatalf("workspace received AWS_PROFILE: exit %#v, error %v", profileExit, profileErr)
	}
	for _, generation := range []string{"two", strings.Repeat("three-complete", 1024)} {
		rawCredentials := hostAWSTemporaryCredentials(generation) + "\n[named]\naws_access_key_id=ignored\naws_secret_access_key=ignored\naws_session_token=ignored\n"
		filtered, state, filterErr := FilterHostDefaultSnapshot(HostAWSDirectorySnapshot{Config: []byte("[default]\nregion=eu-west-1\n"), Credentials: []byte(rawCredentials)})
		if filterErr != nil || state != HostDefaultAvailable {
			t.Fatalf("filter replacement = %q, %v", state, filterErr)
		}
		temporary := filepath.Join(source, ".credentials-next")
		if err := os.WriteFile(temporary, []byte(rawCredentials), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, filepath.Join(source, hostAWSCredentialsFile)); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			var stdout bytes.Buffer
			exit, execErr := adapter.Exec(ctx, snapshot, runtime.ExecSpec{Argv: []string{"/bin/cat", HostAWSCredentialsGuestPath}, User: "501:20"}, runtime.ExecIO{Stdout: &stdout})
			if execErr == nil && exit.Code != nil && *exit.Code == 0 && bytes.Equal(stdout.Bytes(), filtered.Credentials) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("guest did not observe complete filtered generation of %d bytes; last %d bytes, exit %#v, error %v", len(filtered.Credentials), stdout.Len(), exit, execErr)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	writeExit, writeErr := adapter.Exec(ctx, snapshot, runtime.ExecSpec{Argv: []string{"/bin/sh", "-c", "printf denied > " + HostAWSCredentialsGuestPath}, User: "501:20"}, runtime.ExecIO{})
	if writeErr == nil && writeExit.Code != nil && *writeExit.Code == 0 {
		t.Fatal("guest write to read-only host AWS mirror succeeded")
	}
	if err := adapter.Stop(ctx, snapshot, runtime.StopPolicy{TimeoutSeconds: 10, Signal: "TERM"}); err != nil {
		t.Fatal(err)
	}
	stopped, err := adapter.Inspect(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Delete(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Delete(ctx, volumeSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Delete(ctx, networkSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := manager.Disable(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(mirror); err != nil {
		t.Fatalf("stable publication channel did not survive disable: %v", err)
	}
	for _, name := range []string{hostAWSConfigFile, hostAWSCredentialsFile} {
		contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
		if readErr != nil || len(contents) != 0 {
			t.Fatalf("disabled publication %s was not empty: %d bytes, %v", name, len(contents), readErr)
		}
	}
	if err := manager.Remove(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(mirror); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mirror survived cleanup: %v", err)
	}
	after, err := adapter.List(ctx, runtime.ResourceWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if afterIDs := appleHostAWSMirrorResourceIDs(after); !reflect.DeepEqual(afterIDs, baselineIDs) {
		t.Fatalf("unrelated workspace baseline changed: before %#v after %#v", baselineIDs, afterIDs)
	}
	afterNetworks, err := adapter.List(ctx, runtime.ResourceNetwork)
	if err != nil {
		t.Fatal(err)
	}
	if afterIDs := appleHostAWSMirrorResourceIDs(afterNetworks); !reflect.DeepEqual(afterIDs, baselineNetworkIDs) {
		t.Fatalf("unrelated network baseline changed: before %#v after %#v", baselineNetworkIDs, afterIDs)
	}
	cleaned = true
}

func appleHostAWSMirrorResourceIDs(resources []runtime.ResourceSnapshot) []string {
	ids := make([]string, len(resources))
	for index := range resources {
		ids[index] = string(resources[index].ID)
	}
	sort.Strings(ids)
	return ids
}
