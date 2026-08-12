package auth

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
)

func TestAuthLoginIntentWriteAheadCASPermissionsAndDeletion(t *testing.T) {
	repository, err := NewRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	intent := AuthLoginIntent{
		Version: AuthLoginIntentVersion, Generation: 1,
		Project: Project{ID: model.ProjectID("eeeeeeeeeeeeeeeeeeee"), Harness: harness.Claude},
		SessionID: "00000000-0000-7000-8000-000000000121",
		PlanHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		State: AuthLoginPlanned, VolumeName: "dsx-project-authvol-abcdef", ContainerName: "dsx-project-authlogin-abcdef",
	}
	if err := repository.CreateAuthLoginIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	path := repository.authLoginIntentPath(intent.Project, intent.SessionID)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("intent mode = %#o", info.Mode().Perm())
	}
	loaded, found, err := repository.LoadAuthLoginIntent(context.Background(), intent.Project, intent.SessionID)
	if err != nil || !found || loaded != intent {
		t.Fatalf("LoadAuthLoginIntent() = %#v, %v, %v", loaded, found, err)
	}
	intent.Generation = 2
	intent.State = AuthLoginCreating
	intent.VolumeID = "volume-id"
	if err := repository.ReplaceAuthLoginIntent(context.Background(), intent, 1); err != nil {
		t.Fatal(err)
	}
	stale := intent
	stale.Generation = 3
	if err := repository.ReplaceAuthLoginIntent(context.Background(), stale, 1); err == nil {
		t.Fatal("stale intent replacement succeeded")
	}
	if err := repository.DeleteAuthLoginIntent(context.Background(), intent.Project, intent.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repository.LoadAuthLoginIntent(context.Background(), intent.Project, intent.SessionID); err != nil || found {
		t.Fatalf("deleted intent found=%v err=%v", found, err)
	}
}

func TestAuthLoginIntentRejectsSecretBearingOrInvalidIdentityFields(t *testing.T) {
	intent := AuthLoginIntent{
		Version: AuthLoginIntentVersion, Generation: 1,
		Project: Project{ID: model.ProjectID("ffffffffffffffffffff"), Harness: harness.Claude},
		SessionID: "00000000-0000-7000-8000-000000000122",
		PlanHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		State: AuthLoginPlanned, VolumeName: "auth-volume", ContainerName: "auth-login",
	}
	intent.ContainerID = "secret-token\nleak"
	if err := ValidateAuthLoginIntent(intent); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid intent error = %v", err)
	}
}
