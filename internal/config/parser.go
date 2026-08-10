package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/srimajji/dsx/schema"
	"github.com/tidwall/jsonc"
)

const MaxConfigBytes int64 = 1 << 20

const configSchemaURL = "https://dsx.dev/schema/config-v1.json"

var (
	compiledSchema     *jsonschema.Schema
	compiledSchemaErr  error
	compiledSchemaOnce sync.Once
)

// Parse reads and validates one JSONC configuration document. It never writes
// configuration or invokes project/runtime code. All expected input failures
// are returned as diagnostics.
func Parse(sourcePath string, r io.Reader) (ValidatedConfig, []Diagnostic) {
	limited := io.LimitReader(r, MaxConfigBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return ValidatedConfig{SourcePath: sourcePath}, []Diagnostic{diagnostic("read", "cannot read configuration: "+err.Error(), sourcePath, SourceLocation{})}
	}
	if int64(len(data)) > MaxConfigBytes {
		return ValidatedConfig{SourcePath: sourcePath}, []Diagnostic{diagnostic("size", fmt.Sprintf("configuration exceeds the %d byte limit", MaxConfigBytes), sourcePath, SourceLocation{})}
	}
	return ParseBytes(sourcePath, data)
}

// ParseFile opens a configuration read-only and applies the bounded parser.
func ParseFile(path string) (ValidatedConfig, []Diagnostic) {
	file, err := os.Open(path)
	if err != nil {
		return ValidatedConfig{SourcePath: path}, []Diagnostic{diagnostic("read", "cannot open configuration: "+err.Error(), path, SourceLocation{})}
	}
	defer file.Close()
	return Parse(path, file)
}

// ParseBytes validates an already bounded in-memory document.
func ParseBytes(sourcePath string, data []byte) (ValidatedConfig, []Diagnostic) {
	sum := sha256.Sum256(data)
	result := ValidatedConfig{
		ContentDigest:   hex.EncodeToString(sum[:]),
		SourcePath:      sourcePath,
		SourceLocations: make(map[string]SourceLocation),
	}
	if int64(len(data)) > MaxConfigBytes {
		return result, []Diagnostic{diagnostic("size", fmt.Sprintf("configuration exceeds the %d byte limit", MaxConfigBytes), sourcePath, SourceLocation{})}
	}
	if !utf8.Valid(data) {
		return result, []Diagnostic{diagnostic("syntax", "configuration is not valid UTF-8", sourcePath, SourceLocation{})}
	}

	normalized := jsonc.ToJSON(data)
	if len(normalized) != len(data) || !sameNewlines(data, normalized) {
		return result, []Diagnostic{diagnostic("internal", "JSONC normalization did not preserve source offsets", sourcePath, SourceLocation{})}
	}

	var raw any
	if err := json.Unmarshal(normalized, &raw); err != nil {
		loc := jsonErrorLocation(data, err)
		return result, []Diagnostic{diagnostic("syntax", err.Error(), sourcePath, loc)}
	}

	scanner := jsonScanner{data: normalized, source: data, path: sourcePath, locations: result.SourceLocations}
	if duplicate := scanner.scan(); duplicate != nil {
		return result, []Diagnostic{*duplicate}
	}

	sch, err := configSchema()
	if err != nil {
		return result, []Diagnostic{diagnostic("internal", "cannot compile embedded configuration schema: "+err.Error(), sourcePath, SourceLocation{})}
	}
	if err := sch.Validate(raw); err != nil {
		return result, schemaDiagnostics(err, sourcePath, result.SourceLocations)
	}

	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result.Document); err != nil {
		loc := jsonErrorLocation(data, err)
		return result, []Diagnostic{diagnostic("decode", "cannot decode configuration: "+err.Error(), sourcePath, loc)}
	}
	if err := ensureDecoderEOF(decoder); err != nil {
		loc := jsonErrorLocation(data, err)
		return result, []Diagnostic{diagnostic("decode", "cannot decode configuration: "+err.Error(), sourcePath, loc)}
	}

	diagnostics := validateSemantics(result.Document, sourcePath, result.SourceLocations)
	if len(diagnostics) != 0 {
		return result, diagnostics
	}
	return result, nil
}

func configSchema() (*jsonschema.Schema, error) {
	compiledSchemaOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		compiler.UseLoader(offlineSchemaLoader{})
		var document any
		if err := json.Unmarshal(schema.DSXConfigV1, &document); err != nil {
			compiledSchemaErr = fmt.Errorf("decode schema: %w", err)
			return
		}
		if err := compiler.AddResource(configSchemaURL, document); err != nil {
			compiledSchemaErr = fmt.Errorf("register schema: %w", err)
			return
		}
		compiledSchema, compiledSchemaErr = compiler.Compile(configSchemaURL)
	})
	return compiledSchema, compiledSchemaErr
}

type offlineSchemaLoader struct{}

func (offlineSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("remote schema resolution is disabled: %s", url)
}

func ensureDecoderEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func sameNewlines(a, b []byte) bool {
	for i := range a {
		if (a[i] == '\n') != (b[i] == '\n') || (a[i] == '\r') != (b[i] == '\r') {
			return false
		}
	}
	return true
}

func jsonErrorLocation(source []byte, err error) SourceLocation {
	offset := int64(1)
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		offset = syntax.Offset
	}
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) && typeError.Offset > 0 {
		offset = typeError.Offset
	}
	if offset < 1 {
		offset = 1
	}
	return sourceLocation(source, int(offset-1))
}

func sourceLocation(source []byte, offset int) SourceLocation {
	if offset < 0 {
		offset = 0
	}
	if offset > len(source) {
		offset = len(source)
	}
	line, column := 1, 1
	for index := 0; index < offset; {
		if source[index] == '\n' {
			line++
			column = 1
			index++
			continue
		}
		_, width := utf8.DecodeRune(source[index:offset])
		column++
		index += width
	}
	return SourceLocation{Offset: offset, Line: line, Column: column}
}

func diagnostic(code, message, path string, loc SourceLocation) Diagnostic {
	return Diagnostic{Severity: "error", Code: code, Message: message, Path: path, Line: loc.Line, Column: loc.Column}
}

func schemaDiagnostics(err error, sourcePath string, locations map[string]SourceLocation) []Diagnostic {
	validationErr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []Diagnostic{diagnostic("schema", err.Error(), sourcePath, SourceLocation{})}
	}
	leaves := make([]*jsonschema.ValidationError, 0, 4)
	collectValidationLeaves(validationErr, &leaves)
	diagnostics := make([]Diagnostic, 0, len(leaves))
	for _, leaf := range leaves {
		pointer := jsonPointer(leaf.InstanceLocation)
		if additional, ok := leaf.ErrorKind.(*kind.AdditionalProperties); ok {
			for _, property := range additional.Properties {
				propertyPointer := pointer + "/" + escapePointer(property)
				loc := nearestLocation(propertyPointer, locations)
				d := diagnostic("schema", leaf.Error(), sourcePath, loc)
				d.Path = propertyPointer
				diagnostics = append(diagnostics, d)
			}
			continue
		}
		loc := nearestLocation(pointer, locations)
		d := diagnostic("schema", leaf.Error(), sourcePath, loc)
		d.Path = pointer
		diagnostics = append(diagnostics, d)
	}
	sort.SliceStable(diagnostics, func(i, j int) bool {
		if diagnostics[i].Path != diagnostics[j].Path {
			return diagnostics[i].Path < diagnostics[j].Path
		}
		return diagnostics[i].Message < diagnostics[j].Message
	})
	return diagnostics
}

func collectValidationLeaves(err *jsonschema.ValidationError, leaves *[]*jsonschema.ValidationError) {
	if len(err.Causes) == 0 {
		*leaves = append(*leaves, err)
		return
	}
	for _, cause := range err.Causes {
		collectValidationLeaves(cause, leaves)
	}
}

func jsonPointer(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		b.WriteByte('/')
		b.WriteString(escapePointer(part))
	}
	return b.String()
}

func escapePointer(part string) string {
	return strings.ReplaceAll(strings.ReplaceAll(part, "~", "~0"), "/", "~1")
}

func nearestLocation(pointer string, locations map[string]SourceLocation) SourceLocation {
	for {
		if loc, ok := locations[pointer]; ok {
			return loc
		}
		index := strings.LastIndexByte(pointer, '/')
		if index < 0 {
			return SourceLocation{}
		}
		pointer = pointer[:index]
	}
}

type jsonScanner struct {
	data      []byte
	source    []byte
	path      string
	i         int
	locations map[string]SourceLocation
}

func (s *jsonScanner) scan() *Diagnostic {
	s.skipSpace()
	return s.scanValue("")
}

func (s *jsonScanner) scanValue(pointer string) *Diagnostic {
	s.skipSpace()
	if _, exists := s.locations[pointer]; !exists {
		s.locations[pointer] = sourceLocation(s.source, s.i)
	}
	if s.i >= len(s.data) {
		return nil
	}
	switch s.data[s.i] {
	case '{':
		return s.scanObject(pointer)
	case '[':
		return s.scanArray(pointer)
	case '"':
		s.scanString()
	default:
		for s.i < len(s.data) {
			switch s.data[s.i] {
			case ' ', '\t', '\r', '\n', ',', ']', '}':
				return nil
			default:
				s.i++
			}
		}
	}
	return nil
}

func (s *jsonScanner) scanObject(pointer string) *Diagnostic {
	s.i++
	s.skipSpace()
	seen := make(map[string]SourceLocation)
	for s.i < len(s.data) && s.data[s.i] != '}' {
		keyOffset := s.i
		keyBytes := s.scanString()
		var key string
		_ = json.Unmarshal(keyBytes, &key)
		keyLoc := sourceLocation(s.source, keyOffset)
		childPointer := pointer + "/" + escapePointer(key)
		if first, exists := seen[key]; exists {
			d := diagnostic("duplicate", fmt.Sprintf("duplicate key %q; first declared at line %d, column %d and repeated at line %d, column %d", key, first.Line, first.Column, keyLoc.Line, keyLoc.Column), s.path, keyLoc)
			d.Path = childPointer
			d.Related = []SourceLocation{first, keyLoc}
			return &d
		}
		seen[key] = keyLoc
		s.locations[childPointer] = keyLoc
		s.skipSpace()
		s.i++ // colon; syntax was validated before this pass
		if duplicate := s.scanValue(childPointer); duplicate != nil {
			return duplicate
		}
		s.skipSpace()
		if s.i < len(s.data) && s.data[s.i] == ',' {
			s.i++
			s.skipSpace()
		}
	}
	if s.i < len(s.data) {
		s.i++
	}
	return nil
}

func (s *jsonScanner) scanArray(pointer string) *Diagnostic {
	s.i++
	s.skipSpace()
	for index := 0; s.i < len(s.data) && s.data[s.i] != ']'; index++ {
		childPointer := fmt.Sprintf("%s/%d", pointer, index)
		if duplicate := s.scanValue(childPointer); duplicate != nil {
			return duplicate
		}
		s.skipSpace()
		if s.i < len(s.data) && s.data[s.i] == ',' {
			s.i++
			s.skipSpace()
		}
	}
	if s.i < len(s.data) {
		s.i++
	}
	return nil
}

func (s *jsonScanner) scanString() []byte {
	start := s.i
	s.i++
	for s.i < len(s.data) {
		if s.data[s.i] == '\\' {
			s.i += 2
			continue
		}
		if s.data[s.i] == '"' {
			s.i++
			break
		}
		s.i++
	}
	return s.data[start:s.i]
}

func (s *jsonScanner) skipSpace() {
	for s.i < len(s.data) {
		switch s.data[s.i] {
		case ' ', '\t', '\r', '\n':
			s.i++
		default:
			return
		}
	}
}
