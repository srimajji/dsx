package inspect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

var dangerousDevContainerFields = map[string]struct{}{
	"appPort":              {},
	"capAdd":               {},
	"containerEnv":         {},
	"containerUser":        {},
	"dockerComposeFile":    {},
	"features":             {},
	"initializeCommand":    {},
	"mounts":               {},
	"onCreateCommand":      {},
	"overrideCommand":      {},
	"postAttachCommand":    {},
	"postCreateCommand":    {},
	"postStartCommand":     {},
	"privileged":           {},
	"remoteUser":           {},
	"remoteEnv":            {},
	"runArgs":              {},
	"runServices":          {},
	"securityOpt":          {},
	"service":              {},
	"shutdownAction":       {},
	"updateContentCommand": {},
	"updateRemoteUserUID":  {},
	"userEnvProbe":         {},
	"waitFor":              {},
	"workspaceMount":       {},
}

func parseDevContainer(path string, source []byte) (DevContainer, []Diagnostic) {
	fact := DevContainer{Path: path}
	normalized, err := normalizeJSONC(source)
	if err != nil {
		return fact, []Diagnostic{malformedDiagnostic(path, err)}
	}
	if err := rejectDuplicateJSONKeys(normalized); err != nil {
		return fact, []Diagnostic{malformedDiagnostic(path, err)}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(normalized, &fields); err != nil {
		return fact, []Diagnostic{malformedDiagnostic(path, err)}
	}
	if fields == nil {
		return fact, []Diagnostic{malformedDiagnostic(path, fmt.Errorf("top-level value must be an object"))}
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var diagnostics []Diagnostic
	for _, key := range keys {
		value := fields[key]
		switch key {
		case "name":
			diagnostics = append(diagnostics, decodeStringField(path, "/name", value, &fact.Name)...)
		case "image":
			diagnostics = append(diagnostics, decodeStringField(path, "/image", value, &fact.Image)...)
		case "workspaceFolder":
			diagnostics = append(diagnostics, decodeStringField(path, "/workspaceFolder", value, &fact.WorkspaceFolder)...)
		case "forwardPorts":
			ports, portDiagnostics := decodeForwardPorts(path, value)
			fact.ForwardPorts = ports
			diagnostics = append(diagnostics, portDiagnostics...)
		case "build":
			build, buildDiagnostics := decodeBuild(path, value)
			fact.Build = build
			diagnostics = append(diagnostics, buildDiagnostics...)
		default:
			diagnostics = append(diagnostics, unsupportedDiagnostic(path, "/"+escapePointer(key), key))
		}
	}
	if fact.Image != "" && (fact.Build.Dockerfile != "" || fact.Build.Context != "") {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: SeverityError,
			Code:     "conflicting_devcontainer_source",
			Path:     path,
			Field:    "/build",
			Message:  "Dev Container image and build cannot both be imported",
		})
	}
	sort.Strings(fact.ForwardPorts)
	fact.ForwardPorts = compactStrings(fact.ForwardPorts)
	return fact, diagnostics
}

func decodeStringField(path, field string, raw json.RawMessage, target *string) []Diagnostic {
	if err := json.Unmarshal(raw, target); err != nil {
		return []Diagnostic{{
			Severity: SeverityError,
			Code:     "malformed_devcontainer_field",
			Path:     path,
			Field:    field,
			Message:  fmt.Sprintf("allowlisted Dev Container field must be a string: %v", err),
		}}
	}
	return nil
}

func decodeBuild(path string, raw json.RawMessage) (DevContainerBuild, []Diagnostic) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		if err == nil {
			err = fmt.Errorf("value must be an object")
		}
		return DevContainerBuild{}, []Diagnostic{{
			Severity: SeverityError,
			Code:     "malformed_devcontainer_field",
			Path:     path,
			Field:    "/build",
			Message:  fmt.Sprintf("allowlisted Dev Container build must be an object: %v", err),
		}}
	}
	var build DevContainerBuild
	var diagnostics []Diagnostic
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch key {
		case "dockerfile":
			diagnostics = append(diagnostics, decodeStringField(path, "/build/dockerfile", fields[key], &build.Dockerfile)...)
		case "context":
			diagnostics = append(diagnostics, decodeStringField(path, "/build/context", fields[key], &build.Context)...)
		default:
			severity := SeverityWarning
			if key == "args" || key == "options" || key == "target" {
				severity = SeverityError
			}
			diagnostics = append(diagnostics, Diagnostic{
				Severity: severity,
				Code:     "unsupported_devcontainer_field",
				Path:     path,
				Field:    "/build/" + escapePointer(key),
				Message:  fmt.Sprintf("Dev Container build field %q is not imported", key),
			})
		}
	}
	return build, diagnostics
}

func decodeForwardPorts(path string, raw json.RawMessage) ([]string, []Diagnostic) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, []Diagnostic{{
			Severity: SeverityError,
			Code:     "malformed_devcontainer_field",
			Path:     path,
			Field:    "/forwardPorts",
			Message:  fmt.Sprintf("allowlisted Dev Container forwardPorts must be an array: %v", err),
		}}
	}
	ports := make([]string, 0, len(values))
	var diagnostics []Diagnostic
	for index, value := range values {
		var text string
		if err := json.Unmarshal(value, &text); err == nil {
			ports = append(ports, text)
			continue
		}
		var number json.Number
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		if err := decoder.Decode(&number); err == nil {
			parsed, parseErr := strconv.ParseUint(number.String(), 10, 16)
			if parseErr == nil && parsed > 0 {
				ports = append(ports, strconv.FormatUint(parsed, 10))
				continue
			}
		}
		diagnostics = append(diagnostics, Diagnostic{
			Severity: SeverityError,
			Code:     "malformed_devcontainer_field",
			Path:     path,
			Field:    fmt.Sprintf("/forwardPorts/%d", index),
			Message:  "forwarded port must be a nonzero integer up to 65535 or a string",
		})
	}
	return ports, diagnostics
}

func unsupportedDiagnostic(path, pointer, key string) Diagnostic {
	severity := SeverityWarning
	if _, dangerous := dangerousDevContainerFields[key]; dangerous {
		severity = SeverityError
	}
	return Diagnostic{
		Severity: severity,
		Code:     "unsupported_devcontainer_field",
		Path:     path,
		Field:    pointer,
		Message:  fmt.Sprintf("Dev Container field %q is not imported", key),
	}
}

func malformedDiagnostic(path string, err error) Diagnostic {
	return Diagnostic{
		Severity: SeverityError,
		Code:     "malformed_devcontainer",
		Path:     path,
		Message:  fmt.Sprintf("cannot parse Dev Container declaration: %v", err),
	}
}

func escapePointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	write := 1
	for read := 1; read < len(values); read++ {
		if values[read] == values[write-1] {
			continue
		}
		values[write] = values[read]
		write++
	}
	return values[:write]
}

func normalizeJSONC(source []byte) ([]byte, error) {
	result := append([]byte(nil), source...)
	inString := false
	escaped := false
	for index := 0; index < len(result); index++ {
		character := result[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		if character == '"' {
			inString = true
			continue
		}
		if character != '/' || index+1 >= len(result) {
			continue
		}
		switch result[index+1] {
		case '/':
			result[index], result[index+1] = ' ', ' '
			index += 2
			for index < len(result) && result[index] != '\n' && result[index] != '\r' {
				result[index] = ' '
				index++
			}
			index--
		case '*':
			result[index], result[index+1] = ' ', ' '
			index += 2
			closed := false
			for index < len(result) {
				if index+1 < len(result) && result[index] == '*' && result[index+1] == '/' {
					result[index], result[index+1] = ' ', ' '
					index++
					closed = true
					break
				}
				if result[index] != '\n' && result[index] != '\r' {
					result[index] = ' '
				}
				index++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated block comment")
			}
		}
	}
	if inString {
		return nil, fmt.Errorf("unterminated string")
	}
	inString = false
	escaped = false
	for index := 0; index < len(result); index++ {
		switch result[index] {
		case '\\':
			if inString {
				escaped = !escaped
			}
		case '"':
			if !escaped {
				inString = !inString
			}
			escaped = false
		case ',':
			if inString {
				continue
			}
			next := index + 1
			for next < len(result) && (result[next] == ' ' || result[next] == '\t' || result[next] == '\n' || result[next] == '\r') {
				next++
			}
			if next < len(result) && (result[next] == '}' || result[next] == ']') {
				result[index] = ' '
			}
		default:
			if inString {
				escaped = false
			}
		}
	}
	return result, nil
}

func rejectDuplicateJSONKeys(source []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder, ""); err != nil {
		return err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON token %v", token)
		}
		return err
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, pointer string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			field := pointer + "/" + escapePointer(key)
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate key at %s", field)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder, field); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := consumeJSONValue(decoder, pointer+"/"+strconv.Itoa(index)); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}
