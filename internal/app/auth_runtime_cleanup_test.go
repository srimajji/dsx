package app

import (
	"context"
	"testing"

	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

func TestAuthLoginCleanupPreservesReplacementWithForeignOwnership(t *testing.T) {
	projectID := model.ProjectID("cdcdcdcdcdcdcdcdcdcd")
	sessionID := model.RunID("00000000-0000-7000-8000-000000000121")
	name, err := runtime.CanonicalAuthLoginName(t.TempDir(), string(harness.Claude))
	if err != nil {
		t.Fatal(err)
	}
	labels, err := runtime.AuthLoginOwnershipLabels(projectID, sessionID, string(harness.Claude))
	if err != nil {
		t.Fatal(err)
	}
	foreign := append([]runtime.Label(nil), labels...)
	foreign[0].Value = "foreign"
	missing := append([]runtime.Label(nil), labels[:len(labels)-1]...)
	ambiguous := append(append([]runtime.Label(nil), labels...), labels[0])

	for _, test := range []struct {
		name      string
		kind      runtime.ResourceKind
		container bool
		labels    []runtime.Label
	}{
		{name: "container-foreign", kind: runtime.ResourceAuthLogin, container: true, labels: foreign},
		{name: "container-missing", kind: runtime.ResourceAuthLogin, container: true, labels: missing},
		{name: "container-ambiguous", kind: runtime.ResourceAuthLogin, container: true, labels: ambiguous},
		{name: "volume-foreign", kind: runtime.ResourceVolume, labels: foreign},
		{name: "volume-missing", kind: runtime.ResourceVolume, labels: missing},
		{name: "volume-ambiguous", kind: runtime.ResourceVolume, labels: ambiguous},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, err := auth.NewRepository(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			intent := auth.AuthLoginIntent{
				Version: auth.AuthLoginIntentVersion, Generation: 1,
				Project: auth.Project{ID: projectID, Harness: harness.Claude}, SessionID: string(sessionID),
				PlanHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				State:    auth.AuthLoginPlanned, VolumeName: name, ContainerName: name,
			}
			if err := repository.CreateAuthLoginIntent(context.Background(), intent); err != nil {
				t.Fatal(err)
			}
			intent.Generation = 2
			intent.State = auth.AuthLoginRunning
			resource := runtime.Resource{ID: runtime.ResourceID(name), Name: name, Kind: test.kind}
			var container, volume runtime.Resource
			if test.container {
				container = resource
				intent.ContainerID = name
			} else {
				volume = resource
				intent.VolumeID = name
			}
			if err := repository.ReplaceAuthLoginIntent(context.Background(), intent, 1); err != nil {
				t.Fatal(err)
			}

			fake := &recordingWorkspaceRuntime{resources: map[runtime.ResourceID]runtime.ResourceSnapshot{
				resource.ID: {Resource: resource, State: "running", Labels: test.labels},
			}}
			runner := &RuntimeAuthSessionRunner{workspaces: &WorkspaceService{runtime: fake}, repository: repository}
			if err := runner.cleanup(context.Background(), &intent, container, volume); err == nil {
				t.Fatal("cleanup accepted a replacement without exact ownership")
			}
			if _, found := fake.resources[resource.ID]; !found {
				t.Fatal("cleanup deleted the ambiguous replacement")
			}
			if fake.stops != 0 {
				t.Fatalf("cleanup stopped an ambiguous replacement %d times", fake.stops)
			}
			preserved, found, err := repository.LoadAuthLoginIntent(context.Background(), intent.Project, intent.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if !found || preserved.State != auth.AuthLoginCleaning {
				t.Fatalf("cleanup intent = %#v, found=%t", preserved, found)
			}
		})
	}
}
