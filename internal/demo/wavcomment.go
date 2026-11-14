package demo

import (
	"bytes"
	"encoding/binary"
	"strings"
)

// A demo speech clip carries the sentence it "says" in a RIFF LIST/INFO "ICMT"
// chunk, and the demo transcription reads it back.
//
// # Why the audio carries its own text
//
// The microphone is a browser permission, not a capability of the server, so a
// reviewer without a grant cannot exercise the speech path at all. Synthesizing a
// clip and feeding it back is the microphone-free way to prove the whole chain —
// and for that round trip to mean anything, the transcript must come *from the
// audio*, not from a constant that happens to be returned either way. A comment
// chunk is exactly the standard place for that: it is metadata a real encoder
// writes (ICMT is "comments"), it does not alter the PCM, and every WAV reader
// ignores it.
//
// # Scope
//
// This is the demo provider's own wire detail. A vendor TTS would never do it,
// and that is fine: what is being verified is the client's upload → transcribe →
// render path, not a vendor's semantics.

// InfoCommentChunkID is the RIFF sub-chunk id for a comment ("ICMT" in the INFO
// list). Tests assert it so the layout cannot drift.
const infoCommentChunkID = "ICMT"

// WithInfoComment returns wav with a LIST/INFO/ICMT chunk carrying text.
//
// The RIFF size field is recomputed for the new body; the original bytes are
// copied unchanged, so the PCM payload is byte-identical to what it was.
func WithInfoComment(wav []byte, text string) []byte {
	if len(wav) < 12 {
		return wav
	}
	comment := []byte(strings.TrimSpace(text))
	if len(comment) == 0 {
		return wav
	}
	// INFO list: "LIST" <size> "INFO" ("ICMT" <size> <payload padded to even>)
	entry := make([]byte, 0, 8+len(comment)+1)
	entry = append(entry, []byte(infoCommentChunkID)...)
	entry = binary.LittleEndian.AppendUint32(entry, uint32(len(comment)))
	entry = append(entry, comment...)
	if len(comment)%2 == 1 {
		entry = append(entry, 0) // RIFF chunks are word-aligned
	}
	info := make([]byte, 0, 4+len(entry))
	info = append(info, []byte("INFO")...)
	info = append(info, entry...)

	out := make([]byte, 0, len(wav)+8+len(info))
	out = append(out, wav[:len(wav)]...) // header + PCM, verbatim
	list := make([]byte, 0, 8+len(info))
	list = append(list, []byte("LIST")...)
	list = binary.LittleEndian.AppendUint32(list, uint32(len(info)))
	list = append(list, info...)
	out = append(out, list...)
	// RIFF size = everything after the first 8 bytes.
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out
}

// InfoComment returns the text a clip carries in its INFO/ICMT chunk, if any.
func InfoComment(wav []byte) (string, bool) {
	if len(wav) < 12 || !bytes.Equal(wav[:4], []byte("RIFF")) {
		return "", false
	}
	// Walk the top-level chunks after "RIFF"<size>"WAVE".
	offset := 12
	for offset+8 <= len(wav) {
		id := string(wav[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(wav[offset+4 : offset+8]))
		body := offset + 8
		if size < 0 || body+size > len(wav) {
			return "", false
		}
		if id == "LIST" && size >= 4 && string(wav[body:body+4]) == "INFO" {
			inner := wav[body+4 : body+size]
			cursor := 0
			for cursor+8 <= len(inner) {
				sub := string(inner[cursor : cursor+4])
				subSize := int(binary.LittleEndian.Uint32(inner[cursor+4 : cursor+8]))
				start := cursor + 8
				if subSize < 0 || start+subSize > len(inner) {
					break
				}
				if sub == infoCommentChunkID {
					return string(inner[start : start+subSize]), true
				}
				cursor = start + subSize
				if subSize%2 == 1 {
					cursor++
				}
			}
		}
		offset = body + size
		if size%2 == 1 {
			offset++
		}
	}
	return "", false
}
