package ownership

import (
	"testing"

	"github.com/srimajji/dsx/internal/runtime"
)

func FuzzOwnershipFailsClosed(f *testing.F) {
	for selector := byte(0); selector < 11; selector++ {
		f.Add(selector, "ambiguous")
	}
	f.Add(byte(1), "true")
	f.Add(byte(2), "dev.dsx.future")
	f.Add(byte(3), "\x1b[2J")

	f.Fuzz(func(t *testing.T, selector byte, value string) {
		if len(value) > 4096 {
			t.Skip()
		}
		identity := testIdentity(t, runtime.ResourceWorkspace, "workspace")
		record := identity.ManifestRecord()
		record.RuntimeID = record.ExpectedID
		record.Created = true
		observed := runtime.ResourceSnapshot{
			Resource: runtime.Resource{ID: runtime.ResourceID(record.ExpectedID), Name: record.Name, Kind: runtime.ResourceWorkspace},
			Labels:   append([]runtime.Label(nil), identity.Labels()...),
		}

		var recordPointer = &record
		var observedPointer = &observed
		caseNumber := selector % 11
		switch caseNumber {
		case 0: // Missing required ownership evidence.
			observed.Labels = observed.Labels[1:]
		case 1: // Duplicate labels are ambiguous even when values agree.
			observed.Labels = append(observed.Labels, observed.Labels[0])
		case 2: // Unknown labels in the DSX namespace cannot grant authority.
			observed.Labels = append(observed.Labels, runtime.Label{Key: "dev.dsx." + value, Value: value})
		case 3:
			observed.Labels[2].Value = "mutated-" + value
		case 4:
			observed.ID = runtime.ResourceID("different-" + value)
		case 5:
			observed.Name = "different-" + value
		case 6:
			observed.Kind = runtime.ResourceVolume
		case 7:
			observed.Name = "buildkit"
		case 8:
			recordPointer = nil
		case 9:
			observedPointer = nil
		case 10: // The sole fully corroborated seed.
		}

		classification := Classify(recordPointer, observedPointer)
		if classification.DeleteAllowed != (classification.Outcome == OutcomeOwned) {
			t.Fatalf("inconsistent classification: %#v", classification)
		}
		if caseNumber != 10 && classification.DeleteAllowed {
			t.Fatalf("ambiguous ownership evidence authorized deletion: selector=%d value=%q classification=%#v", selector, value, classification)
		}
		if caseNumber == 10 && !classification.DeleteAllowed {
			t.Fatalf("exact ownership evidence was not recognized: %#v", classification)
		}
	})
}
