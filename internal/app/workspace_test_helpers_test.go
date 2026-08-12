package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/state"
)

func cloneWorkspaceTestManifest(manifest state.Manifest) state.Manifest {
	clone := manifest
	if manifest.AWSGrant != nil {
		grant := *manifest.AWSGrant
		clone.AWSGrant = &grant
	}
	return clone
}

func configureHostDefaultAWS(t *testing.T, fixture *workspaceFixture, manager *recordingHostAWSManager) {
	t.Helper()
	directory := filepath.Join(canonicalTemporaryDirectory(t), "aws")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config"), []byte("[default]\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "credentials"), []byte("[default]\naws_access_key_id = test-key\naws_secret_access_key = test-secret\naws_session_token = test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authority, err := bridge.ResolveHostAWSDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	fixture.execution.AWS = plan.AWSCapability{
		Mode: plan.AWSModeHostDefault, SourceDirectory: authority.CanonicalPath, SourceIdentity: authority.Identity,
		Destination: plan.AWSGuestDestination, ReadOnly: true, EligibleProfile: plan.AWSDefaultProfile,
		WorkspaceDefaultEnabled: false, AuthorityModel: plan.AWSAuthorityDynamicHostDefault,
	}
	fixture.service.hostAWS = manager
}
