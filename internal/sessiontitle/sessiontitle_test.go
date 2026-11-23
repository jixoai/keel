package sessiontitle

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestDerive pins the derivation rule at the package that owns it: user text
// only, whitespace collapsed, cut to Limit runes with an ellipsis, empty for no
// user message.
func TestDerive(t *testing.T) {
	long := strings.Repeat("字", Limit+40)
	exact := strings.Repeat("a", Limit)
	cases := []struct {
		name string
		text string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", " \n\t\r\n ", ""},
		{"plain", "hello world", "hello world"},
		{"trimmed", "   hello  ", "hello"},
		{"newlines collapse", "line one\nline two\n\nline three", "line one line two line three"},
		{"runs collapse", "spaced\t\tout   text", "spaced out text"},
		{"exact limit is untouched", exact, exact},
		{"one over the limit is cut", exact + "b", strings.Repeat("a", Limit-1) + "…"},
		{"long text is cut", long, string([]rune(long)[:Limit-1]) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Derive(tc.text)
			if got != tc.want {
				t.Fatalf("Derive(%q) = %q, want %q", tc.text, got, tc.want)
			}
			if runes := []rune(got); len(runes) > Limit {
				t.Fatalf("title %q is %d runes, over the limit %d", got, len(runes), Limit)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("title %q is not valid UTF-8", got)
			}
		})
	}
	if runes := []rune(Derive(long)); len(runes) != Limit {
		t.Fatalf("cut title is %d runes, want exactly %d", len(runes), Limit)
	}
}

// TestDeriveIsStable states the update rule: a title is a pure function of the
// first message, so the same transcript derives the same title every time, and
// a later message is never an input.
func TestDeriveIsStable(t *testing.T) {
	first := "  部署新的退款分析任务  "
	if once, twice := Derive(first), Derive(first); once != twice {
		t.Fatalf("Derive is not deterministic: %q then %q", once, twice)
	}
	// A later message arrives as another event; the derivation has no input for
	// it. The API is single-argument on purpose, so this test documents the
	// boundary rather than simulating a second read.
	if got := Derive(first); got != "部署新的退款分析任务" {
		t.Fatalf("title = %q, want the collapsed first message", got)
	}
}
