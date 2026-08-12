package contract_test

import (
	"regexp"
	"strings"
	"testing"
)

func TestManualsDescribeOnlyNamedWorkspaceWorkflows(t *testing.T) {
	root := "../.."
	manuals := map[string]string{
		"getting started": readContractFile(t, root+"/docs/manual/getting-started.md"),
		"user guide":     readContractFile(t, root+"/docs/manual/user-guide.md"),
	}

	for name, manual := range manuals {
		name, manual := name, manual
		t.Run(name, func(t *testing.T) {
			forbidden := map[string]*regexp.Regexp{
				"old top-level lifecycle command": regexp.MustCompile(`(?m)(?:^|[[:space:]` + "`" + `])dsx (?:shell|run|start|stop|clean|list|ls)(?:[[:space:]` + "`" + `]|$)`),
				"old mode flag":                  regexp.MustCompile(`--mode(?:[ =]|$)`),
				"old sandbox flag":               regexp.MustCompile(`--sandbox(?:[ =]|$)`),
				"old profile flag":               regexp.MustCompile(`--profile(?:[ =]|$)`),
				"old auth profile config":        regexp.MustCompile(`authProfiles`),
				"creation browser config":        regexp.MustCompile(`browser\.enabled`),
				"old clone concurrency config":   regexp.MustCompile(`maxConcurrentClones`),
				"old sandbox volume scope":       regexp.MustCompile(`"scope"[[:space:]]*:[[:space:]]*"sandbox"`),
			}
			for contract, pattern := range forbidden {
				if match := pattern.FindString(manual); match != "" {
					t.Fatalf("%s contains %s %q", name, contract, match)
				}
			}
		})
	}

	combined := manuals["getting started"] + "\n" + manuals["user guide"]
	for _, required := range []string{
		"dsx workspace create feature-a",
		"dsx workspace list",
		"dsx workspace open feature-a",
		"dsx workspace start feature-a",
		"dsx workspace stop feature-a",
		"dsx workspace restart feature-a",
		"dsx workspace update feature-a",
		"dsx workspace remove feature-a",
		"dsx agent feature-a",
		"dsx auth import --agent omp",
		"dsx auth login --agent claude",
		"dsx auth refresh --agent omp",
		"dsx auth purge --agent omp",
		"dsx git status feature-a",
		"dsx git diff feature-a",
		"dsx git fetch feature-a",
		"dsx git apply feature-a",
		"agents.allowed",
		"agents.default",
		"auth.imports",
		"maxConcurrentWorkspaces",
		"git rebase --continue",
		"git rebase --abort",
		"Needs resolution",
		"Legacy — cleanup only",
		"unfetched-work guard",
		"at most 62 bytes",
		"no host source or host-home mount",
		"browser is deleted on success, error, cancellation, or terminal closure",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("manuals do not document required workspace contract %q", required)
		}
	}
}

func TestMaintainerGuideUsesNamedWorkspaceArchitecture(t *testing.T) {
	guide := readContractFile(t, "../../AGENTS.md")
	for contract, pattern := range map[string]*regexp.Regexp{
		"live-mounted architecture": regexp.MustCompile(`(?i)live-mounted`),
		"live workspace mode":       regexp.MustCompile(`(?i)live (?:workspace )?mode`),
		"clone workspace mode":      regexp.MustCompile(`(?i)clone (?:workspace )?mode`),
		"special main workspace":    regexp.MustCompile(`(?i)(?:sandbox|workspace)[[:space:]]+` + "`main`"),
		"old lifecycle file":        regexp.MustCompile(`internal/app/lifecycle\.go`),
		"old clone file":            regexp.MustCompile(`internal/app/clone\.go`),
		"old lifecycle command":     regexp.MustCompile(`(?m)(?:^|[[:space:]` + "`" + `])dsx (?:shell|run|start|stop|clean|list|ls)(?:[[:space:]` + "`" + `]|$)`),
	} {
		if match := pattern.FindString(guide); match != "" {
			t.Fatalf("AGENTS.md contains %s %q", contract, match)
		}
	}
	for _, required := range []string{
		"Every workspace is a named peer backed by a guest-owned private Git clone.",
		"The local checkout is the source and integration point, not a DSX workspace.",
		"without shared Git objects or host source/home mounts",
		"Every lifecycle and Git operation names its workspace",
		"workspace, agent, authentication, and browser-session lifecycles remain separate",
		"`internal/app/workspace.go`",
		"`internal/app/workspace_update_rebase.go`, `internal/app/workspace_git.go`",
		"workspace lock before project lock",
	} {
		if !strings.Contains(guide, required) {
			t.Errorf("AGENTS.md does not document required workspace contract %q", required)
		}
	}
}
