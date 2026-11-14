// Package provider image wire forms: the three shapes an image can take on the
// way out, and the capability declaration that decides which one is used.
//
// A model that accepts images does not accept them in one shape:
//
//	file_id — a provider-side file handle (OpenAI Responses input_image.file_id,
//	          Anthropic image source {type:"file"}, Gemini file_data.file_uri)
//	url     — an absolute URL the provider fetches (Anthropic image source
//	          {type:"url"}, OpenAI-compatible image_url, Gemini file_uri)
//	base64  — inline bytes as a data URI or a vendor base64 block (universal, and
//	          the fallback when nothing else is)
//
// Declaring them on the *driver* (not guessing from the model name) is what lets
// the resolver choose; MaxImageBytes bounds what the deployment will send and
// MaxInlineBytes bounds what it will inline. PI's equivalent is the boolean
// "input includes image"; Keel needs the finer grain because a base64-only
// gateway must re-transfer bytes on every turn (FILE-PROTOCOL.md §2.4).
package provider

import (
	"fmt"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
)

// Image wire form names. They are the *resolver's* vocabulary; the encoders
// switch on the populated field, never on a string.
const (
	ImageFormFileID = "file_id"
	ImageFormURL    = "url"
	ImageFormBase64 = "base64"
)

// ImageCaps is the image transport capability declaration of one driver. The
// zero value means "no declaration": the resolver then behaves as base64-only,
// which every vendor supports, so an undeclared driver keeps today's behavior.
type ImageCaps struct {
	// SupportsFileID declares that a provider-side upload/handle round trip is
	// available for this endpoint.
	SupportsFileID bool
	// SupportsImageURL declares that the endpoint fetches image URLs.
	SupportsImageURL bool
	// MaxImageBytes is the largest original image this provider accepts. 0 means
	// undeclared (no local limit enforced).
	MaxImageBytes int64
	// MaxInlineBytes bounds the base64 fallback specifically (some gateways have a
	// much smaller request-body cap than their per-file cap; DashScope's domain
	// limit is 16MiB). 0 means MaxImageBytes.
	MaxInlineBytes int64
}

// inlineLimit returns the effective base64 cap.
func (c ImageCaps) inlineLimit() int64 {
	if c.MaxInlineBytes > 0 {
		return c.MaxInlineBytes
	}
	return c.MaxImageBytes
}

// BestForm reports the preferred wire form for this declaration, following the
// fixed degradation order file_id > url > base64.
func (c ImageCaps) BestForm() string {
	switch {
	case c.SupportsFileID:
		return ImageFormFileID
	case c.SupportsImageURL:
		return ImageFormURL
	default:
		return ImageFormBase64
	}
}

// ImageCapsForAPI returns the image declaration of one wire protocol, which is
// how the product layer states it: the resolved model names its API type, and the
// protocol is the fact that decides which image shapes can travel. Keeping the
// table keyed by protocol (not by provider name) means a custom gateway inherits
// exactly the shapes its wire supports.
//
// An unknown protocol yields the base64-only zero value: the universal shape, and
// the one a wire this deployment does not know must be assumed to accept.
func ImageCapsForAPI(apiType string) ImageCaps {
	switch apiType {
	case "openai-chat-completions":
		// image_url takes a data URI or an absolute URL; the chat wire has no
		// file_id (the vendor's own reference says file_id is Responses-only).
		return ImageCaps{SupportsImageURL: true, MaxImageBytes: 20 << 20}
	case "openai-responses":
		// input_image takes image_url and file_id, which is why this protocol
		// leads the degradation order with the handle form.
		return ImageCaps{SupportsFileID: true, SupportsImageURL: true, MaxImageBytes: 20 << 20}
	case "anthropic-messages":
		// image source takes base64, url, or a file_id (Files API).
		return ImageCaps{SupportsFileID: true, SupportsImageURL: true, MaxImageBytes: 20 << 20}
	default:
		return ImageCaps{}
	}
}

// imageWire is the neutral description of one outbound image, decoded from a
// content block. Exactly one source is set.
type imageWire struct {
	Mime   string
	Data   string // base64 payload without the data: prefix
	URL    string
	FileID string
}

// form names the populated source.
func (w imageWire) form() string {
	switch {
	case w.FileID != "":
		return ImageFormFileID
	case w.URL != "":
		return ImageFormURL
	case w.Data != "":
		return ImageFormBase64
	}
	return ""
}

// dataURI renders the base64 form as the data URI the OpenAI-compatible wires
// take.
func (w imageWire) dataURI() string {
	return "data:" + w.mime() + ";base64," + w.Data
}

func (w imageWire) mime() string {
	if w.Mime != "" {
		return w.Mime
	}
	return "application/octet-stream"
}

// splitImage decodes one ImageContent block into its wire description. A block
// with no source, or with more than one, is a programming error (the resolver
// sets exactly one) and is reported rather than guessed at: silently picking a
// field would send the wrong bytes or nothing at all.
func splitImage(c agentcore.ImageContent) (imageWire, error) {
	wire := imageWire{Mime: c.MimeType, Data: c.Data, URL: c.URL, FileID: c.FileID}
	count := 0
	for _, set := range []bool{wire.Data != "", wire.URL != "", wire.FileID != ""} {
		if set {
			count++
		}
	}
	switch count {
	case 1:
		return wire, nil
	case 0:
		return imageWire{}, fmt.Errorf("image block carries no source (fileId, url or data)")
	default:
		return imageWire{}, fmt.Errorf("image block carries more than one source (fileId=%t url=%t data=%t)",
			wire.FileID != "", wire.URL != "", wire.Data != "")
	}
}

// requireForm returns an error naming the unsupported form and the provider, so
// an image the driver cannot carry fails the request with a readable message
// instead of being silently dropped (the failure mode this change exists to
// remove).
func requireForm(providerName string, wire imageWire, supported ...string) error {
	form := wire.form()
	for _, allowed := range supported {
		if form == allowed {
			return nil
		}
	}
	return fmt.Errorf("%s: image form %q is not supported by this wire (supported: %s); the resolver should have chosen one of those",
		providerName, form, strings.Join(supported, ", "))
}
