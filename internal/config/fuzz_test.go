package config

import (
	"bytes"
	"strings"
	"testing"
)

func FuzzConfigJSONC(f *testing.F) {
	valid := `{
  // comments must retain source offsets
  "schemaVersion": 1,
  "workspace": {"root":"."},
  "image": {"ref":"ghcr.io/example/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
}`
	for _, seed := range [][]byte{
		[]byte(valid),
		[]byte(`{"schemaVersion":1`),
		[]byte(`{"schemaVersion":1,"schemaVersion":1}`),
		[]byte(`{"schemaVersion":"one","workspace":{"root":"."},"image":{"ref":"invalid"}}`),
		[]byte(`{"schemaVersion":1,"workspace":{"root":"."},"image":{"ref":"invalid"},"unexpected":true}`),
		[]byte("{\"schemaVersion\":1,\"workspace\":{\"root\":\"\x1b[2J\"}}"),
		{0xff, 0xfe, '{', '}'},
		[]byte(strings.Repeat(" ", int(MaxConfigBytes)+1)),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > int(MaxConfigBytes)+1 {
			t.Skip()
		}
		before := bytes.Clone(data)
		validated, diagnostics := ParseBytes("fuzz.jsonc", data)
		if !bytes.Equal(data, before) {
			t.Fatal("ParseBytes mutated its input")
		}
		if len(diagnostics) == 0 {
			if validated.Document.SchemaVersion != 1 || validated.ContentDigest == "" {
				t.Fatalf("successful parse returned an incomplete validated config: %#v", validated)
			}
			return
		}
		if validated.SourcePath != "fuzz.jsonc" || validated.ContentDigest == "" {
			t.Fatalf("rejected config lost immutable source metadata: %#v", validated)
		}
	})
}
