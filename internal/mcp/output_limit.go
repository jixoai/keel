// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/output-limit.ts (MIT License); implemented independently in Go.
// Contract kept identical; see docs/UPSTREAM.md.

package mcp

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jixoai/keel/internal/agentcore"
)

// MCPOutputMaxBytes is the size limit of the model-facing text of an MCP
// result. Longer text keeps its start and end with the middle cut out, like
// Codex does. Mirrors MCP_OUTPUT_MAX_BYTES (output-limit.ts:9).
const MCPOutputMaxBytes = 20 * 1024

// LimitedContent mirrors pi's limitMcpContent result: the model-facing content
// and, when the full text was saved, the path the model can read.
type LimitedContent struct {
	Content        []agentcore.Content
	FullOutputPath string
}

// LimitContent keeps model-facing text within MCPOutputMaxBytes. Longer text
// becomes one text block in Codex's truncation format, followed by the path of
// the file with the full text when saveOutput is given; images follow it.
// Content within the limit is returned as it is (same slice).
//
// saveOutput receives the full untruncated text (text blocks joined with
// newlines) and a ".txt" extension, and returns a path the model can read. A
// nil saveOutput means no file is written. A save error is reported to the
// model in the same text instead of failing.
func LimitContent(content []agentcore.Content, saveOutput func(data, extension string) (string, error)) LimitedContent {
	var texts []string
	for _, block := range content {
		if text, ok := contentText(block); ok {
			texts = append(texts, text)
		}
	}
	combined := strings.Join(texts, "\n")
	truncation, truncated := truncateMiddle(combined, MCPOutputMaxBytes)
	if !truncated {
		return LimitedContent{Content: content}
	}
	where := ""
	fullPath := ""
	if saveOutput != nil {
		path, err := saveOutput(combined, ".txt")
		if err != nil {
			where = "\n\n[Could not save the full output: " + err.Error() + "]"
		} else {
			fullPath = path
			where = "\n\n[Full output: " + path + " (read it with offset/limit)]"
		}
	}
	tokens := (truncation.totalBytes + 3) / 4 // ceil(bytes/4), like pi
	text := fmt.Sprintf(
		"Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n%s%s",
		tokens, truncation.totalLines, truncation.content, where,
	)
	out := make([]agentcore.Content, 0, 1+len(content))
	out = append(out, agentcore.NewTextContent(text))
	for _, block := range content {
		if image, ok := contentImage(block); ok {
			out = append(out, image)
		}
	}
	return LimitedContent{Content: out, FullOutputPath: fullPath}
}

// middleTruncation is text cut in the middle, with the size of the whole text.
type middleTruncation struct {
	// content is the start and the end of the text with a
	// "…N chars truncated…" marker between them.
	content string
	// totalBytes is the byte length of the whole text.
	totalBytes int
	// totalLines is the number of lines in the whole text; a final newline
	// ends the last line instead of starting another.
	totalLines int
}

// truncateMiddle keeps the start and the end of text, half of maxBytes each,
// and replaces the middle with a "…N chars truncated…" marker. It cuts only at
// character boundaries. The second result is false when text fits.
func truncateMiddle(text string, maxBytes int) (middleTruncation, bool) {
	buf := []byte(text)
	if len(buf) <= maxBytes {
		return middleTruncation{}, false
	}
	// Continuation bytes (10xxxxxx) are not character starts.
	isBoundary := func(index int) bool {
		return index >= len(buf) || buf[index]&0xc0 != 0x80
	}
	headEnd := maxBytes / 2
	for headEnd > 0 && !isBoundary(headEnd) {
		headEnd--
	}
	tailStart := len(buf) - (maxBytes - maxBytes/2)
	for tailStart < len(buf) && !isBoundary(tailStart) {
		tailStart++
	}
	head := string(buf[:headEnd])
	tail := string(buf[tailStart:])
	removedChars := utf8.RuneCount(buf[headEnd:tailStart])
	lines := strings.Count(text, "\n") + 1
	if strings.HasSuffix(text, "\n") {
		lines--
	}
	return middleTruncation{
		content:    head + "…" + strconv.Itoa(removedChars) + " chars truncated…" + tail,
		totalBytes: len(buf),
		totalLines: lines,
	}, true
}

// contentText returns the text of a text block. Content from this package and
// from JSON decoding is held by value; pointers are accepted for tolerance.
func contentText(block agentcore.Content) (string, bool) {
	switch value := block.(type) {
	case agentcore.TextContent:
		return value.Text, true
	case *agentcore.TextContent:
		return value.Text, true
	}
	return "", false
}

// contentImage returns an image block. Pointers are accepted like contentText.
func contentImage(block agentcore.Content) (agentcore.ImageContent, bool) {
	switch value := block.(type) {
	case agentcore.ImageContent:
		return value, true
	case *agentcore.ImageContent:
		return *value, true
	}
	return agentcore.ImageContent{}, false
}
