package state

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
)

func TestManifestV2ValidatesWorkspaceGitAndIndependentVolumes(t *testing.T) {
	manifest := manifestV2Fixture(t)
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	roles := map[string]bool{}
	for _, resource := range manifest.Resources {
		roles[resource.Role] = true
	}
	for _, role := range []string{"source", "auth", "session", "deps", "service"} {
		if !roles[role] {
			t.Fatalf("missing independent %s volume", role)
		}
	}
	manifest.Git[0].WorkspaceBranch = "dsx/other"
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("accepted workspace branch outside workspace namespace")
	}
}

func TestManifestAWSGrantDefaultsOffAndRoundTrips(t *testing.T) {
	manifest := manifestV2Fixture(t)
	if manifest.AWSGrant != nil {
		t.Fatalf("default AWS grant = %#v, want absent", manifest.AWSGrant)
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("default-off manifest rejected: %v", err)
	}
	defaultData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(defaultData), `"aws_grant"`) {
		t.Fatalf("default manifest persisted an AWS grant: %s", defaultData)
	}

	manifest.AWSGrant = &AWSGrantRecord{Enabled: true}
	manifest.Operation = "aws-enable"
	enabledData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := DecodeManifest(enabledData)
	if err != nil {
		t.Fatalf("decode enabled AWS grant: %v", err)
	}
	if enabled.AWSGrant == nil || !enabled.AWSGrant.Enabled || enabled.Operation != "aws-enable" {
		t.Fatalf("enabled AWS grant round-trip = %#v operation %q", enabled.AWSGrant, enabled.Operation)
	}

	enabled.AWSGrant.Enabled = false
	enabled.Operation = "aws-disable"
	disabledData, err := json.Marshal(enabled)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := DecodeManifest(disabledData)
	if err != nil {
		t.Fatalf("decode disabled AWS grant: %v", err)
	}
	if disabled.AWSGrant == nil || disabled.AWSGrant.Enabled || disabled.Operation != "aws-disable" {
		t.Fatalf("disabled AWS grant round-trip = %#v operation %q", disabled.AWSGrant, disabled.Operation)
	}
}

func TestManifestAWSGrantJSONIsStrictAndCurrentOnly(t *testing.T) {
	manifest := manifestV2Fixture(t)
	manifest.AWSGrant = &AWSGrantRecord{Enabled: true}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	withUnknownGrantField := strings.Replace(string(data), `"aws_grant":{"enabled":true}`, `"aws_grant":{"enabled":true,"secret":"forbidden"}`, 1)
	if withUnknownGrantField == string(data) {
		t.Fatal("test did not inject the unknown AWS grant field")
	}
	if _, err := DecodeManifest([]byte(withUnknownGrantField)); err == nil {
		t.Fatal("manifest accepted an unknown AWS grant field")
	}

	legacy := manifest
	legacy.Version = LegacyManifestVersion
	legacy.Legacy = true
	if err := ValidateLegacyManifestForCleanup(legacy); err == nil {
		t.Fatal("legacy cleanup accepted an AWS grant")
	}
}

func TestManifestOperationAllowlistIsExact(t *testing.T) {
	allowed := []string{"", "create", "open", "start", "stop", "restart", "update", "remove", "capture", "aws-enable", "aws-disable"}
	for _, operation := range allowed {
		manifest := manifestV2Fixture(t)
		manifest.Operation = operation
		switch operation {
		case "aws-enable":
			manifest.AWSGrant = &AWSGrantRecord{Enabled: true}
		case "aws-disable":
			manifest.AWSGrant = &AWSGrantRecord{}
		}
		if err := ValidateManifest(manifest); err != nil {
			t.Fatalf("allowed operation %q rejected: %v", operation, err)
		}
	}
	for _, operation := range []string{"aws", "aws_enable", "AWS-enable", "aws-enable ", "clean"} {
		manifest := manifestV2Fixture(t)
		manifest.Operation = operation
		if err := ValidateManifest(manifest); err == nil {
			t.Fatalf("unlisted operation %q accepted", operation)
		}
	}
	for _, test := range []struct {
		operation string
		grant     *AWSGrantRecord
	}{
		{operation: "aws-enable"},
		{operation: "aws-enable", grant: &AWSGrantRecord{}},
		{operation: "aws-disable"},
		{operation: "aws-disable", grant: &AWSGrantRecord{Enabled: true}},
	} {
		manifest := manifestV2Fixture(t)
		manifest.Operation = test.operation
		manifest.AWSGrant = test.grant
		if err := ValidateManifest(manifest); err == nil {
			t.Fatalf("operation %q accepted mismatched AWS grant %#v", test.operation, test.grant)
		}
	}

	manifest := manifestV2Fixture(t)
	manifest.Operation = "aws-enable"
	manifest.AWSGrant = &AWSGrantRecord{Enabled: true}
	manifest.State = model.WorkspaceState("aws-enabled")
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("AWS operation bypassed workspace state validation")
	}
}

func TestManifestActiveSessionIsBoundedAndIdentityChecked(t *testing.T) {
	manifest := manifestV2Fixture(t)
	manifest.ActiveSession = &SessionRecord{
		SessionID: model.RunID("01890f5c-7b00-7000-8000-000000000090"),
		Kind:      "agent", Agent: "codex", StartedAt: manifest.UpdatedAt,
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("valid active session rejected: %v", err)
	}
	manifest.ActiveSession.BrowserResource = "missing-browser"
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("active session accepted a missing browser ownership record")
	}
	manifest.ActiveSession = &SessionRecord{
		SessionID: model.RunID("01890f5c-7b00-7000-8000-000000000091"),
		Kind:      "open", Agent: "codex", StartedAt: manifest.UpdatedAt,
	}
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("open session accepted agent-specific fields")
	}
}

func TestDecodeLegacyManifestIsCleanupOnly(t *testing.T) {
	current := manifestV2Fixture(t)
	legacyResources := append([]ResourceRecord(nil), current.Resources...)
	for index := range legacyResources {
		legacyResources[index].Labels = LegacyResourceOwnershipLabels(current.ProjectID, current.Workspace, current.RunID, legacyResources[index].Kind, legacyResources[index].Role)
	}
	wire := legacyManifestV1{
		Version: LegacyManifestVersion, Generation: 1, ProjectID: current.ProjectID,
		CanonicalRoot: current.CanonicalRoot, Sandbox: current.Workspace, RunID: current.RunID,
		Mode: "clone", PlanHash: current.PlanHash, State: model.StateStopped, Operation: "clean",
		Resources: legacyResources, CreatedAt: current.CreatedAt, UpdatedAt: current.UpdatedAt,
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	legacyWithGrant := strings.TrimSuffix(string(data), "}") + `,"aws_grant":{"enabled":true}}`
	if _, err := DecodeManifest([]byte(legacyWithGrant)); err == nil {
		t.Fatal("legacy manifest accepted an AWS grant field")
	}
	if !decoded.Legacy || decoded.Version != LegacyManifestVersion || decoded.Workspace != current.Workspace {
		t.Fatalf("legacy decode = %#v", decoded)
	}
	if err := ValidateManifest(decoded); err == nil {
		t.Fatal("legacy manifest was accepted for current lifecycle")
	}
	decoded.Resources[0].Labels[0].Value = "false"
	if err := ValidateLegacyManifestForCleanup(decoded); err == nil {
		t.Fatal("legacy cleanup accepted mismatched ownership labels")
	}
}

func manifestV2Fixture(t *testing.T) Manifest {
	t.Helper()
	root := t.TempDir()
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := model.WorkspaceName("review")
	runID := model.RunID("01890f5c-7b00-7000-8000-000000000001")
	manifest := Manifest{
		Version: ManifestVersion, Generation: 1, ProjectID: projectID, CanonicalRoot: root,
		Workspace: workspace, RunID: runID, PlanHash: strings.Repeat("a", 64), DefaultAgent: "codex",
		State: model.StatePlanned, Operation: "create", CreatedAt: time.Unix(1_700_000_000, 0).UTC(), UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	for _, item := range []struct{ kind, role string }{{"network", "network"}, {"volume", "source"}, {"volume", "auth"}, {"volume", "session"}, {"volume", "deps"}, {"volume", "service"}, {"workspace", "workspace"}} {
		name, nameErr := CanonicalResourceName(root, workspace, item.role)
		if nameErr != nil {
			t.Fatal(nameErr)
		}
		manifest.Resources = append(manifest.Resources, ResourceRecord{Kind: item.kind, Role: item.role, Name: name, ExpectedID: name, Labels: ResourceOwnershipLabels(projectID, workspace, runID, item.kind, item.role)})
	}
	manifest.Git = []GitRecord{{
		Repository: "workspace", HostPath: root, GuestPath: "/workspace", Identity: manifestRepositoryIdentity(root),
		SourceBranch: "refs/heads/main", SourceRevision: strings.Repeat("1", 40), TrackedFingerprint: strings.Repeat("2", 64),
		WorkspaceBranch: "dsx/review", SourceBundleDigest: strings.Repeat("3", 64),
	}}
	return manifest
}

func manifestRepositoryIdentity(root string) gitx.RepositoryIdentity {
	return gitx.RepositoryIdentity{
		ApprovedRoot: manifestPhysicalIdentity(root),
		Worktree:     manifestPhysicalIdentity(root),
		GitDir:       manifestPhysicalIdentity(filepath.Join(root, ".git")),
	}
}

func manifestPhysicalIdentity(value string) gitx.PhysicalPathIdentity {
	components := []gitx.PathComponentIdentity{{Path: string(filepath.Separator), Device: 1, Inode: 1}}
	current := string(filepath.Separator)
	for index, part := range strings.Split(strings.TrimPrefix(value, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		components = append(components, gitx.PathComponentIdentity{Path: current, Device: 1, Inode: uint64(index + 2)})
	}
	return gitx.PhysicalPathIdentity{CanonicalPath: value, Components: components}
}
