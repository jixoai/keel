package fileplugin

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// maxReplacements bounds a replaceAll edit's occurrence list. A file with more
// matches is refused instead of growing an unbounded offset slice.
const maxReplacements = 100_000

// findOccurrences streams r and returns the byte offsets of non-overlapping
// needle matches. max > 0 stops after max matches (the caller only needs to
// know "more than one" for the uniqueness rule).
func findOccurrences(r io.Reader, needle []byte, max int) ([]int64, error) {
	if len(needle) == 0 {
		return nil, Errorf(CodeInvalid, "oldText must not be empty")
	}
	reader := bufio.NewReaderSize(r, 64<<10)
	keep := len(needle) - 1
	var carry []byte
	var base int64
	offsets := make([]int64, 0)
	for {
		chunk := make([]byte, 64<<10)
		n, err := reader.Read(chunk)
		if n > 0 {
			data := make([]byte, 0, len(carry)+n)
			data = append(data, carry...)
			data = append(data, chunk[:n]...)
			start := 0
			for {
				index := bytes.Index(data[start:], needle)
				if index < 0 {
					break
				}
				offsets = append(offsets, base+int64(start+index))
				if max > 0 && len(offsets) >= max {
					return offsets, nil
				}
				start += index + len(needle)
			}
			if keep > 0 {
				keepLen := keep
				if keepLen > len(data) {
					keepLen = len(data)
				}
				carry = append(carry[:0], data[len(data)-keepLen:]...)
				base += int64(len(data) - keepLen)
			} else {
				carry = carry[:0]
				base += int64(len(data))
			}
		}
		if err == io.EOF {
			return offsets, nil
		}
		if err != nil {
			return nil, fmt.Errorf("fileplugin: scan edit source: %w", err)
		}
	}
}

// replaceStream copies r to w, replacing each recorded non-overlapping match.
// It streams: one 32KiB copy buffer plus the replacement text.
func replaceStream(r io.Reader, w io.Writer, needle, replacement []byte, offsets []int64) error {
	reader := bufio.NewReaderSize(r, 64<<10)
	buffer := make([]byte, 64<<10)
	var position int64
	for _, offset := range offsets {
		if offset < position {
			continue
		}
		if _, err := io.CopyBuffer(w, io.LimitReader(reader, offset-position), buffer); err != nil {
			return fmt.Errorf("fileplugin: copy edit prefix: %w", err)
		}
		if _, err := reader.Discard(len(needle)); err != nil {
			return fmt.Errorf("fileplugin: skip edit match: %w", err)
		}
		if _, err := w.Write(replacement); err != nil {
			return fmt.Errorf("fileplugin: write replacement: %w", err)
		}
		position = offset + int64(len(needle))
	}
	if _, err := io.Copy(w, reader); err != nil {
		return fmt.Errorf("fileplugin: copy edit suffix: %w", err)
	}
	return nil
}

// errNoMatch and errNotUnique are internal classifications; the service maps
// them onto typed conflict errors with a readable message.
var (
	errNoMatch   = errors.New("oldText does not occur")
	errNotUnique = errors.New("oldText is not unique")
)
