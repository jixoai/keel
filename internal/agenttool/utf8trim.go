// Copied from github.com/smallnest/pigo@891d1f37 (MIT License).
// Upstream: internal/agenttool/bash_tool.go
// Modified for fde2e-keel: extracted the two rune-boundary helpers
// without pulling the bash tool into the foundation.

package agenttool

import "unicode/utf8"

// trimUTF8Prefix drops trailing bytes of s that form an incomplete rune, so the
// returned prefix ends on a rune boundary.
func trimUTF8Prefix(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r == utf8.RuneError && size <= 1 {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// trimUTF8Suffix drops leading bytes of s that form an incomplete rune, so the
// returned suffix starts on a rune boundary.
func trimUTF8Suffix(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeRuneInString(s); r == utf8.RuneError && size <= 1 {
			s = s[1:]
			continue
		}
		break
	}
	return s
}
