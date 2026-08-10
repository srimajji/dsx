package inspect

import "sort"

type nixToken struct {
	value string
}

// declarations performs lexical discovery only. It recognizes direct dotted
// declarations and the first level of processes/services attribute sets; it
// deliberately does not evaluate, interpolate, or translate Nix.
func declarations(source []byte) ([]string, []string) {
	tokens := tokenizeNix(source)
	processes := make(map[string]struct{})
	services := make(map[string]struct{})
	groups := map[string]map[string]struct{}{
		"processes": processes,
		"services":  services,
	}
	for index := range tokens {
		group := groups[tokens[index].value]
		if group == nil || index+2 >= len(tokens) {
			continue
		}
		if tokens[index+1].value == "." && isNixIdentifier(tokens[index+2].value) {
			group[tokens[index+2].value] = struct{}{}
			continue
		}
		if tokens[index+1].value != "=" || tokens[index+2].value != "{" {
			continue
		}
		depth := 1
		statementStart := true
		for cursor := index + 3; cursor < len(tokens) && depth > 0; cursor++ {
			token := tokens[cursor].value
			switch token {
			case "{":
				depth++
				statementStart = true
			case "}":
				depth--
				statementStart = false
			case ";":
				statementStart = depth == 1
			default:
				if depth == 1 && statementStart && isNixIdentifier(token) && cursor+1 < len(tokens) && (tokens[cursor+1].value == "." || tokens[cursor+1].value == "=") {
					group[token] = struct{}{}
				}
				statementStart = false
			}
		}
	}
	return sortedKeys(processes), sortedKeys(services)
}

func tokenizeNix(source []byte) []nixToken {
	var tokens []nixToken
	for index := 0; index < len(source); {
		character := source[index]
		switch {
		case isSpace(character):
			index++
		case character == '#':
			index++
			for index < len(source) && source[index] != '\n' {
				index++
			}
		case character == '/' && index+1 < len(source) && source[index+1] == '*':
			index += 2
			commentDepth := 1
			for index < len(source) && commentDepth > 0 {
				if index+1 < len(source) && source[index] == '/' && source[index+1] == '*' {
					commentDepth++
					index += 2
				} else if index+1 < len(source) && source[index] == '*' && source[index+1] == '/' {
					commentDepth--
					index += 2
				} else {
					index++
				}
			}
		case character == '"':
			index++
			for index < len(source) {
				if source[index] == '\\' && index+1 < len(source) {
					index += 2
					continue
				}
				if source[index] == '"' {
					index++
					break
				}
				index++
			}
		case character == '\'' && index+1 < len(source) && source[index+1] == '\'':
			index += 2
			for index+1 < len(source) {
				if source[index] == '\'' && source[index+1] == '\'' {
					index += 2
					break
				}
				index++
			}
		case isIdentifierByte(character):
			start := index
			for index < len(source) && isIdentifierByte(source[index]) {
				index++
			}
			tokens = append(tokens, nixToken{value: string(source[start:index])})
		case character == '.' || character == '=' || character == '{' || character == '}' || character == ';':
			tokens = append(tokens, nixToken{value: string(character)})
			index++
		default:
			index++
		}
	}
	return tokens
}

func isSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\n' || character == '\r'
}

func isIdentifierByte(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '\''
}

func isNixIdentifier(value string) bool {
	if value == "" || !isIdentifierByte(value[0]) || value[0] >= '0' && value[0] <= '9' {
		return false
	}
	for index := range len(value) {
		if !isIdentifierByte(value[index]) {
			return false
		}
	}
	return true
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
