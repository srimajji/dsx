package hostcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/schema"
)

func TestDocumentedCommandHelpContract(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "global", args: []string{"help"}, want: "dsx run --name NAME --agent omp|codex|claude|opencode [--profile NAME] [--browser] --approve-config HASH -- PROMPT"},
		{name: "init", args: []string{"init", "--help"}, want: "Usage: dsx init [--root PATH]"},
		{name: "inspect", args: []string{"inspect", "--help"}, want: "Usage: dsx inspect [--format text|json] [--root PATH] [--mode live|clone] [--sandbox NAME] [--agent NAME]"},
		{name: "doctor", args: []string{"doctor", "--help"}, want: "Usage: dsx doctor [--format text|json] [--require-builder]"},
		{name: "start", args: []string{"start", "--help"}, want: "Usage: dsx start [--root PATH] --approve-config HASH"},
		{name: "stop", args: []string{"stop", "--help"}, want: "Usage: dsx stop [--root PATH] [--name NAME]"},
		{name: "list", args: []string{"list", "--help"}, want: "Usage: dsx list [--root PATH] [--format text|json]"},
		{name: "clean", args: []string{"clean", "--help"}, want: "[--purge-auth --agent NAME [--profile NAME]]"},
		{name: "shell", args: []string{"shell", "--help"}, want: "Usage: dsx shell [--root PATH] [--approve-config HASH] [--agent omp|codex|claude|opencode] [--profile NAME] [-- command args...]"},
		{name: "run", args: []string{"run", "--help"}, want: "Usage: dsx run --name NAME --agent omp|codex|claude|opencode [--profile NAME] [--browser] --approve-config HASH -- PROMPT"},
		{name: "login", args: []string{"login", "--help"}, want: "Usage: dsx login --agent omp|codex|claude|opencode --profile NAME --root PATH --approve-config HASH"},
		{name: "git", args: []string{"git", "--help"}, want: "dsx git apply NAME [--repo MEMBER] [--root PATH] [--format text|json]"},
		{name: "status", args: []string{"status", "--help"}, want: "Usage: dsx status [--root PATH] [--format text|json]"},
		{name: "logs", args: []string{"logs", "--help"}, want: "Usage: dsx logs [--root PATH] [--format text|json] PROCESS"},
		{name: "version", args: []string{"version", "--help"}, want: "Usage: dsx version [--json]"},
	}

	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if exit := Execute(context.Background(), test.args, &stdout, &stderr); exit != 0 {
				t.Fatalf("Execute(%q) exit = %d, stderr = %q", test.args, exit, stderr.String())
			}
			if !bytes.Contains(stdout.Bytes(), []byte(test.want)) {
				t.Fatalf("Execute(%q) output = %q, want contract %q", test.args, stdout.String(), test.want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("Execute(%q) stderr = %q", test.args, stderr.String())
			}
		})
	}
}

func TestConfigurationSchemaExamplesAreAccepted(t *testing.T) {
	t.Parallel()

	var root struct {
		Examples []json.RawMessage `json:"examples"`
	}
	if err := json.Unmarshal(schema.DSXConfigV1, &root); err != nil {
		t.Fatalf("decode embedded schema: %v", err)
	}
	if len(root.Examples) == 0 {
		t.Fatal("embedded schema has no configuration examples")
	}
	for index, example := range root.Examples {
		if _, diagnostics := config.ParseBytes("schema-example.json", example); len(diagnostics) != 0 {
			t.Fatalf("schema example %d diagnostics = %#v", index, diagnostics)
		}
	}
}

func TestUserGuideConfigurationExamplesAreAccepted(t *testing.T) {
	t.Parallel()

	document, err := os.ReadFile("../../docs/manual/user-guide.md")
	if err != nil {
		t.Fatalf("read user guide: %v", err)
	}
	const opening = "```jsonc\n"
	const closing = "\n```"
	remaining := string(document)
	examples := 0
	for {
		start := strings.Index(remaining, opening)
		if start < 0 {
			break
		}
		remaining = remaining[start+len(opening):]
		end := strings.Index(remaining, closing)
		if end < 0 {
			t.Fatal("user guide has an unterminated jsonc example")
		}
		example := []byte(remaining[:end])
		if _, diagnostics := config.ParseBytes("user-guide-example.jsonc", example); len(diagnostics) != 0 {
			t.Fatalf("user guide configuration example %d diagnostics = %#v", examples, diagnostics)
		}
		examples++
		remaining = remaining[end+len(closing):]
	}
	if examples != 2 {
		t.Fatalf("user guide configuration examples = %d, want 2", examples)
	}
}
