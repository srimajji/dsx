package catalog

import (
	"fmt"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/harness/claude"
	"github.com/srimajji/dsx/internal/harness/codex"
	"github.com/srimajji/dsx/internal/harness/omp"
	"github.com/srimajji/dsx/internal/harness/opencode"
)

func All() []harness.Adapter {
	return []harness.Adapter{omp.New(), codex.New(), claude.New(), opencode.New()}
}

// ValidateBuildAttestation verifies that every supported adapter maps exactly
// to the frozen lock manifest installed in the standard agent image.
func ValidateBuildAttestation(data []byte) error {
	for _, adapter := range All() {
		if err := harness.ValidateBuildAttestation(data, adapter.Name(), adapter.Version()); err != nil {
			return fmt.Errorf("%s artifact: %w", adapter.Name(), err)
		}
	}
	return nil
}
