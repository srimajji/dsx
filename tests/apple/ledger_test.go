package apple_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/srimajji/dsx/internal/state"
)

const (
	faultCleanupOptIn       = "DSX_RUN_APPLE_FAULT_CLEANUP"
	faultCleanupEvidenceDir = "DSX_CI_EVIDENCE_DIR"
	faultCleanupRunID       = "DSX_CI_RUN_ID"
	faultCleanupRunLabel    = "DSX_CI_RUN_LABEL"
	faultCleanupEvidence    = "dsx-080-fault-cleanup.json"
)

type appleInventoryEvidence struct {
	Containers     string `json:"containers"`
	Networks       string `json:"networks"`
	Volumes        string `json:"volumes"`
	Builder        string `json:"builder"`
	DefaultNetwork string `json:"default_network"`
	SHA256         string `json:"sha256"`
}

type appleResourceEvidence struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Project string `json:"project"`
	Sandbox string `json:"sandbox"`
	Run     string `json:"run"`
	Role    string `json:"role"`
}

type appleScenarioEvidence struct {
	Name      string                  `json:"name"`
	Outcome   string                  `json:"outcome"`
	Resources []appleResourceEvidence `json:"resources"`
}

type appleRuntimeEvidence struct {
	HostOS          string `json:"host_os"`
	HostArch        string `json:"host_arch"`
	CLIVersion      string `json:"cli_version"`
	ServerVersion   string `json:"server_version"`
	CompatibilityID string `json:"compatibility_id"`
}

func exactAppleRuntimeEvidence() appleRuntimeEvidence {
	return appleRuntimeEvidence{
		HostOS: "Darwin", HostArch: "arm64", CLIVersion: "1.2.2", ServerVersion: "1.2.2",
		CompatibilityID: "apple-container/cli-1.2.2/server-1.2.2",
	}
}

type appleFaultEvidenceBundle struct {
	Schema      string                  `json:"schema"`
	Suite       string                  `json:"suite"`
	CIRunID     string                  `json:"ci_run_id"`
	CIRunLabel  string                  `json:"ci_run_label"`
	Runtime     appleRuntimeEvidence    `json:"runtime"`
	Baseline    appleInventoryEvidence  `json:"baseline"`
	Scenarios   []appleScenarioEvidence `json:"scenarios"`
	Final       appleInventoryEvidence  `json:"final"`
	BuilderSame bool                    `json:"builder_unchanged"`
	DefaultSame bool                    `json:"default_network_unchanged"`
	Verdict     string                  `json:"verdict"`
}

// exactOwnedResource is the only deletion-authority token exposed by the Apple
// test ledger. It can only be produced after a valid manifest and the exact
// inspected runtime object agree on the complete ownership tuple.
type exactOwnedResource struct {
	manifest state.Manifest
	record   state.ResourceRecord
	observed runtime.ResourceSnapshot
}

type ledgerFakeAdapter struct {
	runtime.Adapter
	snapshots map[runtime.ResourceID]runtime.ResourceSnapshot
	deleted   []runtime.ResourceID
}

func (adapter *ledgerFakeAdapter) Inspect(_ context.Context, id runtime.ResourceID) (runtime.ResourceSnapshot, error) {
	snapshot, found := adapter.snapshots[id]
	if !found {
		return runtime.ResourceSnapshot{}, runtime.ErrResourceNotFound
	}
	return snapshot, nil
}

func (adapter *ledgerFakeAdapter) Delete(_ context.Context, snapshot runtime.ResourceSnapshot) error {
	current, found := adapter.snapshots[snapshot.ID]
	if !found || !reflect.DeepEqual(current, snapshot) {
		return errors.New("delete did not use the exact inspected snapshot")
	}
	delete(adapter.snapshots, snapshot.ID)
	adapter.deleted = append(adapter.deleted, snapshot.ID)
	return nil
}

type appleResourceLedger struct {
	byID map[runtime.ResourceID]exactOwnedResource
}

func newAppleResourceLedger() *appleResourceLedger {
	return &appleResourceLedger{byID: make(map[runtime.ResourceID]exactOwnedResource)}
}

func (ledger *appleResourceLedger) ObserveManifest(ctx context.Context, adapter runtime.Adapter, manifest state.Manifest) error {
	if ledger == nil || adapter == nil {
		return errors.New("Apple resource ledger and runtime adapter are required")
	}
	if err := state.ValidateManifest(manifest); err != nil {
		return fmt.Errorf("validate manifest before ledger admission: %w", err)
	}
	for _, record := range manifest.Resources {
		if !record.Created || record.Deleted || record.Absent {
			continue
		}
		observed, err := adapter.Inspect(ctx, runtime.ResourceID(record.RuntimeID))
		if err != nil {
			return fmt.Errorf("inspect exact ledger resource %q: %w", record.RuntimeID, err)
		}
		proof, err := authorizeExactOwnedResource(manifest, record, observed)
		if err != nil {
			return err
		}
		id := observed.ID
		if _, duplicate := ledger.byID[id]; duplicate {
			return fmt.Errorf("duplicate exact runtime ID %q in Apple ledger", id)
		}
		ledger.byID[id] = proof
	}
	return nil
}

func authorizeExactOwnedResource(manifest state.Manifest, record state.ResourceRecord, observed runtime.ResourceSnapshot) (exactOwnedResource, error) {
	if err := state.ValidateManifest(manifest); err != nil {
		return exactOwnedResource{}, fmt.Errorf("invalid deletion manifest: %w", err)
	}
	if !record.Created || record.Deleted || record.Absent || record.RuntimeID == "" {
		return exactOwnedResource{}, errors.New("resource has no live manifest-backed runtime identity")
	}
	if record.RuntimeID != record.ExpectedID || observed.ID != runtime.ResourceID(record.RuntimeID) || observed.Name != record.Name || string(observed.Kind) != record.Kind {
		return exactOwnedResource{}, errors.New("manifest and inspected exact resource identity disagree")
	}
	classification := ownership.Classify(&record, &observed)
	if classification.Outcome != ownership.OutcomeOwned || !classification.DeleteAllowed {
		return exactOwnedResource{}, fmt.Errorf("unsafe Apple resource rejected: %s", classification.Reason)
	}
	return exactOwnedResource{manifest: manifest, record: record, observed: observed}, nil
}

// DeleteExact uses no name, prefix, label query, prune, or --all operation. The
// proof is revalidated immediately before the exact-ID deletion so tests cannot
// turn stale or contradictory ledger data into deletion authority.
func (ledger *appleResourceLedger) DeleteExact(ctx context.Context, adapter runtime.Adapter, id runtime.ResourceID) error {
	if ledger == nil || adapter == nil || id == "" {
		return errors.New("exact ledger ID and runtime adapter are required")
	}
	proof, found := ledger.byID[id]
	if !found {
		return fmt.Errorf("runtime ID %q was never admitted to the exact Apple ledger", id)
	}
	current, err := adapter.Inspect(ctx, id)
	if err != nil {
		return fmt.Errorf("reinspect exact ledger resource %q: %w", id, err)
	}
	currentProof, err := authorizeExactOwnedResource(proof.manifest, proof.record, current)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(proof.observed.Labels, currentProof.observed.Labels) {
		return errors.New("runtime ownership labels changed after ledger admission")
	}
	if err := adapter.Delete(ctx, currentProof.observed); err != nil {
		return fmt.Errorf("delete exact ledger resource %q: %w", id, err)
	}
	delete(ledger.byID, id)
	return nil
}

func (ledger *appleResourceLedger) Evidence() []appleResourceEvidence {
	resources := make([]appleResourceEvidence, 0, len(ledger.byID))
	for _, proof := range ledger.byID {
		resources = append(resources, appleResourceEvidence{
			ID: string(proof.observed.ID), Kind: proof.record.Kind,
			Project: string(proof.manifest.ProjectID), Sandbox: string(proof.manifest.Sandbox),
			Run: string(proof.manifest.RunID), Role: proof.record.Role,
		})
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].ID < resources[j].ID })
	return resources
}
func validateScenarioEvidence(scenarios []appleScenarioEvidence) error {
	names := make(map[string]struct{}, len(scenarios))
	resourceIDs := make(map[string]struct{})
	for _, scenario := range scenarios {
		if scenario.Name == "" {
			return errors.New("evidence scenario name is empty")
		}
		if _, duplicate := names[scenario.Name]; duplicate {
			return fmt.Errorf("duplicate evidence scenario %q", scenario.Name)
		}
		names[scenario.Name] = struct{}{}
		if scenario.Outcome != "passed" && scenario.Outcome != "failed" {
			return fmt.Errorf("scenario %q has invalid outcome %q", scenario.Name, scenario.Outcome)
		}
		for _, resource := range scenario.Resources {
			projectID, projectErr := model.ParseProjectID(resource.Project)
			sandbox, sandboxErr := model.ParseSandboxName(resource.Sandbox)
			runID, runErr := model.ParseRunID(resource.Run)
			if projectErr != nil || sandboxErr != nil || runErr != nil {
				return fmt.Errorf("scenario %q resource %q has invalid ownership tuple", scenario.Name, resource.ID)
			}
			if resource.ID != state.CanonicalResourceName(projectID, sandbox, resource.Role) || resource.Kind == "" || runID == "" {
				return fmt.Errorf("scenario %q resource %q is not a canonical exact identity", scenario.Name, resource.ID)
			}
			if _, duplicate := resourceIDs[resource.ID]; duplicate {
				return fmt.Errorf("resource %q appears in multiple evidence entries", resource.ID)
			}
			resourceIDs[resource.ID] = struct{}{}
		}
	}
	return nil
}

func inventoryEvidence(inventory coreInventory) (appleInventoryEvidence, error) {
	defaultNetwork, err := namedJSONObject(inventory.Networks, "default")
	if err != nil {
		return appleInventoryEvidence{}, fmt.Errorf("attest Apple default network: %w", err)
	}
	hash := sha256.New()
	for _, value := range []string{inventory.Containers, inventory.Networks, inventory.Volumes, inventory.Builder, defaultNetwork} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return appleInventoryEvidence{
		Containers: inventory.Containers, Networks: inventory.Networks, Volumes: inventory.Volumes,
		Builder: inventory.Builder, DefaultNetwork: defaultNetwork, SHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func namedJSONObject(canonicalJSON, name string) (string, error) {
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(canonicalJSON), &rows); err != nil {
		return "", err
	}
	for _, raw := range rows {
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			return "", err
		}
		candidate, _ := row["name"].(string)
		if candidate == "" {
			candidate, _ = row["id"].(string)
		}
		if candidate != name {
			continue
		}
		canonical, err := json.Marshal(row)
		if err != nil {
			return "", err
		}
		return string(canonical), nil
	}
	return "", fmt.Errorf("named object %q is absent", name)
}

func writeFaultEvidence(directory string, bundle appleFaultEvidenceBundle) error {
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("DSX_CI_EVIDENCE_DIR must be a clean absolute path")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("DSX_CI_EVIDENCE_DIR must be an existing non-symlink directory")
	}
	sort.Slice(bundle.Scenarios, func(i, j int) bool { return bundle.Scenarios[i].Name < bundle.Scenarios[j].Name })
	for index := range bundle.Scenarios {
		sort.Slice(bundle.Scenarios[index].Resources, func(i, j int) bool {
			return bundle.Scenarios[index].Resources[i].ID < bundle.Scenarios[index].Resources[j].ID
		})
	}
	encoded, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(directory, ".dsx-080-evidence-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, filepath.Join(directory, faultCleanupEvidence))
}

func TestAppleLedgerGuardRejectsUnsafeDeletionAuthority(t *testing.T) {
	manifest, record, snapshot := ledgerContractFixture(t)
	if _, err := authorizeExactOwnedResource(manifest, record, snapshot); err != nil {
		t.Fatalf("complete exact ownership proof rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*state.Manifest, *state.ResourceRecord, *runtime.ResourceSnapshot)
	}{
		{name: "uncreated", mutate: func(_ *state.Manifest, record *state.ResourceRecord, _ *runtime.ResourceSnapshot) {
			record.Created = false
			record.RuntimeID = ""
		}},
		{name: "different exact ID", mutate: func(_ *state.Manifest, _ *state.ResourceRecord, snapshot *runtime.ResourceSnapshot) {
			snapshot.ID = "unrelated"
		}},
		{name: "different kind", mutate: func(_ *state.Manifest, _ *state.ResourceRecord, snapshot *runtime.ResourceSnapshot) {
			snapshot.Kind = runtime.ResourceVolume
		}},
		{name: "incomplete labels", mutate: func(_ *state.Manifest, _ *state.ResourceRecord, snapshot *runtime.ResourceSnapshot) {
			snapshot.Labels = snapshot.Labels[:len(snapshot.Labels)-1]
		}},
		{name: "conflicting run", mutate: func(_ *state.Manifest, _ *state.ResourceRecord, snapshot *runtime.ResourceSnapshot) {
			for index := range snapshot.Labels {
				if snapshot.Labels[index].Key == state.OwnershipRunLabel {
					snapshot.Labels[index].Value = "01890f5c-7b00-7000-8000-000000000099"
				}
			}
		}},
		{name: "conflicting sandbox", mutate: func(_ *state.Manifest, _ *state.ResourceRecord, snapshot *runtime.ResourceSnapshot) {
			for index := range snapshot.Labels {
				if snapshot.Labels[index].Key == state.OwnershipSandboxLabel {
					snapshot.Labels[index].Value = "other"
				}
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidateManifest := manifest
			candidateRecord := record
			candidateSnapshot := snapshot
			candidateManifest.Resources = append([]state.ResourceRecord(nil), manifest.Resources...)
			candidateRecord.Labels = append([]state.OwnershipLabel(nil), record.Labels...)
			candidateSnapshot.Labels = append([]runtime.Label(nil), snapshot.Labels...)
			test.mutate(&candidateManifest, &candidateRecord, &candidateSnapshot)
			if _, err := authorizeExactOwnedResource(candidateManifest, candidateRecord, candidateSnapshot); err == nil {
				t.Fatal("unsafe deletion authority was admitted")
			}
		})
	}
	ledger := newAppleResourceLedger()
	if err := ledger.DeleteExact(context.Background(), nil, "missing"); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("unguarded deletion error = %v", err)
	}
}
func TestAppleLedgerRevalidatesImmediatelyBeforeExactDeletion(t *testing.T) {
	manifest, _, snapshot := ledgerContractFixture(t)
	adapter := &ledgerFakeAdapter{snapshots: map[runtime.ResourceID]runtime.ResourceSnapshot{snapshot.ID: snapshot}}
	ledger := newAppleResourceLedger()
	if err := ledger.ObserveManifest(context.Background(), adapter, manifest); err != nil {
		t.Fatal(err)
	}

	changed := snapshot
	changed.Labels = append([]runtime.Label(nil), snapshot.Labels...)
	changed.Labels[len(changed.Labels)-1].Value = "contradictory"
	adapter.snapshots[snapshot.ID] = changed
	if err := ledger.DeleteExact(context.Background(), adapter, snapshot.ID); err == nil {
		t.Fatal("changed ownership evidence authorized deletion")
	}
	if len(adapter.deleted) != 0 {
		t.Fatalf("unsafe deletion calls = %q", adapter.deleted)
	}

	adapter.snapshots[snapshot.ID] = snapshot
	if err := ledger.DeleteExact(context.Background(), adapter, snapshot.ID); err != nil {
		t.Fatalf("exact deletion rejected: %v", err)
	}
	if !reflect.DeepEqual(adapter.deleted, []runtime.ResourceID{snapshot.ID}) {
		t.Fatalf("exact deletions = %q", adapter.deleted)
	}
	if err := ledger.DeleteExact(context.Background(), adapter, snapshot.ID); err == nil {
		t.Fatal("repeated deletion reused consumed authority")
	}
}

func TestAppleEvidenceGoldenIsCanonicalAndSorted(t *testing.T) {
	inventory := coreInventory{
		Containers: `[]`, Networks: `[{"name":"z"},{"name":"default","labels":{"owner":"apple"}}]`,
		Volumes: `[]`, Builder: `[{"configuration":{"id":"buildkit"}}]`,
	}
	evidence, err := inventoryEvidence(inventory)
	if err != nil {
		t.Fatal(err)
	}
	bundle := appleFaultEvidenceBundle{
		Schema: "dsx.apple.fault-cleanup/v1", Suite: "DSX-080", CIRunID: "ci-1", CIRunLabel: "lane-26",
		Runtime: exactAppleRuntimeEvidence(), Baseline: evidence,
		Scenarios: []appleScenarioEvidence{
			{Name: "zeta", Outcome: "passed", Resources: []appleResourceEvidence{{ID: "z"}, {ID: "a"}}},
			{Name: "alpha", Outcome: "passed"},
		},
		Final: evidence, BuilderSame: true, DefaultSame: true, Verdict: "passed",
	}
	directory := t.TempDir()
	if err := writeFaultEvidence(directory, bundle); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(directory, faultCleanupEvidence))
	if err != nil {
		t.Fatal(err)
	}
	var decoded appleFaultEvidenceBundle
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Scenarios) != 2 || decoded.Scenarios[0].Name != "alpha" || decoded.Scenarios[1].Resources[0].ID != "a" {
		t.Fatalf("evidence order is not deterministic: %#v", decoded.Scenarios)
	}
	if !strings.HasSuffix(string(encoded), "\n") || decoded.Baseline.DefaultNetwork != `{"labels":{"owner":"apple"},"name":"default"}` {
		t.Fatalf("canonical evidence mismatch: %s", encoded)
	}
}

func ledgerContractFixture(t *testing.T) (state.Manifest, state.ResourceRecord, runtime.ResourceSnapshot) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := model.ParseSandboxName("main")
	if err != nil {
		t.Fatal(err)
	}
	runID, err := model.ParseRunID("01890f5c-7b00-7000-8000-000000000080")
	if err != nil {
		t.Fatal(err)
	}
	name := state.CanonicalResourceName(projectID, sandbox, "network")
	labels := state.ResourceOwnershipLabels(projectID, sandbox, runID, string(runtime.ResourceNetwork), "network")
	record := state.ResourceRecord{Kind: string(runtime.ResourceNetwork), Role: "network", Name: name, ExpectedID: name, RuntimeID: name, Labels: labels, Created: true}
	manifest := state.Manifest{
		Version: state.ManifestVersion, Generation: 1, ProjectID: projectID, CanonicalRoot: root,
		Sandbox: sandbox, RunID: runID, Mode: model.ModeLive, PlanHash: strings.Repeat("a", 64),
		State: model.StateRunning, Operation: "create", Resources: []state.ResourceRecord{record},
		CreatedAt: fixedEvidenceTime, UpdatedAt: fixedEvidenceTime,
	}
	runtimeLabels := make([]runtime.Label, len(labels))
	for index, label := range labels {
		runtimeLabels[index] = runtime.Label{Key: label.Key, Value: label.Value}
	}
	snapshot := runtime.ResourceSnapshot{Resource: runtime.Resource{ID: runtime.ResourceID(name), Name: name, Kind: runtime.ResourceNetwork}, Labels: runtimeLabels}
	return manifest, record, snapshot
}

var fixedEvidenceTime = time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC)
