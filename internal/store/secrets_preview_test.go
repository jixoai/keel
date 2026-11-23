package store

// The masked credential preview: the exact boundary (>18 runes keeps first6 +
// "…" + last6, anything else is the fixed mask), rune counting (a CJK or emoji
// credential counts code points, not bytes or UTF-16 units), and the guarantee
// that no preview carries the whole value.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSecretPreviewBoundary(t *testing.T) {
	// The boundary is 18: six + ellipsis + six would be the whole value at 19
	// characters (6 + 1 + 6 = 13 visible, but the rule is stated on the rune
	// count), and at 18 the visible parts would be exactly the value. Everything
	// at or below 18 is the fixed mask.
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", value: "", want: MaskedSecretPreview},
		{name: "single char", value: "a", want: MaskedSecretPreview},
		{name: "short key", value: "sk-live-1", want: MaskedSecretPreview},
		// 17 runes: one below the boundary.
		{name: "17 runes", value: strings.Repeat("a", 17), want: MaskedSecretPreview},
		// 18 runes: exactly the boundary, still fully masked.
		{name: "18 runes", value: strings.Repeat("b", 18), want: MaskedSecretPreview},
		// 19 runes: the first length that previews.
		{
			name:  "19 runes",
			value: "abcdefghijklmnopqrs",
			want:  "abcdef…nopqrs",
		},
		{
			name:  "typical api key",
			value: "sk-live-0123456789-abcdefghijklmnop",
			want:  "sk-liv…klmnop",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SecretPreview([]byte(test.value))
			if got != test.want {
				t.Fatalf("SecretPreview(%d runes) = %q, want %q", utf8.RuneCountInString(test.value), got, test.want)
			}
			if strings.Contains(got, test.value) && len(test.value) > 0 {
				t.Fatalf("the preview contains the whole value: %q", got)
			}
		})
	}
}

func TestSecretPreviewCountsRunes(t *testing.T) {
	// 18 CJK runes are 54 bytes; a byte-counted boundary would preview them.
	eighteen := strings.Repeat("密", 18)
	if got := SecretPreview([]byte(eighteen)); got != MaskedSecretPreview {
		t.Fatalf("18 CJK runes = %q, want the fixed mask (the rule counts runes)", got)
	}
	// 19 CJK runes preview first six and last six code points.
	nineteen := strings.Repeat("密", 19)
	want := strings.Repeat("密", 6) + "…" + strings.Repeat("密", 6)
	if got := SecretPreview([]byte(nineteen)); got != want {
		t.Fatalf("19 CJK runes = %q, want %q", got, want)
	}
	// An emoji is one rune (four UTF-8 bytes): 19 emoji must preview.
	emoji := strings.Repeat("🔑", 19)
	if got := SecretPreview([]byte(emoji)); got != strings.Repeat("🔑", 6)+"…"+strings.Repeat("🔑", 6) {
		t.Fatalf("19 emoji = %q, want first/last six code points", got)
	}
	// 18 emoji stay masked.
	if got := SecretPreview([]byte(strings.Repeat("🔑", 18))); got != MaskedSecretPreview {
		t.Fatalf("18 emoji = %q, want the fixed mask", got)
	}
	// Mixed scripts inside the visible window are preserved verbatim. The value
	// is 23 runes; a shorter one would be fully masked.
	mixed := "密钥abc123-é中🔑tail-longer"
	if got := SecretPreview([]byte(mixed)); got != "密钥abc1…longer" {
		t.Fatalf("mixed preview = %q", got)
	}
}

func TestSecretPreviewNeverCarriesTheValueWhole(t *testing.T) {
	for _, value := range []string{
		"short", "exactly-eighteen-x", strings.Repeat("x", 18),
		strings.Repeat("x", 19), "sk-live-0123456789-abcdefghijklmnop",
	} {
		preview := SecretPreview([]byte(value))
		if strings.Contains(preview, value) {
			t.Fatalf("preview of %q is the whole value: %q", value, preview)
		}
	}
	// The fixed mask is the same for every short value, so its text cannot
	// reveal which short value was stored.
	if SecretPreview([]byte("a")) != SecretPreview([]byte("another-short-key")) {
		t.Fatal("the fixed mask must not vary by value")
	}
}
