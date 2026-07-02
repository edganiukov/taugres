package ui

import "strings"

// PrefixLines prefixes every line of s (used for multi-line error messages).
func PrefixLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
