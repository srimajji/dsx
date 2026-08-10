package plan

import (
	"fmt"
	"strings"

	"github.com/srimajji/dsx/internal/config"
)

const (
	PriorityDefault = 100
	PriorityImport  = 200
	PriorityProject = 300
	PriorityCLI     = 400
)

var standardSource = config.SourceRef{Kind: "default", Path: "standard", Priority: PriorityDefault}
var detectedSource = config.SourceRef{Kind: "detected", Priority: PriorityImport}

func cliSource(flag string) config.SourceRef {
	return config.SourceRef{Kind: "cli", Path: flag, Priority: PriorityCLI}
}

func projectSource(validated config.ValidatedConfig, pointer string) config.SourceRef {
	if source, ok := validated.Provenance[pointer]; ok {
		return source
	}
	location, ok := validated.SourceLocations[pointer]
	if !ok {
		location = nearestSourceLocation(validated.SourceLocations, pointer)
	}
	path := location.Path
	if path == "" {
		path = validated.SourcePath
	}
	return config.SourceRef{
		Kind:     "project",
		Path:     path,
		Line:     location.Line,
		Column:   location.Column,
		Priority: PriorityProject,
	}
}

func nearestSourceLocation(locations map[string]config.SourceLocation, pointer string) config.SourceLocation {
	for {
		if location, ok := locations[pointer]; ok {
			return location
		}
		index := strings.LastIndexByte(pointer, '/')
		if index < 0 {
			return config.SourceLocation{}
		}
		pointer = pointer[:index]
	}
}

func importedSource(source config.SourceRef) config.SourceRef {
	source.Priority = PriorityImport
	return source
}

func putSource(provenance config.Provenance, pointer string, source config.SourceRef) {
	provenance[pointer] = source
}

func escapePointerToken(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}

func parsePointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if pointer[0] != '/' {
		return nil, fmt.Errorf("imported pointer %q is not an RFC 6901 pointer", pointer)
	}
	encoded := strings.Split(pointer[1:], "/")
	parts := make([]string, len(encoded))
	for index, token := range encoded {
		var decoded strings.Builder
		for offset := 0; offset < len(token); offset++ {
			if token[offset] != '~' {
				decoded.WriteByte(token[offset])
				continue
			}
			if offset+1 >= len(token) || (token[offset+1] != '0' && token[offset+1] != '1') {
				return nil, fmt.Errorf("imported pointer %q has an invalid RFC 6901 escape", pointer)
			}
			offset++
			if token[offset] == '0' {
				decoded.WriteByte('~')
			} else {
				decoded.WriteByte('/')
			}
		}
		parts[index] = decoded.String()
	}
	return parts, nil
}
