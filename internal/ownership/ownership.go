package ownership

import (
	"fmt"
	"strings"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

const (
	ManagedLabel  = state.OwnershipManagedLabel
	ProjectLabel  = state.OwnershipProjectLabel
	SandboxLabel  = state.OwnershipSandboxLabel
	RunLabel      = state.OwnershipRunLabel
	KindLabel     = state.OwnershipKindLabel
	RoleLabel     = state.OwnershipRoleLabel
	ContractLabel = state.OwnershipContractLabel

	ContractValue        = state.OwnershipContractValue
	maxResourceNameBytes = 63
)

var ownershipLabelOrder = [...]string{
	ManagedLabel,
	ContractLabel,
	ProjectLabel,
	SandboxLabel,
	RunLabel,
	KindLabel,
	RoleLabel,
}

type Identity struct {
	ProjectID model.ProjectID
	Sandbox   model.SandboxName
	RunID     model.RunID
	Kind      runtime.ResourceKind
	Role      string
}

func NewIdentity(projectID model.ProjectID, sandbox model.SandboxName, runID model.RunID, kind runtime.ResourceKind, role string) (Identity, error) {
	identity := Identity{ProjectID: projectID, Sandbox: sandbox, RunID: runID, Kind: kind, Role: role}
	if err := identity.Validate(); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func (identity Identity) Validate() error {
	if _, err := model.ParseProjectID(string(identity.ProjectID)); err != nil {
		return fmt.Errorf("ownership project: %w", err)
	}
	if _, err := model.ParseSandboxName(string(identity.Sandbox)); err != nil {
		return fmt.Errorf("ownership sandbox: %w", err)
	}
	if _, err := model.ParseRunID(string(identity.RunID)); err != nil {
		return fmt.Errorf("ownership run: %w", err)
	}
	switch identity.Kind {
	case runtime.ResourceWorkspace, runtime.ResourceBrowser, runtime.ResourceNetwork, runtime.ResourceVolume:
	default:
		return fmt.Errorf("unsupported ownership resource kind %q", identity.Kind)
	}
	if _, err := model.ParseSandboxName(identity.Role); err != nil {
		return fmt.Errorf("ownership role: %w", err)
	}
	if len(identity.Name()) > maxResourceNameBytes {
		return fmt.Errorf("canonical resource name exceeds %d bytes", maxResourceNameBytes)
	}
	return nil
}

func (identity Identity) Name() string {
	return state.CanonicalResourceName(identity.ProjectID, identity.Sandbox, identity.Role)
}

func (identity Identity) Labels() []runtime.Label {
	recordLabels := state.ResourceOwnershipLabels(identity.ProjectID, identity.Sandbox, identity.RunID, string(identity.Kind), identity.Role)
	labels := make([]runtime.Label, len(recordLabels))
	for index, label := range recordLabels {
		labels[index] = runtime.Label{Key: label.Key, Value: label.Value}
	}
	return labels
}

func (identity Identity) ManifestRecord() state.ResourceRecord {
	name := identity.Name()
	return state.ResourceRecord{
		Kind:       string(identity.Kind),
		Role:       identity.Role,
		Name:       name,
		ExpectedID: name,
		Labels:     state.ResourceOwnershipLabels(identity.ProjectID, identity.Sandbox, identity.RunID, string(identity.Kind), identity.Role),
	}
}

type Outcome string

const (
	OutcomeOwned     Outcome = "owned"
	OutcomeOrphaned  Outcome = "orphaned"
	OutcomeAmbiguous Outcome = "ambiguous"
	OutcomeForeign   Outcome = "foreign"
	OutcomeExcluded  Outcome = "excluded"
)

type Classification struct {
	Outcome       Outcome
	Reason        string
	DeleteAllowed bool
}

func Classify(record *state.ResourceRecord, observed *runtime.ResourceSnapshot) Classification {
	if isBuilderResource(record, observed) {
		return Classification{Outcome: OutcomeExcluded, Reason: "Apple runtime builder resources are never DSX-owned"}
	}
	if record == nil && observed == nil {
		return Classification{Outcome: OutcomeForeign, Reason: "no manifest or runtime resource"}
	}
	if record == nil {
		if !hasDSXLabel(observed.Labels) {
			if strings.HasPrefix(observed.Name, "dsx-") {
				return Classification{Outcome: OutcomeAmbiguous, Reason: "DSX-like runtime name has no ownership labels"}
			}
			return Classification{Outcome: OutcomeForeign, Reason: "runtime resource has no DSX ownership evidence"}
		}
		labels, err := runtimeLabelMap(observed.Labels)
		if err != nil {
			return Classification{Outcome: OutcomeAmbiguous, Reason: err.Error()}
		}
		identity, err := identityFromLabels(labels)
		if err != nil || observed.Name != identity.Name() || observed.Kind != identity.Kind {
			return Classification{Outcome: OutcomeAmbiguous, Reason: "runtime ownership evidence is incomplete or inconsistent"}
		}
		return Classification{Outcome: OutcomeOrphaned, Reason: "runtime resource has no corroborating manifest"}
	}
	if observed == nil {
		return Classification{Outcome: OutcomeOrphaned, Reason: "manifest resource is absent from the runtime"}
	}
	if err := validateRecord(*record); err != nil {
		return Classification{Outcome: OutcomeAmbiguous, Reason: "invalid manifest ownership: " + err.Error()}
	}
	expectedID := record.RuntimeID
	if expectedID == "" {
		expectedID = record.ExpectedID
	}
	if observed.ID == "" || expectedID == "" || string(observed.ID) != expectedID {
		return Classification{Outcome: OutcomeAmbiguous, Reason: "runtime identity does not match manifest"}
	}
	if observed.Name != record.Name || string(observed.Kind) != record.Kind {
		return Classification{Outcome: OutcomeAmbiguous, Reason: "runtime name or kind does not match manifest"}
	}
	if err := compareLabels(record.Labels, observed.Labels); err != nil {
		return Classification{Outcome: OutcomeAmbiguous, Reason: err.Error()}
	}
	if record.Deleted {
		return Classification{Outcome: OutcomeAmbiguous, Reason: "manifest marks a still-present resource deleted"}
	}
	if !record.Created {
		return Classification{Outcome: OutcomeOwned, Reason: "write-ahead intent and runtime ownership corroborate", DeleteAllowed: true}
	}
	return Classification{Outcome: OutcomeOwned, Reason: "manifest and runtime ownership corroborate", DeleteAllowed: true}
}

func validateRecord(record state.ResourceRecord) error {
	labels, err := stateLabelMap(record.Labels)
	if err != nil {
		return err
	}
	if len(labels) != len(ownershipLabelOrder) {
		return fmt.Errorf("manifest must contain exactly %d ownership labels", len(ownershipLabelOrder))
	}
	identity, err := identityFromLabels(labels)
	if err != nil {
		return err
	}
	if record.Name != identity.Name() || record.ExpectedID != record.Name || record.Kind != string(identity.Kind) || record.Role != identity.Role {
		return fmt.Errorf("record fields do not match canonical ownership labels")
	}
	if record.Created && record.RuntimeID != record.ExpectedID {
		return fmt.Errorf("created record runtime identity does not match its write-ahead identity")
	}
	if !record.Created && record.RuntimeID != "" {
		return fmt.Errorf("uncreated record contains a runtime identity")
	}
	return nil
}

func identityFromLabels(labels map[string]string) (Identity, error) {
	for _, key := range ownershipLabelOrder {
		if _, exists := labels[key]; !exists {
			return Identity{}, fmt.Errorf("missing ownership label %q", key)
		}
	}
	for key := range labels {
		if strings.HasPrefix(key, "dev.dsx.") && !isOwnershipLabel(key) {
			return Identity{}, fmt.Errorf("unknown DSX ownership label %q", key)
		}
	}
	if labels[ManagedLabel] != "true" || labels[ContractLabel] != ContractValue {
		return Identity{}, fmt.Errorf("managed or contract label is invalid")
	}
	projectID, err := model.ParseProjectID(labels[ProjectLabel])
	if err != nil {
		return Identity{}, err
	}
	sandbox, err := model.ParseSandboxName(labels[SandboxLabel])
	if err != nil {
		return Identity{}, err
	}
	runID, err := model.ParseRunID(labels[RunLabel])
	if err != nil {
		return Identity{}, err
	}
	return NewIdentity(projectID, sandbox, runID, runtime.ResourceKind(labels[KindLabel]), labels[RoleLabel])
}

func compareLabels(expected []state.OwnershipLabel, observed []runtime.Label) error {
	expectedMap, err := stateLabelMap(expected)
	if err != nil {
		return fmt.Errorf("manifest labels: %w", err)
	}
	observedMap, err := runtimeLabelMap(observed)
	if err != nil {
		return err
	}
	if len(observedMap) != len(ownershipLabelOrder) {
		return fmt.Errorf("runtime must contain exactly %d ownership labels", len(ownershipLabelOrder))
	}
	if _, err := identityFromLabels(observedMap); err != nil {
		return fmt.Errorf("runtime ownership labels: %w", err)
	}
	for _, key := range ownershipLabelOrder {
		if expectedMap[key] != observedMap[key] {
			return fmt.Errorf("runtime ownership label %q does not match manifest", key)
		}
	}
	return nil
}

func runtimeLabelMap(labels []runtime.Label) (map[string]string, error) {
	result := make(map[string]string, len(labels))
	for _, label := range labels {
		if label.Key == "" || label.Value == "" {
			return nil, fmt.Errorf("runtime labels must be nonempty")
		}
		if _, duplicate := result[label.Key]; duplicate {
			return nil, fmt.Errorf("runtime labels contain duplicate %q", label.Key)
		}
		result[label.Key] = label.Value
	}
	return result, nil
}

func stateLabelMap(labels []state.OwnershipLabel) (map[string]string, error) {
	result := make(map[string]string, len(labels))
	for _, label := range labels {
		if label.Key == "" || label.Value == "" {
			return nil, fmt.Errorf("ownership labels must be nonempty")
		}
		if _, duplicate := result[label.Key]; duplicate {
			return nil, fmt.Errorf("duplicate ownership label %q", label.Key)
		}
		result[label.Key] = label.Value
	}
	return result, nil
}

func isOwnershipLabel(key string) bool {
	for _, known := range ownershipLabelOrder {
		if key == known {
			return true
		}
	}
	return false
}

func hasDSXLabel(labels []runtime.Label) bool {
	for _, label := range labels {
		if strings.HasPrefix(label.Key, "dev.dsx.") {
			return true
		}
	}
	return false
}

func isBuilderResource(record *state.ResourceRecord, observed *runtime.ResourceSnapshot) bool {
	if observed != nil && (observed.Name == "buildkit" || strings.HasPrefix(observed.Name, "container-builder")) {
		return true
	}
	return record != nil && (record.Name == "buildkit" || strings.HasPrefix(record.Name, "container-builder"))
}
