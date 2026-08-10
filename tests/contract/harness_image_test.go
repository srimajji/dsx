package contract_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/harness/catalog"
)

type harnessImageLock struct {
	SchemaVersion int    `json:"schemaVersion"`
	Platform      string `json:"platform"`
	Base          struct {
		Reference string `json:"reference"`
	} `json:"base"`
	Harnesses []struct {
		Name           string `json:"name"`
		Version        string `json:"version"`
		Source         string `json:"source"`
		UpstreamDigest string `json:"upstreamDigest"`
		BuildSHA256    string `json:"buildSha256"`
		Executable     string `json:"executable"`
	} `json:"harnesses"`
}

func TestHarnessImageLockMatchesAdaptersAndBuildRecipe(t *testing.T) {
	root := filepath.Join("..", "..")
	data, err := os.ReadFile(filepath.Join(root, "images", "agent", "harnesses.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock harnessImageLock
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		t.Fatal(err)
	}
	if lock.SchemaVersion != 1 || lock.Platform != "linux/arm64" || !strings.Contains(lock.Base.Reference, "@sha256:") {
		t.Fatalf("invalid harness image lock header: %#v", lock)
	}
	byName := make(map[harness.Name]harness.PinnedArtifact)
	for _, adapter := range catalog.All() {
		byName[adapter.Name()] = adapter.Version()
	}
	containerfile, err := os.ReadFile(filepath.Join(root, "images", "agent", "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	seen := make([]string, 0, len(lock.Harnesses))
	for _, entry := range lock.Harnesses {
		name, err := harness.ParseName(entry.Name)
		if err != nil {
			t.Fatal(err)
		}
		artifact, found := byName[name]
		if !found || artifact.Version != entry.Version || artifact.Source != entry.Source || artifact.Digest != entry.UpstreamDigest || artifact.Executable != entry.Executable {
			t.Fatalf("lock mismatch for %q: entry=%#v adapter=%#v", name, entry, artifact)
		}
		if len(entry.BuildSHA256) != 64 || !strings.Contains(string(containerfile), "--checksum=sha256:"+entry.BuildSHA256+" "+entry.Source) {
			t.Fatalf("build recipe does not checksum %q", name)
		}
		seen = append(seen, entry.Name)
	}
	sort.Strings(seen)
	if strings.Join(seen, ",") != "claude,codex,omp,opencode" {
		t.Fatalf("locked harnesses = %v", seen)
	}
	if !strings.Contains(string(containerfile), "FROM "+lock.Base.Reference) {
		t.Fatal("Containerfile base does not match lock")
	}
}
