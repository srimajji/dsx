package harness_test

import (
	"os"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/harness/codex"
)

func TestValidateBuildAttestationRejectsUntrustedContent(t *testing.T) {
	lock, err := os.ReadFile("../../images/agent/harnesses.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	adapter := codex.New()
	artifact := adapter.Version()
	if err := harness.ValidateBuildAttestation(lock, adapter.Name(), artifact); err != nil {
		t.Fatalf("frozen lock rejected: %v", err)
	}

	spoofed := append([]byte(nil), lock...)
	spoofed[len(spoofed)-2] ^= 1
	wrongDigest := artifact
	wrongDigest.Digest = "sha256:" + strings.Repeat("0", 64)
	cases := []struct {
		name string
		data []byte
		pin  harness.PinnedArtifact
	}{
		{name: "missing file", data: nil, pin: artifact},
		{name: "spoofed attestation", data: spoofed, pin: artifact},
		{name: "wrong lock digest", data: lock, pin: wrongDigest},
		{name: "oversized output", data: make([]byte, harness.MaxBuildAttestationBytes+1), pin: artifact},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := harness.ValidateBuildAttestation(test.data, adapter.Name(), test.pin); err == nil {
				t.Fatal("untrusted attestation accepted")
			}
		})
	}
}
