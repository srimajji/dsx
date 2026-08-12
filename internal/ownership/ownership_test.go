package ownership

import (
	"testing"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

func TestIdentityLabelsAndReadableNameDeterministic(t *testing.T) {
	identity := testIdentity(t, runtime.ResourceWorkspace, "workspace")
	if got, want := identity.Name(), "dsx-tracking-chrome-feature-a-workspace-1abbf9"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
	first, second := identity.Labels(), identity.Labels()
	if len(first) != len(ownershipLabelOrder) || len(second) != len(first) {
		t.Fatalf("labels = %#v", first)
	}
	for index, key := range ownershipLabelOrder {
		if first[index] != second[index] || first[index].Key != key {
			t.Fatalf("labels are not deterministic: %#v / %#v", first, second)
		}
	}
	if first[3].Key != WorkspaceLabel || first[3].Value != "feature-a" {
		t.Fatalf("workspace ownership label = %#v", first[3])
	}
}

func TestClassifyCurrentOwnedRequiresExactCorroboration(t *testing.T) {
	identity := testIdentity(t, runtime.ResourceWorkspace, "workspace")
	record := identity.ManifestRecord()
	record.RuntimeID = record.ExpectedID
	record.Created = true
	observed := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(record.ExpectedID), Name: identity.Name(), Kind: runtime.ResourceWorkspace},
		Labels:   identity.Labels(),
	}
	classification := Classify(&record, &observed)
	if classification.Outcome != OutcomeOwned || !classification.DeleteAllowed || !classification.AdoptAllowed || classification.Legacy {
		t.Fatalf("classification = %#v", classification)
	}

	observed.Labels = append([]runtime.Label(nil), observed.Labels...)
	observed.Labels[2].Value = string(mustProjectID(t, "/different/project"))
	classification = Classify(&record, &observed)
	if classification.Outcome != OutcomeAmbiguous || classification.DeleteAllowed || classification.AdoptAllowed {
		t.Fatalf("mismatched ownership classification = %#v", classification)
	}
}

func TestClassifyWriteAheadIntentAuthorizesOnlyExactRuntimeIdentity(t *testing.T) {
	identity := testIdentity(t, runtime.ResourceNetwork, "network")
	record := identity.ManifestRecord()
	observed := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(record.ExpectedID), Name: record.Name, Kind: runtime.ResourceNetwork},
		Labels:   identity.Labels(),
	}
	classification := Classify(&record, &observed)
	if classification.Outcome != OutcomeOwned || !classification.DeleteAllowed || !classification.AdoptAllowed {
		t.Fatalf("write-ahead classification = %#v", classification)
	}
	observed.ID = "different"
	classification = Classify(&record, &observed)
	if classification.Outcome != OutcomeAmbiguous || classification.DeleteAllowed || classification.AdoptAllowed {
		t.Fatalf("mismatched write-ahead classification = %#v", classification)
	}
}

func TestClassifyLegacyIsCleanupOnlyWithCorroboratingLabels(t *testing.T) {
	identity := testIdentity(t, runtime.ResourceWorkspace, "workspace")
	legacyRole := "project-home"
	legacyName := "dsx-" + string(identity.ProjectID) + "-" + string(identity.Workspace) + "-" + legacyRole
	legacyLabels := state.LegacyResourceOwnershipLabels(identity.ProjectID, identity.Workspace, identity.RunID, string(identity.Kind), legacyRole)
	record := state.ResourceRecord{
		Kind: string(identity.Kind), Role: legacyRole, Name: legacyName, ExpectedID: legacyName,
		RuntimeID: legacyName, Created: true, Labels: legacyLabels,
	}
	observedLabels := make([]runtime.Label, len(legacyLabels))
	for index, label := range legacyLabels {
		observedLabels[index] = runtime.Label{Key: label.Key, Value: label.Value}
	}
	observed := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(legacyName), Name: legacyName, Kind: identity.Kind},
		Labels:   observedLabels,
	}
	classification := Classify(&record, &observed)
	if classification.Outcome != OutcomeLegacy || !classification.DeleteAllowed || classification.AdoptAllowed || !classification.Legacy {
		t.Fatalf("legacy classification = %#v", classification)
	}

	withoutManifest := Classify(nil, &observed)
	if withoutManifest.Outcome != OutcomeLegacy || withoutManifest.DeleteAllowed || withoutManifest.AdoptAllowed || !withoutManifest.Legacy {
		t.Fatalf("unmanifested legacy classification = %#v", withoutManifest)
	}
	observed.Labels[2].Value = string(mustProjectID(t, "/different/project"))
	mismatch := Classify(&record, &observed)
	if mismatch.Outcome != OutcomeAmbiguous || mismatch.DeleteAllowed || mismatch.AdoptAllowed {
		t.Fatalf("legacy label mismatch = %#v", mismatch)
	}
}

func TestClassifyOrphansNamesAndBuildersArePreserved(t *testing.T) {
	identity := testIdentity(t, runtime.ResourceVolume, "session")
	record := identity.ManifestRecord()
	record.RuntimeID = record.ExpectedID
	record.Created = true
	observed := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(record.ExpectedID), Name: identity.Name(), Kind: runtime.ResourceVolume},
		Labels:   identity.Labels(),
	}
	for name, classification := range map[string]Classification{
		"manifest only": Classify(&record, nil),
		"runtime only":  Classify(nil, &observed),
		"name only":     Classify(nil, &runtime.ResourceSnapshot{Resource: runtime.Resource{ID: "dsx-old", Name: "dsx-old", Kind: runtime.ResourceWorkspace}}),
	} {
		if classification.DeleteAllowed || classification.AdoptAllowed {
			t.Fatalf("%s classification = %#v", name, classification)
		}
	}

	builder := runtime.ResourceSnapshot{Resource: runtime.Resource{ID: "buildkit", Name: "buildkit", Kind: runtime.ResourceWorkspace}, Labels: identity.Labels()}
	classification := Classify(&record, &builder)
	if classification.Outcome != OutcomeExcluded || classification.DeleteAllowed || classification.AdoptAllowed {
		t.Fatalf("builder = %#v", classification)
	}
}

func TestClassifyDuplicateOrUnknownDSXLabelsAmbiguous(t *testing.T) {
	identity := testIdentity(t, runtime.ResourceWorkspace, "workspace")
	record := identity.ManifestRecord()
	record.RuntimeID = record.ExpectedID
	record.Created = true
	observed := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(record.RuntimeID), Name: record.Name, Kind: runtime.ResourceWorkspace},
		Labels:   append(identity.Labels(), runtime.Label{Key: ManagedLabel, Value: "true"}),
	}
	if got := Classify(&record, &observed); got.Outcome != OutcomeAmbiguous || got.DeleteAllowed || got.AdoptAllowed {
		t.Fatalf("duplicate = %#v", got)
	}
	observed.Labels = append(identity.Labels(), runtime.Label{Key: "dev.dsx.future", Value: "value"})
	if got := Classify(&record, &observed); got.Outcome != OutcomeAmbiguous || got.DeleteAllowed || got.AdoptAllowed {
		t.Fatalf("unknown = %#v", got)
	}
}

func TestInvalidIdentityRejected(t *testing.T) {
	root := "/Volumes/Dev/work/tracking-chrome-extension"
	projectID := mustProjectID(t, root)
	workspace, _ := model.ParseWorkspaceName("feature-a")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000001")
	for _, test := range []struct {
		projectID model.ProjectID
		root      string
		role      string
	}{
		{projectID: projectID, root: "/different/project", role: "workspace"},
		{projectID: projectID, root: root, role: "role-too-long"},
		{projectID: projectID, root: root, role: "bad_role"},
	} {
		if _, err := NewIdentity(test.projectID, test.root, workspace, runID, runtime.ResourceWorkspace, test.role); err == nil {
			t.Fatalf("invalid identity accepted: %#v", test)
		}
	}
}

func testIdentity(t *testing.T, kind runtime.ResourceKind, role string) Identity {
	t.Helper()
	root := "/Volumes/Dev/work/tracking-chrome-extension"
	projectID := mustProjectID(t, root)
	workspace, err := model.ParseWorkspaceName("feature-a")
	if err != nil {
		t.Fatal(err)
	}
	runID, err := model.ParseRunID("01890f5c-7b00-7000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewIdentity(projectID, root, workspace, runID, kind, role)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func mustProjectID(t *testing.T, root string) model.ProjectID {
	t.Helper()
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	return projectID
}
