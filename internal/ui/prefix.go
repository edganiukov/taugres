package ui

import "strings"

// PrefixLines prefixes every line of s (used for multi-line error messages).
// It writes in a single pass into a pre-sized buffer — no intermediate slice
// and no second walk to join.
func PrefixLines(s, prefix string) string {
	var b strings.Builder
	// One prefix per line: (number of newlines + 1).
	b.Grow(len(s) + (strings.Count(s, "\n")+1)*len(prefix))
	for {
		b.WriteString(prefix)
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+1]) // keep the newline with its line
		s = s[i+1:]
	}
}
