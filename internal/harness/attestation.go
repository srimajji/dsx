package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	// BuildAttestationPath is a root-owned, read-only copy of the frozen image
	// build input installed in the standard agent image.
	BuildAttestationPath = "/usr/local/share/dsx/harnesses.lock.json"
	// MaxBuildAttestationBytes bounds guest output before it is parsed on the host.
	MaxBuildAttestationBytes = 8 << 10
	// BuildAttestationDigest pins the exact bytes copied by images/agent/Containerfile.
	BuildAttestationDigest = "sha256:219209eda5f77b364f8270de14e6c5deffe1573f63edcf02b46dd16c00bdbda6"
)

type buildAttestation struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Platform      string                 `json:"platform"`
	Harnesses     []buildAttestationItem `json:"harnesses"`
}

type buildAttestationItem struct {
	Name           Name   `json:"name"`
	Version        string `json:"version"`
	Source         string `json:"source"`
	UpstreamDigest string `json:"upstreamDigest"`
	BuildSHA256    string `json:"buildSha256"`
	Executable     string `json:"executable"`
}

// ValidateBuildAttestation proves that bounded guest bytes are the exact lock
// manifest baked into the standard image and that the selected adapter maps to
// its frozen entry. Harness executable output is deliberately not involved.
func ValidateBuildAttestation(data []byte, name Name, artifact PinnedArtifact) error {
	if len(data) == 0 {
		return fmt.Errorf("harness build attestation is empty")
	}
	if len(data) > MaxBuildAttestationBytes {
		return fmt.Errorf("harness build attestation exceeds %d bytes", MaxBuildAttestationBytes)
	}
	sum := sha256.Sum256(data)
	if actual := "sha256:" + hex.EncodeToString(sum[:]); actual != BuildAttestationDigest {
		return fmt.Errorf("harness build attestation digest is %q, want %q", actual, BuildAttestationDigest)
	}
	var lock buildAttestation
	if err := json.Unmarshal(data, &lock); err != nil {
		return fmt.Errorf("decode harness build attestation: %w", err)
	}
	if lock.SchemaVersion != 1 || lock.Platform != "linux/arm64" {
		return fmt.Errorf("unsupported harness build attestation schema or platform")
	}
	if len(lock.Harnesses) != 4 {
		return fmt.Errorf("harness build attestation contains %d artifacts, want 4", len(lock.Harnesses))
	}
	seen := make(map[Name]struct{}, len(lock.Harnesses))
	var selected *buildAttestationItem
	for index := range lock.Harnesses {
		item := &lock.Harnesses[index]
		if _, err := ParseName(string(item.Name)); err != nil {
			return fmt.Errorf("harness build attestation entry %d: %w", index, err)
		}
		if _, duplicate := seen[item.Name]; duplicate {
			return fmt.Errorf("duplicate harness build attestation entry %q", item.Name)
		}
		seen[item.Name] = struct{}{}
		if len(item.BuildSHA256) != 64 {
			return fmt.Errorf("harness build attestation entry %q has an invalid build digest", item.Name)
		}
		if _, err := hex.DecodeString(item.BuildSHA256); err != nil {
			return fmt.Errorf("harness build attestation entry %q has an invalid build digest", item.Name)
		}
		if item.Name == name {
			selected = item
		}
	}
	if selected == nil {
		return fmt.Errorf("harness build attestation has no entry for %q", name)
	}
	if selected.Version != artifact.Version || selected.Source != artifact.Source || selected.UpstreamDigest != artifact.Digest || selected.Executable != artifact.Executable {
		return fmt.Errorf("harness %q artifact does not match its frozen build attestation entry", name)
	}
	return nil
}
