package bridge

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
)

// HostDefaultState is the stable, non-secret result of filtering one host AWS
// directory snapshot.
type HostDefaultState string

const (
	HostDefaultAvailable   HostDefaultState = "available"
	HostDefaultUnavailable HostDefaultState = "unavailable"
	HostDefaultUnsupported HostDefaultState = "unsupported"

	maxHostDefaultSourceBytes = MaxHostAWSFileBytes
	maxHostDefaultSections    = 256
	maxHostDefaultKeys        = 4096
	maxHostDefaultNameBytes   = 128
	maxHostDefaultKeyBytes    = 128
	maxHostDefaultLineBytes   = 64 << 10
)

var (
	ErrHostDefaultMalformed     = errors.New("host AWS source syntax is malformed")
	ErrHostDefaultOversized     = errors.New("host AWS source exceeds the size limit")
	ErrHostDefaultLimitExceeded = errors.New("host AWS source exceeds a structural limit")
)

type hostDefaultEntry struct {
	key   string
	value []byte
}

type parsedHostDefault struct {
	present bool
	entries []hostDefaultEntry
}

// FilterHostDefaultSnapshot validates both standard AWS files in their
// entirety, then emits only a complete temporary [default] profile. Named
// sections are deliberately omitted. Errors and states never contain source
// bytes.
func FilterHostDefaultSnapshot(snapshot HostAWSDirectorySnapshot) (filtered HostAWSDirectorySnapshot, state HostDefaultState, err error) {
	config, err := parseHostDefaultINI(snapshot.Config)
	if err != nil {
		return HostAWSDirectorySnapshot{}, HostDefaultUnavailable, err
	}
	credentials, err := parseHostDefaultINI(snapshot.Credentials)
	if err != nil {
		return HostAWSDirectorySnapshot{}, HostDefaultUnavailable, err
	}
	if !credentials.present {
		return HostAWSDirectorySnapshot{}, HostDefaultUnavailable, nil
	}
	if containsConfigHostDefaultCredential(config.entries) || containsForbiddenHostDefaultKey(config.entries) || containsForbiddenHostDefaultKey(credentials.entries) {
		return HostAWSDirectorySnapshot{}, HostDefaultUnsupported, nil
	}

	var credentialValues [3][]byte
	for _, entry := range credentials.entries {
		switch entry.key {
		case "aws_access_key_id":
			credentialValues[0] = entry.value
		case "aws_secret_access_key":
			credentialValues[1] = entry.value
		case "aws_session_token":
			credentialValues[2] = entry.value
		}
	}
	if len(credentialValues[0]) == 0 || len(credentialValues[1]) == 0 {
		return HostAWSDirectorySnapshot{}, HostDefaultUnavailable, nil
	}
	if len(credentialValues[2]) == 0 {
		return HostAWSDirectorySnapshot{}, HostDefaultUnsupported, nil
	}

	filtered.Config, err = encodeHostDefault(config)
	if err != nil {
		return HostAWSDirectorySnapshot{}, HostDefaultUnavailable, err
	}
	filtered.Credentials, err = encodeHostDefaultCredentials(credentialValues)
	if err != nil {
		return HostAWSDirectorySnapshot{}, HostDefaultUnavailable, err
	}
	return filtered, HostDefaultAvailable, nil
}

func parseHostDefaultINI(source []byte) (parsedHostDefault, error) {
	if len(source) > maxHostDefaultSourceBytes {
		return parsedHostDefault{}, ErrHostDefaultOversized
	}
	if !utf8.Valid(source) || containsInvalidHostDefaultControl(source) {
		return parsedHostDefault{}, ErrHostDefaultMalformed
	}

	var result parsedHostDefault
	sections := make(map[string]struct{})
	keys := make(map[string]struct{})
	section := ""
	sectionIsDefault := false
	sectionCount := 0
	keyCount := 0

	for offset := 0; offset < len(source); {
		lineEnd := bytes.IndexByte(source[offset:], '\n')
		if lineEnd < 0 {
			lineEnd = len(source)
		} else {
			lineEnd += offset
		}
		line := source[offset:lineEnd]
		if len(line) != 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) > maxHostDefaultLineBytes {
			return parsedHostDefault{}, ErrHostDefaultLimitExceeded
		}
		hasLeadingSpace := len(line) != 0 && line[0] == ' '
		line = trimHostDefaultSpace(line)
		if hasLeadingSpace && len(line) != 0 && line[0] != '#' && line[0] != ';' {
			return parsedHostDefault{}, ErrHostDefaultMalformed
		}
		if len(line) != 0 && line[0] != '#' && line[0] != ';' {
			if line[0] == '[' {
				if len(line) < 3 || line[len(line)-1] != ']' {
					return parsedHostDefault{}, ErrHostDefaultMalformed
				}
				name := trimHostDefaultSpace(line[1 : len(line)-1])
				if !validHostDefaultSectionName(name) {
					return parsedHostDefault{}, ErrHostDefaultMalformed
				}
				if len(name) > maxHostDefaultNameBytes {
					return parsedHostDefault{}, ErrHostDefaultLimitExceeded
				}
				sectionCount++
				if sectionCount > maxHostDefaultSections {
					return parsedHostDefault{}, ErrHostDefaultLimitExceeded
				}
				section = string(name)
				if _, duplicate := sections[section]; duplicate {
					return parsedHostDefault{}, ErrHostDefaultMalformed
				}
				sections[section] = struct{}{}
				clear(keys)
				sectionIsDefault = section == "default"
				if sectionIsDefault {
					result.present = true
				}
			} else {
				if section == "" {
					return parsedHostDefault{}, ErrHostDefaultMalformed
				}
				equals := bytes.IndexByte(line, '=')
				if equals <= 0 {
					return parsedHostDefault{}, ErrHostDefaultMalformed
				}
				keyBytes := trimHostDefaultSpace(line[:equals])
				if len(keyBytes) > maxHostDefaultKeyBytes {
					return parsedHostDefault{}, ErrHostDefaultLimitExceeded
				}
				if !validHostDefaultKey(keyBytes) {
					return parsedHostDefault{}, ErrHostDefaultMalformed
				}
				key := strings.ToLower(string(keyBytes))
				if _, duplicate := keys[key]; duplicate {
					return parsedHostDefault{}, ErrHostDefaultMalformed
				}
				keys[key] = struct{}{}
				keyCount++
				if keyCount > maxHostDefaultKeys {
					return parsedHostDefault{}, ErrHostDefaultLimitExceeded
				}
				if sectionIsDefault {
					result.entries = append(result.entries, hostDefaultEntry{key: key, value: trimHostDefaultSpace(line[equals+1:])})
				}
			}
		}
		if lineEnd == len(source) {
			break
		}
		offset = lineEnd + 1
	}
	return result, nil
}

func containsInvalidHostDefaultControl(source []byte) bool {
	for index, value := range source {
		switch {
		case value == '\n':
		case value == '\r' && index+1 < len(source) && source[index+1] == '\n':
		case value < 0x20 || value == 0x7f:
			return true
		}
	}
	return false
}

func trimHostDefaultSpace(value []byte) []byte {
	return bytes.Trim(value, " ")
}

func validHostDefaultSectionName(name []byte) bool {
	if len(name) == 0 {
		return false
	}
	for _, value := range name {
		if value < 0x21 || value > 0x7e || value == '[' || value == ']' || value == '=' || value == '#' || value == ';' {
			if value != ' ' {
				return false
			}
		}
	}
	return true
}

func validHostDefaultKey(key []byte) bool {
	if len(key) == 0 {
		return false
	}
	for _, value := range key {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_' || value == '-' || value == '.' {
			continue
		}
		return false
	}
	return true
}

func containsConfigHostDefaultCredential(entries []hostDefaultEntry) bool {
	for _, entry := range entries {
		switch entry.key {
		case "aws_access_key_id", "aws_secret_access_key", "aws_session_token", "aws_security_token":
			return true
		}
	}
	return false
}

func containsForbiddenHostDefaultKey(entries []hostDefaultEntry) bool {
	for _, entry := range entries {
		key := entry.key
		switch key {
		case "credential_process", "credential_source", "source_profile", "web_identity_token_file",
			"role_arn", "external_id", "mfa_serial", "role_session_name", "duration_seconds",
			"sso_start_url", "sso_region", "sso_account_id", "sso_role_name", "sso_session",
			"services", "login_session", "include_profile", "config_file", "credentials_file",
			"shared_credentials_file", "credential_file", "credential_provider":
			return true
		}
		if strings.HasPrefix(key, "credential_") || strings.HasPrefix(key, "sso_") || strings.HasPrefix(key, "role_") || strings.HasPrefix(key, "endpoint_url") || strings.HasSuffix(key, "_arn") || strings.HasSuffix(key, "_file") || strings.HasSuffix(key, "_process") || strings.HasSuffix(key, "_source") || strings.HasSuffix(key, "_profile") {
			return true
		}
	}
	return false
}

func encodeHostDefault(parsed parsedHostDefault) ([]byte, error) {
	if !parsed.present {
		return nil, nil
	}
	slices.SortFunc(parsed.entries, func(left, right hostDefaultEntry) int { return strings.Compare(left.key, right.key) })
	return encodeHostDefaultEntries(parsed.entries)
}

func encodeHostDefaultCredentials(values [3][]byte) ([]byte, error) {
	entries := [...]hostDefaultEntry{
		{key: "aws_access_key_id", value: values[0]},
		{key: "aws_secret_access_key", value: values[1]},
		{key: "aws_session_token", value: values[2]},
	}
	return encodeHostDefaultEntries(entries[:])
}

func encodeHostDefaultEntries(entries []hostDefaultEntry) ([]byte, error) {
	size := len("[default]\n")
	for _, entry := range entries {
		size += len(entry.key) + 1 + len(entry.value) + 1
	}
	if size > maxHostDefaultSourceBytes {
		return nil, ErrHostDefaultLimitExceeded
	}
	encoded := make([]byte, 0, size)
	encoded = append(encoded, "[default]\n"...)
	for _, entry := range entries {
		encoded = append(encoded, entry.key...)
		encoded = append(encoded, '=')
		encoded = append(encoded, entry.value...)
		encoded = append(encoded, '\n')
	}
	return encoded, nil
}
