package schema

import _ "embed"

// DSXConfigV1 is the offline Draft 2020-12 schema for project configuration.
//
//go:embed dsx-config-v1.schema.json
var DSXConfigV1 []byte
