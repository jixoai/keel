// Tests ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi), packages/mcp/test/output-limit.test.ts
// (MIT License). Each subtest keeps the title of the pi test it ports.

package mcp

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jixoai/keel/internal/agentcore"
)

func testImage() agentcore.ImageContent {
	return agentcore.NewImageContent("AAAA", "image/png")
}

// firstText returns the text of the first block, failing when it is not text.
func firstText(t *testing.T, content []agentcore.Content) string {
	t.Helper()
	if len(content) == 0 {
		t.Fatal("expected a text block first, got no content")
	}
	text, ok := content[0].(agentcore.TextContent)
	if !ok {
		t.Fatalf("expected a text block first, got %T", content[0])
	}
	return text.Text
}

func TestLimitContent(t *testing.T) {
	t.Run("returns content within the limit as it is, without saving", func(t *testing.T) {
		content := []agentcore.Content{
			agentcore.NewTextContent(strings.Repeat("x", MCPOutputMaxBytes)),
			testImage(),
		}
		saves := 0
		limited := LimitContent(content, func(string, string) (string, error) {
			saves++
			return "/tmp/full.txt", nil
		})
		if !reflect.DeepEqual(limited.Content, content) {
			t.Errorf("LimitContent(at limit) = %#v, want %#v", limited, content)
		}
		if &limited.Content[0] != &content[0] {
			t.Error("LimitContent(at limit) returned a copy, want the same slice")
		}
		if saves != 0 {
			t.Errorf("saves = %d, want 0", saves)
		}
	})

	t.Run("cuts the middle of longer text, saves the full text and keeps images", func(t *testing.T) {
		var builder strings.Builder
		for index := 1; index <= 3000; index++ {
			if index > 1 {
				builder.WriteByte('\n')
			}
			fmt.Fprintf(&builder, "line %d", index)
		}
		full := builder.String()
		var saved []string
		var savedExt []string
		split := strings.Index(full[100:], "\n") + 100
		image := testImage()
		limited := LimitContent(
			[]agentcore.Content{
				agentcore.NewTextContent(full[:split]),
				image,
				agentcore.NewTextContent(full[split+1:]),
			},
			func(data, extension string) (string, error) {
				saved = append(saved, data)
				savedExt = append(savedExt, extension)
				return "/tmp/full.txt", nil
			},
		)
		// Text blocks are joined with newlines before the limit applies.
		// (pi also receives a ".txt" extension; this port fixes it at ".txt".)
		if !reflect.DeepEqual(saved, []string{full}) {
			t.Errorf("saveOutput received %d calls with %q, want one call with the full text", len(saved), saved)
		}
		if !reflect.DeepEqual(savedExt, []string{".txt"}) {
			t.Errorf("saveOutput extensions = %v, want [.txt]", savedExt)
		}
		if limited.FullOutputPath != "/tmp/full.txt" {
			t.Errorf("FullOutputPath = %q, want /tmp/full.txt", limited.FullOutputPath)
		}
		if len(limited.Content) != 2 {
			t.Fatalf("len(LimitContent(...)) = %d, want 2", len(limited.Content))
		}
		if !reflect.DeepEqual(limited.Content[1], image) {
			t.Errorf("limited.Content[1] = %#v, want the image %#v", limited.Content[1], image)
		}
		text := firstText(t, limited.Content)
		prefix := fmt.Sprintf(
			"Warning: truncated output (original token count: %d)\nTotal output lines: 3000\n\nline 1\nline 2\n",
			(len(full)+3)/4,
		)
		if !strings.HasPrefix(text, prefix) {
			t.Errorf("truncated text starts %q, want prefix %q", text, prefix)
		}
		if !regexp.MustCompile(`…\d+ chars truncated…`).MatchString(text) {
			t.Errorf("truncated text contains no truncation marker: %q", text)
		}
		if suffix := "line 3000\n\n[Full output: /tmp/full.txt (read it with offset/limit)]"; !strings.HasSuffix(text, suffix) {
			t.Errorf("truncated text ends %q, want suffix %q", text, suffix)
		}
		if len(text) >= MCPOutputMaxBytes+1024 {
			t.Errorf("len(truncated text) = %d, want < %d", len(text), MCPOutputMaxBytes+1024)
		}
	})

	t.Run("reports a failed save, and names no file without a saver", func(t *testing.T) {
		content := []agentcore.Content{
			agentcore.NewTextContent(strings.Repeat("y", MCPOutputMaxBytes+1)),
		}
		failed := LimitContent(content, func(string, string) (string, error) {
			return "", errors.New("disk full")
		})
		failedText := firstText(t, failed.Content)
		if suffix := "\n\n[Could not save the full output: disk full]"; !strings.HasSuffix(failedText, suffix) {
			t.Errorf("failed save text ends %q, want suffix %q", failedText, suffix)
		}
		if strings.Contains(failedText, "[Full output:") {
			t.Errorf("failed save text names a file: %q", failedText)
		}
		if failed.FullOutputPath != "" {
			t.Errorf("failed FullOutputPath = %q, want empty", failed.FullOutputPath)
		}

		unsaved := LimitContent(content, nil)
		unsavedText := firstText(t, unsaved.Content)
		if !regexp.MustCompile(`(?s)^Warning: truncated output .*…\d+ chars truncated…y+$`).MatchString(unsavedText) {
			t.Errorf("unsaved text = %q, want the truncation format without a path", unsavedText)
		}
	})

	t.Run("cuts multi-byte text only at character boundaries", func(t *testing.T) {
		// After the one-byte prefix, characters start at odd bytes, and both cuts
		// (byte 10240 and byte 40004 - 10240) are even: each lands inside a
		// two-byte character.
		text := "a" + strings.Repeat("é", 20_000) + "end"
		limited := firstText(t, LimitContent([]agentcore.Content{agentcore.NewTextContent(text)}, nil).Content)
		if strings.ContainsRune(limited, '\uFFFD') {
			t.Errorf("truncated text contains U+FFFD: %q", limited)
		}
		if !strings.HasSuffix(limited, "end") {
			t.Errorf("truncated text ends %q, want it to end with %q", limited, "end")
		}
		marker := regexp.MustCompile(`…(\d+) chars truncated…`).FindStringSubmatch(limited)
		if marker == nil {
			t.Fatalf("expected the truncation marker in %q", limited)
		}
		body := limited[strings.Index(limited, "\n\n")+2:]
		parts := strings.SplitN(body, marker[0], 2)
		if len(parts) != 2 {
			t.Fatalf("truncated text has no tail after the marker: %q", limited)
		}
		head, tail := parts[0], parts[1]
		if len(head) > MCPOutputMaxBytes/2 {
			t.Errorf("len(head) = %d bytes, want <= %d", len(head), MCPOutputMaxBytes/2)
		}
		if len(tail) > MCPOutputMaxBytes/2 {
			t.Errorf("len(tail) = %d bytes, want <= %d", len(tail), MCPOutputMaxBytes/2)
		}
		removed, err := strconv.Atoi(marker[1])
		if err != nil {
			t.Fatalf("marker count %q is not a number: %v", marker[1], err)
		}
		if got, want := utf8.RuneCountInString(head)+utf8.RuneCountInString(tail)+removed, utf8.RuneCountInString(text); got != want {
			t.Errorf("head+tail+removed runes = %d, want %d", got, want)
		}
	})
}
