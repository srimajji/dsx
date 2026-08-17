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
		{name: "global", args: []string{"help"}, want: "dsx workspace create NAME"},
		{name: "global aws", args: []string{"help"}, want: "dsx aws status WORKSPACE"},
		{name: "init", args: []string{"init", "--help"}, want: "Usage: dsx init [--root PATH]"},
		{name: "inspect", args: []string{"inspect", "--help"}, want: "Usage: dsx inspect [--format text|json] [--root PATH]"},
		{name: "doctor", args: []string{"doctor", "--help"}, want: "Usage: dsx doctor [--format text|json] [--require-builder]"},
		{name: "workspace", args: []string{"workspace", "--help"}, want: "dsx workspace restart NAME"},
		{name: "workspace create snapshot", args: []string{"workspace", "--help"}, want: "dsx workspace create NAME [--root PATH] [--default-agent AGENT] [--approve-config HASH] [--snapshot] [--open]"},
		{name: "workspace update snapshot", args: []string{"workspace", "--help"}, want: "dsx workspace update NAME [--root PATH] [--snapshot]"},
		{name: "agent", args: []string{"agent", "--help"}, want: "Usage: dsx agent WORKSPACE"},
		{name: "auth", args: []string{"auth", "--help"}, want: "dsx auth purge --agent"},
		{name: "aws", args: []string{"aws", "--help"}, want: "dsx aws enable WORKSPACE"},
		{name: "aws enable", args: []string{"aws", "enable", "--help"}, want: "Usage: dsx aws enable WORKSPACE"},
		{name: "aws disable", args: []string{"aws", "disable", "--help"}, want: "Usage: dsx aws disable WORKSPACE"},
		{name: "aws status", args: []string{"aws", "status", "--help"}, want: "Usage: dsx aws status WORKSPACE"},
		{name: "git", args: []string{"git", "--help"}, want: "dsx git apply WORKSPACE"},
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
	if examples != 1 {
		t.Fatalf("user guide configuration examples = %d, want 1", examples)
	}
}
