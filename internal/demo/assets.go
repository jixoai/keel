package demo

import (
	"bytes"
	"embed"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
)

// The demo's hosted assets: the images an image turn returns and the tone a
// speech turn plays. They are compiled into the binary (not read from disk) so
// a deployment cannot serve a demo asset it did not build with, and so the demo
// keeps working from a container with no extra volume.
//
//go:embed assets_*.png
var assetFS embed.FS

// Asset names as they appear in the hosted URL (/v1/demo/assets/<name>.png).
const (
	AssetSquare    = "square"
	AssetPortrait  = "portrait"
	AssetLandscape = "landscape"
)

// Asset describes one hosted image: its name, its pixel size and its bytes.
type Asset struct {
	Name   string
	Width  int
	Height int
	Bytes  []byte
}

// assetSize is the pixel size of every compiled-in tile. The tiles are small on
// purpose: a demo image only has to render in a gallery, and a 32×32 PNG is a
// few hundred bytes, so the binary stays small and a review page loads instantly.
const assetSize = 32

var (
	assetsOnce sync.Once
	assetList  []Asset
	assetErr   error
)

// Assets returns the hosted images in a stable order. The slice and the byte
// slices are copied: a caller cannot mutate the compiled-in data.
func Assets() []Asset {
	assetsOnce.Do(func() {
		for _, name := range []string{AssetSquare, AssetPortrait, AssetLandscape} {
			raw, err := assetFS.ReadFile("assets_" + name + ".png")
			if err != nil {
				assetErr = fmt.Errorf("demo: read asset %q: %w", name, err)
				return
			}
			assetList = append(assetList, Asset{
				Name:   name,
				Width:  assetSize,
				Height: assetSize,
				Bytes:  raw,
			})
		}
	})
	if assetErr != nil {
		panic(assetErr.Error())
	}
	out := make([]Asset, len(assetList))
	for i, asset := range assetList {
		out[i] = Asset{
			Name:   asset.Name,
			Width:  asset.Width,
			Height: asset.Height,
			Bytes:  append([]byte(nil), asset.Bytes...),
		}
	}
	return out
}

// AssetByName returns one hosted image. A name that is not one of the compiled-in
// tiles is not found: the HTTP handler must never turn a path into a file read
// (the demo asset surface is a closed set, not a static directory).
func AssetByName(name string) (Asset, bool) {
	wanted := strings.TrimSpace(strings.TrimSuffix(strings.ToLower(name), ".png"))
	for _, asset := range Assets() {
		if asset.Name == wanted {
			return asset, true
		}
	}
	return Asset{}, false
}

// AssetURL is the path an image turn returns. It is server-relative: the client
// resolves it against the origin it already talks to, so the demo works behind
// any deployment host without configuring one.
func AssetURL(name string) string {
	return "/v1/demo/assets/" + name + ".png"
}

// AssetForSize picks the tile for a requested size. The request's box selects the
// closest tile by aspect ratio, so a portrait request gets the portrait tile and
// a square request the square one; the choice is deterministic and never depends
// on a clock or a random seed. An empty or unreadable size yields the square tile.
func AssetForSize(size string) Asset {
	width, height, ok := parseSize(size)
	if !ok || width <= 0 || height <= 0 {
		return mustAsset(AssetSquare)
	}
	// Only the orientation matters: the tiles are all the same pixel size, and the
	// gallery renders them at their declared size.
	switch {
	case width*100 > height*115: // wider than ~1.15:1
		return mustAsset(AssetLandscape)
	case height*100 > width*115: // taller than ~1.15:1
		return mustAsset(AssetPortrait)
	default:
		return mustAsset(AssetSquare)
	}
}

// mustAsset resolves a compiled-in asset or panics: a missing tile is a build
// defect, and silently returning an empty image would render a broken gallery.
func mustAsset(name string) Asset {
	asset, ok := AssetByName(name)
	if !ok {
		panic("demo: missing compiled-in asset " + name)
	}
	return asset
}

// parseSize splits a "WxH" box. It mirrors the gateway's own parser so the demo
// accepts exactly the sizes the validation allowlist admits.
func parseSize(size string) (int, int, bool) {
	left, right, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x")
	if !ok {
		return 0, 0, false
	}
	var width, height int
	if _, err := fmt.Sscanf(left, "%d", &width); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(right, "%d", &height); err != nil {
		return 0, 0, false
	}
	if width <= 0 || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// Audio constants for the synthesized speech clip.
const (
	// AudioSampleRate is the WAV sample rate. 16 kHz mono 16-bit is the smallest
	// combination every browser plays without resampling surprises.
	AudioSampleRate = 16000
	// AudioChannels is mono.
	AudioChannels = 1
	// AudioBitsPerSample is 16-bit signed little-endian PCM.
	AudioBitsPerSample = 16
	// audioToneHz is the confirmation tone's frequency: a soft A4-ish pitch, not
	// an alarm.
	audioToneHz = 440.0
	// audioToneSeconds is how long the clip lasts. A speech turn is a short
	// confirmation, not a paragraph: one second is enough for the player to start
	// and for a reviewer to hear that audio arrived.
	audioToneSeconds = 1
	// audioAmplitude scales the sine wave to about -12 dBFS, so the clip is
	// audible without being startling.
	audioAmplitude = 0.25
)

// SpeechWAV returns the fixed confirmation clip a demo speech turn plays: a WAV
// container (RIFF/PCM) carrying a one-second 440 Hz tone with a short fade in and
// out so it does not click.
//
// It is generated here rather than stored as a file so the bytes are exactly
// reproducible and the repository carries no binary audio blob. The synthesis is
// pure arithmetic: no clock, no randomness, so two calls are byte-identical.
func SpeechWAV() []byte {
	samples := AudioSampleRate * audioToneSeconds
	pcm := make([]byte, samples*AudioChannels*AudioBitsPerSample/8)
	fade := AudioSampleRate / 50 // 20 ms
	for i := 0; i < samples; i++ {
		// A short linear fade keeps the first and last samples at zero, which is
		// what stops a click at playback start/stop.
		gain := 1.0
		if i < fade {
			gain = float64(i) / float64(fade)
		} else if tail := samples - 1 - i; tail < fade {
			// The last sample is index samples-1, so its tail is 0: the clip ends on
			// exact silence, which is what stops a click at the end of playback.
			gain = float64(tail) / float64(fade)
		}
		value := audioAmplitude * gain * sine(audioToneHz*float64(i)/AudioSampleRate)
		sample := int16(value * 32767)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
	}
	return wavContainer(pcm)
}

// sine returns sin(2π·turns) reduced to [-1,1].
//
// The demo clip only has to sound like a clean pitch at 16-bit precision, so the
// reduction is done here rather than pulling in math just for one call: the
// argument is folded onto [-π/2, π/2] and evaluated with a Maclaurin series. The
// accuracy that matters is audible (a few least-significant bits are inaudible at
// quarter scale), and folding first is what keeps the series accurate — an
// unfolded series over a large argument diverges and produced a clipped waveform.
func sine(turns float64) float64 {
	const twoPi = 2 * 3.141592653589793
	const halfPi = 3.141592653589793 / 2

	// Reduce to a phase in [0, 2π).
	phase := turns - float64(int(turns))
	if phase < 0 {
		phase += 1
	}
	angle := phase * twoPi

	// Fold into [-π/2, π/2] using the quadrant identities:
	// sin(π - x) = sin(x), sin(π + x) = -sin(x), sin(2π - x) = -sin(x).
	const pi = 3.141592653589793
	sign := 1.0
	switch {
	case angle > 3*halfPi: // (3π/2, 2π]: sin(x) = -sin(2π - x)
		angle = twoPi - angle
		sign = -1
	case angle > halfPi: // (π/2, 3π/2]: upper half mirrors, lower half negates
		if angle <= pi {
			angle = pi - angle
		} else {
			angle -= pi
			sign = -1
		}
	}

	// Maclaurin series on [-π/2, π/2]: truncation error at π/2 is < 1e-8 with
	// these terms, far below 16-bit resolution.
	x2 := angle * angle
	return sign * angle * (1 - x2/6*(1-x2/20*(1-x2/42)))
}

// wavContainer wraps PCM bytes in a canonical 44-byte RIFF/WAVE header.
func wavContainer(pcm []byte) []byte {
	const headerSize = 44
	out := make([]byte, 0, headerSize+len(pcm))
	appendU32 := func(value uint32) { out = binary.LittleEndian.AppendUint32(out, value) }
	appendU16 := func(value uint16) { out = binary.LittleEndian.AppendUint16(out, value) }
	byteRate := AudioSampleRate * AudioChannels * AudioBitsPerSample / 8
	blockAlign := AudioChannels * AudioBitsPerSample / 8

	out = append(out, []byte("RIFF")...)
	appendU32(uint32(headerSize - 8 + len(pcm)))
	out = append(out, []byte("WAVE")...)
	out = append(out, []byte("fmt ")...)
	appendU32(16)                 // fmt chunk size
	appendU16(1)                  // PCM
	appendU16(AudioChannels)      // channels
	appendU32(AudioSampleRate)    // sample rate
	appendU32(uint32(byteRate))   // byte rate
	appendU16(uint16(blockAlign)) // block align
	appendU16(AudioBitsPerSample) // bits per sample
	out = append(out, []byte("data")...)
	appendU32(uint32(len(pcm)))
	return append(out, pcm...)
}

// WAVContentType is the media type a speech endpoint answers with.
const WAVContentType = "audio/wav"

// wavHeaderProbe is the first four bytes of any generated clip. Tests assert it
// so the container cannot silently become a bare PCM stream.
var wavHeaderProbe = []byte("RIFF")

// SpeechWAVLooksValid reports whether the generated clip starts with the WAV
// container header. It exists for the HTTP layer's self-check test.
func SpeechWAVLooksValid(clip []byte) bool {
	return len(clip) > len(wavHeaderProbe) && bytes.Equal(clip[:len(wavHeaderProbe)], wavHeaderProbe)
}

// AssetPathPrefix is the URL prefix every hosted asset shares, without a trailing
// file name. Callers matching a reference use it instead of building the prefix out
// of AssetURL(""), which would append ".png" to nothing.
const AssetPathPrefix = "/v1/demo/assets/"
