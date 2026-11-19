package fileplugin

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// Read defaults. A read is always bounded: at most 200 lines and 256KiB by
// default, at most 2000 lines when the caller asks for more.
const (
	DefaultReadLines = 200
	MaxReadLines     = 2000
	DefaultReadBytes = 256 << 10
	maxLineBytes     = 1 << 20
)

type linePage struct {
	Content       string
	TotalLines    int
	ReturnedLines int
	NextOffset    int
	Truncated     bool
}

// readLinePage streams r and returns the requested line window plus the total
// line count. Only the returned window is retained (bounded by maxBytes); the
// scan itself uses a fixed 64KiB buffer.
func readLinePage(r io.Reader, offset, limit, maxBytes int) (linePage, error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = DefaultReadLines
	}
	if limit > MaxReadLines {
		limit = MaxReadLines
	}
	if maxBytes <= 0 {
		maxBytes = DefaultReadBytes
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	var content bytes.Buffer
	page := linePage{}
	lineNo := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if lineNo >= offset && page.ReturnedLines < limit {
			remaining := maxBytes - content.Len()
			switch {
			case len(line)+1 <= remaining:
				content.Write(line)
				content.WriteByte('\n')
				page.ReturnedLines++
			case remaining > 1:
				// A single line larger than the page budget still returns a
				// bounded prefix instead of an empty result.
				content.Write(line[:remaining-1])
				content.WriteByte('\n')
				page.ReturnedLines++
				page.Truncated = true
			default:
				page.Truncated = true
			}
		}
		lineNo++
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			page.Truncated = true
		} else {
			return linePage{}, err
		}
	}
	page.TotalLines = lineNo
	page.NextOffset = offset + page.ReturnedLines
	if page.NextOffset < page.TotalLines {
		page.Truncated = true
	}
	page.Content = content.String()
	return page, nil
}
