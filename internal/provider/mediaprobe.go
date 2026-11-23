package provider

// Cheap attachment metadata (change keel-harness-v2 modality-input lane).
//
// Owner constraint (2026-11-23): metadata must be dependency-free and bounded.
// This file therefore uses only the standard library:
//
//	image width/height   image.DecodeConfig (header-only; jpeg/png/gif registered)
//	WAV duration         a RIFF `fmt `/`data` chunk walk over the header
//	PDF pages            deliberately not parsed (a pure-Go parser is multi-MB);
//	                     the fallback text says "pages unknown"
//	MP4/MOV dimensions   deliberately not parsed (the box walk buys little the
//	                     model cannot get from the file itself); the fallback
//	                     says the duration/dimensions are unknown
//
// Everything operates on the bounded header read by MediaResolver.Probe; the
// whole attachment is never materialised for metadata. An unregistered image
// format (WebP without x/image/webp, HEIC) yields "unknown" rather than a
// dependency.

import (
	"bytes"
	"encoding/binary"
	"image"
	_ "image/gif"  // register the GIF decoder (stdlib)
	_ "image/jpeg" // register the JPEG decoder (stdlib)
	_ "image/png"  // register the PNG decoder (stdlib)
)

// probeHeader fills what the bounded header can prove and leaves the rest zero.
func probeHeader(facts *MediaFacts, header []byte) {
	if facts == nil || len(header) == 0 {
		return
	}
	if facts.Width == 0 || facts.Height == 0 {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(header)); err == nil {
			if cfg.Width > 0 && cfg.Height > 0 {
				facts.Width, facts.Height = cfg.Width, cfg.Height
			}
		}
	}
	if facts.DurationMs == 0 && isWav(*facts) {
		facts.DurationMs = wavDurationMs(header)
	}
	// PDF pages stay 0: see the file comment. The fallback text reports "pages
	// unknown" rather than guessing from a byte count or a parser dependency.
}

// isWav reports whether the facts describe a WAV container, by MIME type or
// (when the blob store did not know it) by the display name's extension.
func isWav(facts MediaFacts) bool {
	switch facts.Mime {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return true
	}
	return lowerExt(facts.Name) == ".wav"
}

// wavDurationMs parses the RIFF header far enough to compute duration: the
// fmt chunk's byte rate and the data chunk's size. It returns 0 (unknown) for
// anything that is not a plain PCM/float WAV; a compressed WAV's data size does
// not divide by the byte rate.
func wavDurationMs(header []byte) int64 {
	if len(header) < 12 {
		return 0
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return 0
	}
	var byteRate uint32
	var dataBytes uint32
	pos := 12
	for pos+8 <= len(header) {
		id := string(header[pos : pos+4])
		size := binary.LittleEndian.Uint32(header[pos+4 : pos+8])
		body := pos + 8
		switch id {
		case "fmt ":
			// PCM/float format tags are 1 or 3; a compressed tag has no honest
			// byte-rate duration.
			if size >= 16 && body+16 <= len(header) {
				format := binary.LittleEndian.Uint16(header[body : body+2])
				if format != 1 && format != 3 {
					return 0
				}
				byteRate = binary.LittleEndian.Uint32(header[body+8 : body+12])
			}
		case "data":
			// The declared data size may exceed the header; that is fine — the
			// duration comes from the declared size, not the bytes read.
			if byteRate > 0 {
				dataBytes = size
				if dataBytes == 0 {
					return 0
				}
				return int64(dataBytes) * 1000 / int64(byteRate)
			}
		}
		if size == 0 {
			break
		}
		// Chunks are word-aligned: an odd size is followed by one pad byte.
		next := body + int(size)
		if size%2 == 1 {
			next++
		}
		if next <= pos {
			break
		}
		pos = next
	}
	return 0
}
