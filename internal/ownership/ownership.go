package ownership

import (
	"fmt"
	"strings"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

const (
	ManagedLabel        = state.OwnershipManagedLabel
	ProjectLabel        = state.OwnershipProjectLabel
	WorkspaceLabel      = state.OwnershipWorkspaceLabel
	RunLabel            = state.OwnershipRunLabel
	KindLabel           = state.OwnershipKindLabel
	RoleLabel           = state.OwnershipRoleLabel
	ContractLabel       = state.OwnershipContractLabel
	LegacySandboxLabel  = state.LegacySandboxLabel
	ContractValue       = state.OwnershipContractValue
	LegacyContractValue = state.LegacyContractValue
)

var ownershipLabelOrder = [...]string{
	ManagedLabel,
	ContractLabel,
	ProjectLabel,
	WorkspaceLabel,
	RunLabel,
	KindLabel,
	RoleLabel,
}

var legacyOwnershipLabelOrder = [...]string{
	ManagedLabel,
	ContractLabel,
	ProjectLabel,
	LegacySandboxLabel,
	RunLabel,
	KindLabel,
	RoleLabel,
}

type Identity struct {
	ProjectID     model.ProjectID
	CanonicalRoot string
	Workspace     model.WorkspaceName
	RunID         model.RunID
	Kind          runtime.ResourceKind
	Role          string
}

func NewIdentity(projectID model.ProjectID, canonicalRoot string, workspace model.WorkspaceName, runID model.RunID, kind runtime.ResourceKind, role string) (Identity, error) {
	identity := Identity{
		ProjectID: projectID, CanonicalRoot: canonicalRoot, Workspace: workspace,
		RunID: runID, Kind: kind, Role: role,
	}
	if err := identity.Validate(); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func (identity Identity) Validate() error {
	if _, err := model.ParseProjectID(string(identity.ProjectID)); err != nil {
		return fmt.Errorf("ownership project: %w", err)
	}
	derivedProjectID, err := model.NewProjectID(identity.CanonicalRoot)
	if err != nil || derivedProjectID != identity.ProjectID {
		return fmt.Errorf("ownership canonical project root does not match project ID")
	}
	if _, err := model.ParseWorkspaceName(string(identity.Workspace)); err != nil {
		return fmt.Errorf("ownership workspace: %w", err)
	}
	if _, err := model.ParseRunID(string(identity.RunID)); err != nil {
		return fmt.Errorf("ownership run: %w", err)
	}
	switch identity.Kind {
	case runtime.ResourceWorkspace, runtime.ResourceBrowser, runtime.ResourceNetwork, runtime.ResourceVolume:
	default:
		return fmt.Errorf("unsupported ownership resource kind %q", identity.Kind)
	}
	if _, err := runtime.CanonicalResourceName(identity.CanonicalRoot, identity.Workspace, identity.Role); err != nil {
		return fmt.Errorf("ownership resource name: %w", err)
	}
	return nil
}

func (identity Identity) Name() string {
	name, _ := runtime.CanonicalResourceName(identity.CanonicalRoot, identity.Workspace, identity.Role)
	return name
}

func (identity Identity) Labels() []runtime.Label {
	recordLabels := state.ResourceOwnershipLabels(identity.ProjectID, identity.Workspace, identity.RunID, string(identity.Kind), identity.Role)
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
		Labels:     state.ResourceOwnershipLabels(identity.ProjectID, identity.Workspace, identity.RunID, string(identity.Kind), identity.Role),
	}
}

type Outcome string

const (
	OutcomeOwned     Outcome = "owned"
	OutcomeLegacy    Outcome = "legacy"
	OutcomeOrphaned  Outcome = "orphaned"
	OutcomeAmbiguous Outcome = "ambiguous"
	OutcomeForeign   Outcome = "foreign"
	OutcomeExcluded  Outcome = "excluded"
)

type Classification struct {
	Outcome       Outcome
	Reason        string
	DeleteAllowed bool
	AdoptAllowed  bool
	Legacy        bool
}

// Classify authorizes current resources for lifecycle use only when the
// manifest and runtime corroborate one another exactly. A legacy v1 resource
// may be authorized for explicit cleanup, but never for adoption or lifecycle
// operations in the current workspace model.
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
				return Classification{Outcome: OutcomeAmbiguous, Reason: "DSX-like runtime name has no corroborating ownership labels"}
			}
			return Classification{Outcome: OutcomeForeign, Reason: "runtime resource has no DSX ownership evidence"}
		}
		labels, err := runtimeLabelMap(observed.Labels)
		if err != nil {
			return Classification{Outcome: OutcomeAmbiguous, Reason: err.Error()}
		}
		identity, legacy, err := identityFromLabels(labels)
		if err != nil || observed.Kind != identity.Kind {
			return Classification{Outcome: OutcomeAmbiguous, Reason: "runtime ownership evidence is incomplete or inconsistent"}
		}
		if legacy {
			return Classification{Outcome: OutcomeLegacy, Reason: "legacy runtime resource has no corroborating manifest", Legacy: true}
		}
		return Classification{Outcome: OutcomeOrphaned, Reason: "runtime resource has no corroborating manifest"}
	}
	if observed == nil {
		return Classification{Outcome: OutcomeOrphaned, Reason: "manifest resource is absent from the runtime"}
	}
	legacy, err := validateRecord(*record)
	if err != nil {
		return Classification{Outcome: OutcomeAmbiguous, Reason: "invalid manifest ownership: " + err.Error()}
	}
	expectedID := record.RuntimeID
	if expectedID == "" {
		expectedID = record.ExpectedID
	}
	if observed.ID == "" || expectedID == "" || string(observed.ID) != expectedID {
		return Classification{Outcome: OutcomeAmbiguous, Reason: "runtime identity does not match manifest", Legacy: legacy}
	}
	if observed.Name != record.Name || string(observed.Kind) != record.Kind {
		return Classification{Outcome: OutcomeAmbiguous, Reason: "runtime name or kind does not match manifest", Legacy: legacy}
	}
	if err := compareLabels(record.Labels, observed.Labels); err != nil {
		return Classification{Outcome: OutcomeAmbiguous, Reason: err.Error(), Legacy: legacy}
	}
	if record.Deleted {
		return Classification{Outcome: OutcomeAmbiguous, Reason: "manifest marks a still-present resource deleted", Legacy: legacy}
	}
	if legacy {
		return Classification{
			Outcome: OutcomeLegacy, Reason: "legacy manifest and runtime ownership corroborate for cleanup only",
			DeleteAllowed: true, Legacy: true,
		}
	}
	reason := "manifest and runtime ownership corroborate"
	if !record.Created {
		reason = "write-ahead intent and runtime ownership corroborate"
	}
	return Classification{Outcome: OutcomeOwned, Reason: reason, DeleteAllowed: true, AdoptAllowed: true}
}

func validateRecord(record state.ResourceRecord) (bool, error) {
	labels, err := stateLabelMap(record.Labels)
	if err != nil {
		return false, err
	}
	identity, legacy, err := identityFromLabels(labels)
	if err != nil {
		return legacy, err
	}
	if record.ExpectedID != record.Name || record.Kind != string(identity.Kind) || record.Role != identity.Role {
		return legacy, fmt.Errorf("record fields do not match ownership labels")
	}
	if record.Created && record.RuntimeID != record.ExpectedID {
		return legacy, fmt.Errorf("created record runtime identity does not match its write-ahead identity")
	}
	if !record.Created && record.RuntimeID != "" {
		return legacy, fmt.Errorf("uncreated record contains a runtime identity")
	}
	return legacy, nil
}

type labelIdentity struct {
	ProjectID model.ProjectID
	Workspace model.WorkspaceName
	RunID     model.RunID
	Kind      runtime.ResourceKind
	Role      string
}

func identityFromLabels(labels map[string]string) (labelIdentity, bool, error) {
	legacy := labels[ContractLabel] == LegacyContractValue
	order := ownershipLabelOrder[:]
	workspaceLabel := WorkspaceLabel
	contractValue := ContractValue
	if legacy {
		order = legacyOwnershipLabelOrder[:]
		workspaceLabel = LegacySandboxLabel
		contractValue = LegacyContractValue
	}
	for _, key := range order {
		if _, exists := labels[key]; !exists {
			return labelIdentity{}, legacy, fmt.Errorf("missing ownership label %q", key)
		}
	}
	if len(labels) != len(order) {
		return labelIdentity{}, legacy, fmt.Errorf("ownership must contain exactly %d labels", len(order))
	}
	for key := range labels {
		if strings.HasPrefix(key, "dev.dsx.") && !containsLabel(order, key) {
			return labelIdentity{}, legacy, fmt.Errorf("unknown DSX ownership label %q", key)
		}
	}
	if labels[ManagedLabel] != "true" || labels[ContractLabel] != contractValue {
		return labelIdentity{}, legacy, fmt.Errorf("managed or contract label is invalid")
	}
	projectID, err := model.ParseProjectID(labels[ProjectLabel])
	if err != nil {
		return labelIdentity{}, legacy, err
	}
	workspace, err := model.ParseWorkspaceName(labels[workspaceLabel])
	if err != nil {
		return labelIdentity{}, legacy, err
	}
	runID, err := model.ParseRunID(labels[RunLabel])
	if err != nil {
		return labelIdentity{}, legacy, err
	}
	kind := runtime.ResourceKind(labels[KindLabel])
	switch kind {
	case runtime.ResourceWorkspace, runtime.ResourceBrowser, runtime.ResourceNetwork, runtime.ResourceVolume:
	default:
		return labelIdentity{}, legacy, fmt.Errorf("unsupported ownership resource kind %q", kind)
	}
	role := labels[RoleLabel]
	if _, err := model.ParseWorkspaceName(role); err != nil || (!legacy && len(role) > runtime.MaxResourceRoleBytes) {
		return labelIdentity{}, legacy, fmt.Errorf("invalid ownership role %q", role)
	}
	return labelIdentity{ProjectID: projectID, Workspace: workspace, RunID: runID, Kind: kind, Role: role}, legacy, nil
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
	_, expectedLegacy, err := identityFromLabels(expectedMap)
	if err != nil {
		return fmt.Errorf("manifest ownership labels: %w", err)
	}
	_, observedLegacy, err := identityFromLabels(observedMap)
	if err != nil {
		return fmt.Errorf("runtime ownership labels: %w", err)
	}
	if expectedLegacy != observedLegacy || len(expectedMap) != len(observedMap) {
		return fmt.Errorf("runtime ownership contract does not match manifest")
	}
	for key, value := range expectedMap {
		if observedMap[key] != value {
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

func containsLabel(labels []string, key string) bool {
	for _, known := range labels {
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
