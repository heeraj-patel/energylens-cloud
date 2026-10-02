package catalog

import "strings"

// These rules mirror the KB's reviewed manual pattern scope. Only a stored
// reviewed pattern is interpreted; the requested model is always literal.
func normalizedModel(value string) string {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			out = append(out, c)
		}
	}
	return string(out)
}

func documentedPatternMatches(pattern, model string) bool {
	if len(pattern) < 5 || len(pattern) > 80 || strings.Contains(pattern, ".*") || len(model) < 1 || len(model) > 100 ||
		!asciiAlnum(pattern[0]) || !asciiAlnum(pattern[len(pattern)-1]) ||
		!asciiAlnum(model[0]) || !modelEnd(model[len(model)-1]) {
		return false
	}
	wildcards, literals := 0, 0
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '*' {
			wildcards++
		} else if asciiAlnum(c) {
			literals++
		} else if c != '.' && c != '_' && c != ' ' && c != '/' && c != '-' {
			return false
		}
	}
	if wildcards < 1 || wildcards > 2 || literals < 5 {
		return false
	}
	for i := 0; i < len(model); i++ {
		c := model[i]
		if !asciiAlnum(c) && c != '.' && c != '_' && c != ' ' && c != '/' && c != '-' && c != '(' && c != ')' {
			return false
		}
	}
	// Normalize separators as the KB does, retaining each wildcard as one
	// position. A wildcard never consumes a separator or multiple characters.
	actual := normalizedModel(model)
	wanted := make([]byte, 0, len(pattern))
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '*' {
			wanted = append(wanted, '*')
		} else if asciiAlnum(pattern[i]) {
			c := pattern[i]
			if c >= 'a' && c <= 'z' {
				c -= 'a' - 'A'
			}
			wanted = append(wanted, c)
		}
	}
	if len(actual) != len(wanted) {
		return false
	}
	for i, c := range wanted {
		if c != '*' && c != actual[i] {
			return false
		}
	}
	return true
}

func asciiAlnum(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

func modelEnd(c byte) bool { return asciiAlnum(c) || c == ')' }
