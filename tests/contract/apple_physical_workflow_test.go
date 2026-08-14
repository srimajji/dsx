package contract_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type physicalRunnerConfig struct {
	Schema      string `json:"schema"`
	Provisioned bool   `json:"provisioned"`
	RunnerGroup string `json:"runner_group"`
	Lanes       []struct {
		OSMajor      int      `json:"os_major"`
		Architecture string   `json:"architecture"`
		Labels       []string `json:"labels"`
	} `json:"lanes"`
	Runtime struct {
		CLI    string `json:"container_cli"`
		Server string `json:"container_api_server"`
	} `json:"runtime"`
	GitHub struct {
		ProtectedRefsOnly bool `json:"protected_refs_only"`
		PullRequest       bool `json:"pull_request_event_enabled"`
	} `json:"github"`
}

func TestPhysicalAppleWorkflowTrustAndLaneContract(t *testing.T) {
	root := filepath.Join("..", "..")
	workflow := readContractFile(t, filepath.Join(root, ".github", "workflows", "apple-physical.yml"))

	for _, required := range []string{
		"workflow_dispatch:",
		"github.ref_protected == true",
		"environment: dsx-physical-apple",
		"group: dsx-physical-apple",
		"os_major: '26'",
		"runner_label: dsx-physical-macos-26-arm64",
		"os_major: '27'",
		"runner_label: dsx-physical-macos-27-arm64",
		"actions: read",
		"contents: read",
		"persist-credentials: false",
		"if: ${{ always() }}",
		"physical-lane.sh finish",
		"actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		"retention-days: 30",
		"if-no-files-found: error",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("physical workflow is missing %q", required)
		}
	}
	if strings.Contains(workflow, "pull_request:") || strings.Contains(workflow, "pull_request_target:") {
		t.Fatal("untrusted pull-request events must not schedule the destructive workflow")
	}
	if strings.Contains(workflow, "write") {
		t.Fatal("physical workflow must not grant a write permission")
	}
	if strings.Index(workflow, "Run Apple 1.2.2 canary") > strings.Index(workflow, "Run physical Apple suites") {
		t.Fatal("runtime canary must precede the physical suites")
	}
	cleanupStep := strings.Index(workflow, "Always clean exact ownership and compare sentinels")
	if cleanupStep < 0 {
		t.Fatal("exact cleanup step is absent")
	}
	cleanupAlways := strings.Index(workflow[cleanupStep:], "if: ${{ always() }}")
	if cleanupAlways < 0 || cleanupAlways > 180 {
		t.Fatal("exact cleanup is not an unconditional always step")
	}

	pinnedAction := regexp.MustCompile(`(?m)^\s*uses:\s+[^@\s]+@([0-9a-f]{40})(?:\s+#.*)?$`)
	usesLine := regexp.MustCompile(`(?m)^\s*uses:`)
	if matches := pinnedAction.FindAllStringSubmatch(workflow, -1); len(matches) != len(usesLine.FindAllString(workflow, -1)) || len(matches) != 3 {
		t.Fatalf("all workflow actions must use immutable 40-hex revisions; found %d pinned of %d", len(matches), len(usesLine.FindAllString(workflow, -1)))
	}
}

func TestPhysicalRunnerCleanupAndQuarantineContract(t *testing.T) {
	root := filepath.Join("..", "..")
	physical := readContractFile(t, filepath.Join(root, "scripts", "runner-ops", "physical-lane.sh"))
	sweeper := readContractFile(t, filepath.Join(root, "scripts", "runner-ops", "sweep.sh"))
	cleanup := readContractFile(t, filepath.Join(root, "scripts", "runner-ops", "cleanup-owned.sh"))
	common := readContractFile(t, filepath.Join(root, "scripts", "runner-ops", "common.sh"))
	all := strings.ToLower(physical + "\n" + sweeper + "\n" + cleanup + "\n" + common)

	for _, required := range []string{
		"dsxci-${repository_name}-${github_run_id}-${github_run_attempt}-${random}",
		"create_sentinel container",
		"create_sentinel volume",
		"create_sentinel network",
		"host.lock",
		"cleanup-owned.sh",
	} {
		source := strings.ToLower(physical + "\n" + cleanup)
		if !strings.Contains(source, strings.ToLower(required)) {
			t.Fatalf("runner controller is missing %q", required)
		}
	}
	if !strings.Contains(common, "QUARANTINED.json") || !strings.Contains(common, "require_not_quarantined") {
		t.Fatal("runner quarantine must block future jobs")
	}
	if !strings.Contains(sweeper, `.status == "completed"`) || !strings.Contains(sweeper, "Authorization: Bearer $GH_TOKEN") {
		t.Fatal("sweeper must establish exact terminal GitHub run state")
	}
	if !strings.Contains(cleanup, `$labels | type == "object" and length == 7`) ||
		!strings.Contains(cleanup, `dsx.ownership/v1`) ||
		!strings.Contains(cleanup, `resource identity changed before exact deletion`) {
		t.Fatal("cleanup must require complete ownership and re-attest before exact deletion")
	}
	for _, forbidden := range []string{
		"container system stop",
		"container builder delete",
		"container network delete default",
		" delete --all",
		" prune",
	} {
		if strings.Contains(all, forbidden) {
			t.Fatalf("runner operations contain forbidden broad mutation %q", forbidden)
		}
	}
	intent := strings.Index(physical, `.sentinels += [{kind:$kind,name:$name,created:false,intent_written:true}]`)
	mutation := strings.Index(physical, `container create --name "$name"`)
	if intent < 0 || mutation < 0 || intent > mutation {
		t.Fatal("sentinel mutation must be preceded by a durable exact intent")
	}
}

func TestPhysicalRunnerConfigAndDocumentationAreExplicitlyUnexecuted(t *testing.T) {
	root := filepath.Join("..", "..")
	configData := []byte(readContractFile(t, filepath.Join(root, "runner-ops", "physical-apple.example.json")))
	var config physicalRunnerConfig
	decoder := json.NewDecoder(strings.NewReader(string(configData)))
	if err := decoder.Decode(&config); err != nil {
		t.Fatal(err)
	}
	if config.Schema != "dsx.physical-runner/v1" || config.Provisioned || config.RunnerGroup != "dsx-physical-apple" {
		t.Fatalf("unexpected physical runner config: %#v", config)
	}
	if config.Runtime.CLI != "1.2.2" || config.Runtime.Server != "1.2.2" || !config.GitHub.ProtectedRefsOnly || config.GitHub.PullRequest {
		t.Fatalf("physical runner trust/runtime contract is invalid: %#v", config)
	}
	wantLanes := map[int]string{26: "dsx-physical-macos-26-arm64", 27: "dsx-physical-macos-27-arm64"}
	if len(config.Lanes) != len(wantLanes) {
		t.Fatalf("physical lane count = %d, want %d", len(config.Lanes), len(wantLanes))
	}
	for _, lane := range config.Lanes {
		wantLabel, ok := wantLanes[lane.OSMajor]
		if !ok || lane.Architecture != "arm64" || !containsContractString(lane.Labels, wantLabel) {
			t.Fatalf("unexpected physical lane: %#v", lane)
		}
	}

	doc := readContractFile(t, filepath.Join(root, "docs", "operations", "runner-operations.md"))
	for _, statement := range []string{
		"No physical macOS 26 or macOS 27 runner has been provisioned, registered, or exercised",
		"No canary, destructive suite, sweeper, quarantine drill, or recovery drill has run here.",
		"Manual dispatch is not a trust bypass",
		"Quarantine recovery requires a human operator and an independent reviewer",
		"remain external and incomplete",
	} {
		if !strings.Contains(doc, statement) {
			t.Fatalf("runner operations documentation is missing %q", statement)
		}
	}
}

func readContractFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func containsContractString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
