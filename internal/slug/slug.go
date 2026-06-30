// Package slug derives URL-safe slugs from human text.
package slug

import (
	"strings"
	"unicode"
)

// Make converts s into a lowercase, hyphen-separated slug containing only
// ASCII letters, digits, and hyphens. It returns "" when s has no usable
// characters; callers should fall back to a generated id in that case.
func Make(s string) string {
	var b strings.Builder
	lastHyphen := true // suppress a leading hyphen
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case unicode.IsSpace(r), r == '-', r == '_', r == '.', r == '/':
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
		// All other runes (punctuation, non-ASCII) are dropped.
	}
	return strings.Trim(b.String(), "-")
}
