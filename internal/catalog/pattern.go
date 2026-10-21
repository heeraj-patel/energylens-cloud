package catalog

import "strings"

// These rules mirror the KB's reviewed manual pattern scope. Only a stored
// reviewed pattern is interpreted; the requested model is always literal.
// In a pattern '*' is one or more characters and '?' exactly one.
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
		!asciiAlnum(pattern[0]) || !asciiAlnum(pattern[len(pattern)-1]) && pattern[len(pattern)-1] != '*' && pattern[len(pattern)-1] != '?' ||
		!asciiAlnum(model[0]) || !modelEnd(model[len(model)-1]) {
		return false
	}
	wildcards, literals := 0, 0
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '*' || c == '?' {
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
	// Normalize separators as the KB does, keeping each wildcard in place.
	// A wildcard never consumes a separator.
	actual := normalizedModel(model)
	wanted := make([]byte, 0, len(pattern))
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '*' || pattern[i] == '?' {
			wanted = append(wanted, pattern[i])
		} else if asciiAlnum(pattern[i]) {
			c := pattern[i]
			if c >= 'a' && c <= 'z' {
				c -= 'a' - 'A'
			}
			wanted = append(wanted, c)
		}
	}
	return wildcardMatch(string(wanted), actual)
}

// wildcardMatch matches text against a normalized pattern in which '*' takes
// one or more characters and '?' exactly one.
func wildcardMatch(pattern, text string) bool {
	reach := make([]bool, len(text)+1)
	reach[0] = true
	for i := 0; i < len(pattern); i++ {
		next := make([]bool, len(text)+1)
		switch pattern[i] {
		case '?':
			for j := 0; j < len(text); j++ {
				next[j+1] = reach[j]
			}
		case '*':
			seen := false
			for j := 0; j < len(text); j++ {
				seen = seen || reach[j]
				next[j+1] = seen
			}
		default:
			for j := 0; j < len(text); j++ {
				next[j+1] = reach[j] && text[j] == pattern[i]
			}
		}
		reach = next
	}
	return reach[len(text)]
}

// wildcardPrefixMatches reports whether a model matching pattern can start
// with prefix: pattern tokens consume the prefix, and whatever is left of the
// pattern may expand to anything.
func wildcardPrefixMatches(pattern, prefix string) bool {
	reach := make([]bool, len(prefix)+1)
	reach[0] = true
	for i := 0; i < len(pattern); i++ {
		next := make([]bool, len(prefix)+1)
		for j := 0; j <= len(prefix); j++ {
			if !reach[j] {
				continue
			}
			if j == len(prefix) {
				next[j] = true
				continue
			}
			switch pattern[i] {
			case '?':
				next[j+1] = true
			case '*':
				for k := j + 1; k <= len(prefix); k++ {
					next[k] = true
				}
			default:
				if prefix[j] == pattern[i] {
					next[j+1] = true
				}
			}
		}
		reach = next
	}
	return reach[len(prefix)]
}

func asciiAlnum(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

func modelEnd(c byte) bool { return asciiAlnum(c) || c == ')' }
