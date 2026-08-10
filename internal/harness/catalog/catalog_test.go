package catalog

import (
	"os"
	"testing"

	"github.com/srimajji/dsx/internal/harness"
)

func TestAllContainsEverySupportedHarnessExactlyOnce(t *testing.T) {
	found := make(map[harness.Name]bool)
	for _, adapter := range All() {
		if found[adapter.Name()] {
			t.Fatalf("duplicate adapter %q", adapter.Name())
		}
		found[adapter.Name()] = true
		artifact := adapter.Version()
		if artifact.Version == "" || artifact.Source == "" || artifact.Digest == "" || artifact.Executable == "" {
			t.Fatalf("incomplete artifact for %q: %#v", adapter.Name(), artifact)
		}
	}
	for _, name := range []harness.Name{harness.OMP, harness.Codex, harness.Claude, harness.OpenCode} {
		if !found[name] {
			t.Fatalf("missing adapter %q", name)
		}
	}
}

func TestAllExactlyMatchesFrozenAgentImageLock(t *testing.T) {
	lock, err := os.ReadFile("../../../images/agent/harnesses.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateBuildAttestation(lock); err != nil {
		t.Fatal(err)
	}
}
