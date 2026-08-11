package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONCFixtures(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"valid-minimal.jsonc", "valid-full.jsonc"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := fixture(t, name)
			validated, diagnostics := ParseBytes(name, data)
			if len(diagnostics) != 0 {
				t.Fatalf("ParseBytes() diagnostics = %#v", diagnostics)
			}
			if validated.Document.SchemaVersion != 1 || validated.ContentDigest == "" {
				t.Fatalf("ParseBytes() returned incomplete config: %#v", validated)
			}
			if validated.SourceLocations["/workspace/root"].Line == 0 {
				t.Fatal("ParseBytes() did not retain source locations")
			}
		})
	}

	t.Run("syntax", func(t *testing.T) {
		_, diagnostics := ParseBytes("invalid-syntax.jsonc", fixture(t, "invalid-syntax.jsonc"))
		assertDiagnostic(t, diagnostics, "syntax", "", true)
	})

	t.Run("bounded", func(t *testing.T) {
		_, diagnostics := Parse("large.jsonc", strings.NewReader(strings.Repeat(" ", int(MaxConfigBytes)+1)))
		assertDiagnostic(t, diagnostics, "size", "", false)
	})
}

func TestManagedStandardImageConfig(t *testing.T) {
	validated, diagnostics := ParseBytes("standard.jsonc", []byte(`{
		"schemaVersion": 1,
		"workspace": {"root": "."},
		"image": {"standard": true}
	}`))
	if len(diagnostics) != 0 {
		t.Fatalf("ParseBytes() diagnostics = %#v", diagnostics)
	}
	if !validated.Document.Image.Standard || validated.SourceLocations["/image/standard"].Line == 0 {
		t.Fatalf("managed standard image = %#v", validated)
	}
}

func TestSchemaUnknownField(t *testing.T) {
	t.Parallel()
	_, diagnostics := ParseBytes("invalid-unknown.jsonc", fixture(t, "invalid-unknown.jsonc"))
	assertDiagnostic(t, diagnostics, "schema", "/workspace/initializeCommand", true)
}

func TestDuplicateLocations(t *testing.T) {
	t.Parallel()
	_, diagnostics := ParseBytes("invalid-duplicate.jsonc", fixture(t, "invalid-duplicate.jsonc"))
	assertDiagnostic(t, diagnostics, "duplicate", "/workspace/root", true)
	if len(diagnostics[0].Related) != 2 {
		t.Fatalf("duplicate related locations = %#v, want first and repeated declarations", diagnostics[0].Related)
	}
	first, repeated := diagnostics[0].Related[0], diagnostics[0].Related[1]
	if first.Line != 4 || repeated.Line != 5 || first.Column == 0 || repeated.Column == 0 {
		t.Fatalf("duplicate locations = %#v, want lines 4 and 5", diagnostics[0].Related)
	}
	if !strings.Contains(diagnostics[0].Message, "line 4") || !strings.Contains(diagnostics[0].Message, "line 5") {
		t.Fatalf("duplicate message does not report both locations: %q", diagnostics[0].Message)
	}
}

func TestSemanticValidation(t *testing.T) {
	t.Parallel()
	_, diagnostics := ParseBytes("invalid-semantic.jsonc", fixture(t, "invalid-semantic.jsonc"))
	assertDiagnostic(t, diagnostics, "semantic", "", true)
	assertHasPath(t, diagnostics, "/image/ref")
	assertHasPath(t, diagnostics, "/processes/web/dependsOn/0")
	assertHasPath(t, diagnostics, "/workspace/members/1/path")
}

func TestPortBindExplicitNonLoopbackGrant(t *testing.T) {
	t.Parallel()
	document := `{
  "schemaVersion": 1,
  "workspace": {"root":"."},
  "image": {"ref":"ghcr.io/example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "ports": [{"name":"web","guest":3000,"host":"dynamic","bind":"0.0.0.0","protocol":"tcp"}]
}`
	validated, diagnostics := ParseBytes("nonloopback.jsonc", []byte(document))
	if len(diagnostics) != 0 {
		t.Fatalf("explicit non-loopback bind diagnostics = %#v", diagnostics)
	}
	if len(validated.Document.Ports) != 1 || validated.Document.Ports[0].Bind != "0.0.0.0" {
		t.Fatalf("non-loopback bind = %#v", validated.Document.Ports)
	}

	invalid := strings.Replace(document, "0.0.0.0", "224.0.0.1", 1)
	_, diagnostics = ParseBytes("multicast.jsonc", []byte(invalid))
	assertDiagnostic(t, diagnostics, "semantic", "/ports/0/bind", true)
}

func TestSemanticMountDenylist(t *testing.T) {
	t.Parallel()
	paths := []string{
		"/Users/alice",
		"/var/run/docker.sock",
		"/private/tmp/ssh-foo/agent.sock",
		"/private/tmp/gpg-agent.sock",
		"/Library/Keychains",
		"/var/db/tailscale",
		"/private",
		"/private/var",
		"/var",
		"/run",
		"/tmp",
		"/usr/local/libexec",
	}
	for _, source := range paths {
		source := source
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			document := fmt.Sprintf(`{
  "schemaVersion": 1,
  "workspace": {"root":"."},
  "image": {"ref":"ghcr.io/example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "mounts": [{"source":{"type":"host","path":%q},"target":"/reviewed","readOnly":true}]
}`, source)
			_, diagnostics := ParseBytes("deny.jsonc", []byte(document))
			assertDiagnostic(t, diagnostics, "semantic", "/mounts/0/source/path", true)
		})
	}
}

func TestSemanticReservedGuestHelperTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		member string
		path   string
	}{
		{"mount child", `"mounts":[{"source":{"type":"workspace","path":"."},"target":"/usr/local/libexec/dsx/dsx-guest"}]`, "/mounts/0/target"},
		{"mount ancestor", `"mounts":[{"source":{"type":"workspace","path":"."},"target":"/usr/local/libexec"}]`, "/mounts/0/target"},
		{"volume exact", `"volumes":{"tools":{"target":"/usr/local/libexec/dsx","scope":"sandbox"}}`, "/volumes/tools/target"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := `{"schemaVersion":1,"workspace":{"root":"."},"image":{"ref":"ghcr.io/example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},` + test.member + `}`
			_, diagnostics := ParseBytes("reserved.jsonc", []byte(document))
			assertDiagnostic(t, diagnostics, "semantic", test.path, true)
		})
	}
}

func TestHostMountValidationHelperBoundaries(t *testing.T) {
	if err := ValidateHostMountPath("/opt/project/data"); err != nil {
		t.Fatalf("safe canonical path rejected: %v", err)
	}
	for _, source := range []string{"relative", "/opt/project/../secret", "/Users/alice", "/opt/.ssh/config", "/private", "/private/var/run", "/private/tmp", "/var", "/run", "/tmp"} {
		if err := ValidateHostMountPath(source); err == nil {
			t.Errorf("ValidateHostMountPath(%q) accepted unsafe path", source)
		}
	}
}

func TestJSONCSmokeNoSideEffects(t *testing.T) {
	directory := t.TempDir()
	before, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}

	realistic := []byte(`{
  "$schema": "https://dsx.dev/schema/config-v1.json",
  "schemaVersion": 1,
  "workspace": {"root":".","members":[{"name":"api","path":"services/api"}]},
  "image": {"ref":"ghcr.io/example/dev@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
  "setup": [{"argv":["pnpm","install","--frozen-lockfile"],"cwd":"/workspace"}],
  "processes": {
    "db": {"argv":["postgres"]},
    "web": {"argv":["pnpm","dev"],"dependsOn":["db"],"health":{"http":{"url":"http://127.0.0.1:3000/health"},"interval":"1s","timeout":"2s"}}
  },
  "volumes": {"node_modules":{"target":"/workspace/node_modules","scope":"sandbox"}},
  "mounts": [],
  "agents": {"default":"codex","allowed":["omp","codex","claude","opencode"]},
  "authProfiles": {"codex-main":{"harness":"codex","persistence":"global"}},
  "ports": [{"name":"web","guest":3000,"host":"dynamic","bind":"127.0.0.1","protocol":"tcp"}],
  "browser": {"enabled":false},
  "aws": {"mode":"none"},
  "network": {"internet":true,"hostGrants":[]},
  "resources": {"cpus":4,"memory":"8GiB","maxConcurrentClones":2},
}`)
	if _, diagnostics := ParseBytes("memory.jsonc", realistic); len(diagnostics) != 0 {
		t.Fatalf("realistic config rejected: %#v", diagnostics)
	}

	duplicate := strings.Replace(string(realistic), `"schemaVersion": 1,`, `"schemaVersion": 1, "schemaVersion": 1,`, 1)
	if _, diagnostics := ParseBytes("memory.jsonc", []byte(duplicate)); len(diagnostics) != 1 || diagnostics[0].Code != "duplicate" {
		t.Fatalf("duplicate alteration diagnostics = %#v", diagnostics)
	}

	homeMount := strings.Replace(string(realistic), `"mounts": [],`, `"mounts": [{"source":{"type":"host","path":"/Users/alice"},"target":"/host-home","readOnly":true}],`, 1)
	if _, diagnostics := ParseBytes("memory.jsonc", []byte(homeMount)); len(diagnostics) == 0 || diagnostics[0].Code != "semantic" {
		t.Fatalf("host-home alteration diagnostics = %#v", diagnostics)
	}

	after, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("parser created resources under %s: before=%v after=%v", directory, before, after)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "config", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertDiagnostic(t *testing.T, diagnostics []Diagnostic, code, diagnosticPath string, requireLocation bool) {
	t.Helper()
	if len(diagnostics) == 0 {
		t.Fatalf("diagnostics are empty, want code %q", code)
	}
	found := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Code != code || (diagnosticPath != "" && diagnostic.Path != diagnosticPath) {
			continue
		}
		found = true
		if requireLocation && (diagnostic.Line == 0 || diagnostic.Column == 0) {
			t.Fatalf("diagnostic has no source location: %#v", diagnostic)
		}
	}
	if !found {
		t.Fatalf("diagnostics = %#v, want code %q path %q", diagnostics, code, diagnosticPath)
	}
}

func assertHasPath(t *testing.T, diagnostics []Diagnostic, diagnosticPath string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Path == diagnosticPath {
			return
		}
	}
	t.Fatalf("diagnostics = %#v, want path %q", diagnostics, diagnosticPath)
}
