package runtime

import (
	"regexp"
	"testing"

	"github.com/srimajji/dsx/internal/model"
)

func TestCanonicalResourceNameIsReadableDeterministicAndBounded(t *testing.T) {
	workspace, err := model.ParseWorkspaceName("feature-a")
	if err != nil {
		t.Fatal(err)
	}
	root := "/Volumes/Dev/work/tracking-chrome-extension"
	first, err := CanonicalResourceName(root, workspace, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if want := "dsx-tracking-chrome-feature-a-workspace-1abbf9"; first != want {
		t.Fatalf("CanonicalResourceName() = %q, want %q", first, want)
	}
	second, err := CanonicalResourceName(root, workspace, "workspace")
	if err != nil || second != first {
		t.Fatalf("repeated name = %q, %v; want %q", second, err, first)
	}

	longWorkspace, err := model.ParseWorkspaceName("abcdefghijklmnopqrstuvwx")
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := CanonicalResourceName("/tmp/abcdefghijklmnop-extra", longWorkspace, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != MaxGeneratedResourceNameBytes {
		t.Fatalf("maximal name length = %d, want %d: %q", len(bounded), MaxGeneratedResourceNameBytes, bounded)
	}
	if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`).MatchString(bounded) {
		t.Fatalf("generated name is not runtime-safe: %q", bounded)
	}
}

func TestCanonicalResourceNameSanitizesHostileAndUnicodeFolders(t *testing.T) {
	workspace, err := model.ParseWorkspaceName("tests")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		root    string
		project string
	}{
		{root: "/tmp/UPPER___project...name", project: "upper-project-na"},
		{root: "/tmp/🔥🔥", project: "project"},
		{root: "/tmp/a\n\x1b[2Jb", project: "a-2jb"},
	} {
		name, err := CanonicalResourceName(test.root, workspace, "browser")
		if err != nil {
			t.Fatalf("CanonicalResourceName(%q): %v", test.root, err)
		}
		prefix := "dsx-" + test.project + "-tests-browser-"
		if len(name) != len(prefix)+6 || name[:len(prefix)] != prefix {
			t.Fatalf("CanonicalResourceName(%q) = %q, want prefix %q plus hash", test.root, name, prefix)
		}
	}
}

func TestCanonicalResourceNameRejectsInvalidAuthorityInputs(t *testing.T) {
	workspace, err := model.ParseWorkspaceName("feature-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		root string
		role string
	}{
		{root: "relative", role: "workspace"},
		{root: "/tmp/../tmp/project", role: "workspace"},
		{root: "/tmp/project", role: "role-too-long"},
		{root: "/tmp/project", role: "bad_role"},
	} {
		if name, err := CanonicalResourceName(test.root, workspace, test.role); err == nil {
			t.Fatalf("CanonicalResourceName(%q, %q) = %q, want error", test.root, test.role, name)
		}
	}
}

func TestCanonicalAuthLoginNameAndLabelsAreProjectScoped(t *testing.T) {
	root := "/Volumes/Dev/work/tracking-chrome-extension"
	name, err := CanonicalAuthLoginName(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if want := "dsx-tracking-chrome-auth-codex-1abbf9"; name != want {
		t.Fatalf("CanonicalAuthLoginName() = %q, want %q", name, want)
	}
	projectID, _ := model.NewProjectID(root)
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000001")
	labels, err := AuthLoginOwnershipLabels(projectID, runID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 6 {
		t.Fatalf("auth login labels = %#v", labels)
	}
	for _, label := range labels {
		if label.Key == "dev.dsx.workspace" || label.Key == "dev.dsx.sandbox" {
			t.Fatalf("auth login received workspace identity: %#v", labels)
		}
	}
	if _, err := CanonicalAuthLoginName(root, "auth-login"); err == nil {
		t.Fatal("accepted harness token longer than nine bytes")
	}
}
