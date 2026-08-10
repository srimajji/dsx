package ownership

import (
	"testing"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

func TestIdentityLabelsAndNameDeterministic(t *testing.T) {
	identity := testIdentity(t, runtime.ResourceWorkspace, "workspace")
	if got, want := identity.Name(), "dsx-abcdefghijklmnopqrst-main-workspace"; got != want {
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
}

func TestClassifyOwnedRequiresCorroboration(t *testing.T) {
	identity := testIdentity(t, runtime.ResourceWorkspace, "workspace")
	record := identity.ManifestRecord()
	record.RuntimeID = record.ExpectedID
	record.Created = true
	observed := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(record.ExpectedID), Name: identity.Name(), Kind: runtime.ResourceWorkspace},
		Labels:   identity.Labels(),
	}
	classification := Classify(&record, &observed)
	if classification.Outcome != OutcomeOwned || !classification.DeleteAllowed {
		t.Fatalf("classification = %#v", classification)
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
	if classification.Outcome != OutcomeOwned || !classification.DeleteAllowed {
		t.Fatalf("write-ahead classification = %#v", classification)
	}
	observed.ID = "different"
	classification = Classify(&record, &observed)
	if classification.Outcome != OutcomeAmbiguous || classification.DeleteAllowed {
		t.Fatalf("mismatched write-ahead classification = %#v", classification)
	}
}

func TestClassifyOrphansArePreserved(t *testing.T) {
	identity := testIdentity(t, runtime.ResourceVolume, "project-home")
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
	} {
		if classification.Outcome != OutcomeOrphaned || classification.DeleteAllowed {
			t.Fatalf("%s classification = %#v", name, classification)
		}
	}
}

func TestClassifyMismatchIsAmbiguousAndPreserved(t *testing.T) {
	identity := testIdentity(t, runtime.ResourceNetwork, "network")
	record := identity.ManifestRecord()
	record.RuntimeID = record.ExpectedID
	record.Created = true
	observed := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: "different-id", Name: identity.Name(), Kind: runtime.ResourceNetwork},
		Labels:   identity.Labels(),
	}
	classification := Classify(&record, &observed)
	if classification.Outcome != OutcomeAmbiguous || classification.DeleteAllowed {
		t.Fatalf("identity mismatch = %#v", classification)
	}

	observed.ID = runtime.ResourceID(record.ExpectedID)
	observed.Labels = append([]runtime.Label(nil), identity.Labels()...)
	observed.Labels[2].Value = "bbbbbbbbbbbbbbbbbbbb"
	classification = Classify(&record, &observed)
	if classification.Outcome != OutcomeAmbiguous || classification.DeleteAllowed {
		t.Fatalf("label mismatch = %#v", classification)
	}
	observed.Labels = append(identity.Labels(), runtime.Label{Key: "com.example.extra", Value: "value"})
	classification = Classify(&record, &observed)
	if classification.Outcome != OutcomeAmbiguous || classification.DeleteAllowed {
		t.Fatalf("extra-label mismatch = %#v", classification)
	}
}

func TestClassifyForeignAndBuilderAlwaysExcluded(t *testing.T) {
	foreign := runtime.ResourceSnapshot{Resource: runtime.Resource{ID: "foreign", Name: "user-container", Kind: runtime.ResourceWorkspace}}
	classification := Classify(nil, &foreign)
	if classification.Outcome != OutcomeForeign || classification.DeleteAllowed {
		t.Fatalf("foreign = %#v", classification)
	}

	identity := testIdentity(t, runtime.ResourceWorkspace, "workspace")
	record := identity.ManifestRecord()
	record.Name = "buildkit"
	record.RuntimeID = "builder"
	record.Created = true
	builder := runtime.ResourceSnapshot{Resource: runtime.Resource{ID: "builder", Name: "buildkit", Kind: runtime.ResourceWorkspace}, Labels: identity.Labels()}
	classification = Classify(&record, &builder)
	if classification.Outcome != OutcomeExcluded || classification.DeleteAllowed {
		t.Fatalf("builder = %#v", classification)
	}
}

func TestClassifyDuplicateOrUnknownDSXLabelsAmbiguous(t *testing.T) {
	identity := testIdentity(t, runtime.ResourceWorkspace, "workspace")
	record := identity.ManifestRecord()
	record.RuntimeID = "container-id"
	record.Created = true
	observed := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: "container-id", Name: identity.Name(), Kind: runtime.ResourceWorkspace},
		Labels:   append(identity.Labels(), runtime.Label{Key: ManagedLabel, Value: "true"}),
	}
	if got := Classify(&record, &observed); got.Outcome != OutcomeAmbiguous || got.DeleteAllowed {
		t.Fatalf("duplicate = %#v", got)
	}
	observed.Labels = append(identity.Labels(), runtime.Label{Key: "dev.dsx.future", Value: "value"})
	if got := Classify(&record, &observed); got.Outcome != OutcomeAmbiguous || got.DeleteAllowed {
		t.Fatalf("unknown = %#v", got)
	}
	if got := Classify(nil, &observed); got.Outcome != OutcomeAmbiguous || got.DeleteAllowed {
		t.Fatalf("unmanifested malformed DSX resource = %#v", got)
	}
}

func TestInvalidIdentityRejected(t *testing.T) {
	_, err := NewIdentity("abcdefghijklmnopqrst", "main", "01890f5c-7b00-7000-8000-000000000001", runtime.ResourceWorkspace, "this-role-name-is-far-too-long")
	if err == nil {
		t.Fatal("invalid long role accepted")
	}
}

func testIdentity(t *testing.T, kind runtime.ResourceKind, role string) Identity {
	t.Helper()
	projectID, err := model.ParseProjectID("abcdefghijklmnopqrst")
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := model.ParseSandboxName("main")
	if err != nil {
		t.Fatal(err)
	}
	runID, err := model.ParseRunID("01890f5c-7b00-7000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewIdentity(projectID, sandbox, runID, kind, role)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
