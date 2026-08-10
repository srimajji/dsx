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

func TestAppleLeappMirrorAtomicRotationReadOnlyMount(t *testing.T) {
	containerExecutable := os.Getenv("DSX_APPLE_CONTAINER")
	dsxExecutable := os.Getenv("DSX_LEAPP_MIRROR_DSX")
	imageReference := os.Getenv("DSX_APPLE_TEST_IMAGE")
	if containerExecutable == "" || dsxExecutable == "" || imageReference == "" {
		t.Skip("set DSX_APPLE_CONTAINER, DSX_LEAPP_MIRROR_DSX, and DSX_APPLE_TEST_IMAGE for the destructive opt-in Apple mirror test")
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
	baselineIDs := appleMirrorResourceIDs(baseline)
	baselineNetworks, err := adapter.List(ctx, runtime.ResourceNetwork)
	if err != nil {
		t.Fatal(err)
	}
	baselineNetworkIDs := appleMirrorResourceIDs(baselineNetworks)
	image, err := adapter.EnsureImage(ctx, runtime.ImageSpec{Reference: imageReference})
	if err != nil {
		t.Fatal(err)
	}

	stateRoot := canonicalTemporaryDirectory(t)
	source := leappFixture(t, "[default]\nregion=eu-west-1\n", "generation-one\n")
	authority, err := ResolveLeappDirectory(source)
	if err != nil {
		t.Fatal(err)
	}
	identity := LeaseIdentity{ProjectID: model.ProjectID("abcdefghijklmnopqrst"), Sandbox: model.SandboxName("mirror-apple"), RunID: model.RunID("01890f5c-7b00-7000-8000-000000000051")}
	manager, err := NewProductionLeappMirrorManager(stateRoot, dsxExecutable)
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := manager.Ensure(ctx, identity, authority)
	if err != nil {
		t.Fatal(err)
	}

	networkOwner, err := ownership.NewIdentity(identity.ProjectID, identity.Sandbox, identity.RunID, runtime.ResourceNetwork, "network")
	if err != nil {
		t.Fatal(err)
	}
	createdNetwork, err := adapter.CreateNetwork(ctx, runtime.NetworkSpec{Name: networkOwner.Name(), Labels: networkOwner.Labels()})
	if err != nil {
		_ = manager.Stop(context.Background(), identity)
		t.Fatal(err)
	}
	networkSnapshot := runtime.ResourceSnapshot{Resource: createdNetwork, Labels: networkOwner.Labels()}
	owner, err := ownership.NewIdentity(identity.ProjectID, identity.Sandbox, identity.RunID, runtime.ResourceWorkspace, "workspace")
	if err != nil {
		_ = adapter.Delete(context.Background(), networkSnapshot)
		_ = manager.Stop(context.Background(), identity)
		t.Fatal(err)
	}
	created, err := adapter.CreateWorkspace(ctx, runtime.WorkspaceSpec{
		Name: owner.Name(), Image: image, Entrypoint: []string{"/bin/sh", "-lc", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"},
		WorkingDir: "/", User: "501:20", Mounts: []runtime.Mount{{Source: mirror, Target: LeappGuestDirectory, Type: "bind", ReadOnly: true, Authority: runtime.MountAuthorityLeappMirror}}, Networks: []string{createdNetwork.Name}, Labels: owner.Labels(),
	})
	if err != nil {
		if unexpected, inspectErr := adapter.Inspect(context.Background(), runtime.ResourceID(owner.Name())); inspectErr == nil {
			_ = adapter.Delete(context.Background(), unexpected)
		}
		_ = manager.Stop(context.Background(), identity)
		_ = adapter.Delete(context.Background(), networkSnapshot)
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
		_ = manager.Stop(cleanupCtx, identity)
	}()
	if secondMirror, err := manager.Ensure(ctx, identity, authority); err != nil || secondMirror != mirror {
		t.Fatalf("post-create exact reattach = %q, %v", secondMirror, err)
	}
	if err := adapter.StartWorkspace(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot, err = adapter.Inspect(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, generation := range []string{"generation-two\n", strings.Repeat("generation-three-complete\n", 512)} {
		temporary := filepath.Join(source, ".credentials-next")
		if err := os.WriteFile(temporary, []byte(generation), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, filepath.Join(source, leappCredentialsFile)); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			var stdout bytes.Buffer
			exit, execErr := adapter.Exec(ctx, snapshot, runtime.ExecSpec{Argv: []string{"/bin/cat", LeappCredentialsGuestPath}, User: "501:20"}, runtime.ExecIO{Stdout: &stdout})
			if execErr == nil && exit.Code != nil && *exit.Code == 0 && stdout.String() == generation {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("guest did not observe complete generation of %d bytes; last %d bytes, exit %#v, error %v", len(generation), stdout.Len(), exit, execErr)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	writeExit, writeErr := adapter.Exec(ctx, snapshot, runtime.ExecSpec{Argv: []string{"/bin/sh", "-c", "printf denied > " + LeappCredentialsGuestPath}, User: "501:20"}, runtime.ExecIO{})
	if writeErr == nil && writeExit.Code != nil && *writeExit.Code == 0 {
		t.Fatal("guest write to read-only Leapp mirror succeeded")
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
	if err := adapter.Delete(ctx, networkSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(mirror); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mirror survived cleanup: %v", err)
	}
	after, err := adapter.List(ctx, runtime.ResourceWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if afterIDs := appleMirrorResourceIDs(after); !reflect.DeepEqual(afterIDs, baselineIDs) {
		t.Fatalf("unrelated workspace baseline changed: before %#v after %#v", baselineIDs, afterIDs)
	}
	afterNetworks, err := adapter.List(ctx, runtime.ResourceNetwork)
	if err != nil {
		t.Fatal(err)
	}
	if afterIDs := appleMirrorResourceIDs(afterNetworks); !reflect.DeepEqual(afterIDs, baselineNetworkIDs) {
		t.Fatalf("unrelated network baseline changed: before %#v after %#v", baselineNetworkIDs, afterIDs)
	}
	cleaned = true
}

func appleMirrorResourceIDs(resources []runtime.ResourceSnapshot) []string {
	ids := make([]string, len(resources))
	for index := range resources {
		ids[index] = string(resources[index].ID)
	}
	sort.Strings(ids)
	return ids
}
