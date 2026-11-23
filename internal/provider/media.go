package provider

// Input modality dispatch (change keel-harness-v2, modality-input lane).
//
// The durable transcript stores *references* (ImageRefContent for images,
// MediaRefContent for everything else). One attempt's request boundary decides,
// per attachment, whether the selected model accepts the modality and whether
// this wire protocol can carry it natively:
//
//	reference ──▶ modality ──▶ model declaration (agent.ModelProperties)
//	                          └▶ wire declaration (MediaCapsForAPI)
//	                                ├ native  → provider part (file/audio/image)
//	                                └ fallback → metadata text block + tools hint
//
// The matrix below is the truthful statement of what the adapters in this
// package encode. A modality is native only where the *wire* documents the part
// AND the adapter implements it; everything else degrades to a text block that
// names the blob and tells the model to inspect it with file tools. No byte
// stream is dropped silently, and no native part is produced for a modality the
// wire cannot carry (the upstream would reject the whole request).
//
// Per-protocol matrix (native parts implemented by this package):
//
//	                 image          pdf/document        audio            video
//	openai-chat      image_url      file (PDF only)     input_audio      -
//	openai-responses input_image    input_file          - (1)            -
//	anthropic        image          document            -                -
//	unknown/other    base64 image   -                   -                -
//
//  1. The Responses API's input_audio type exists in SDK typings but the
//     upstream rejects it at runtime (audio input is documented for Chat
//     Completions); claiming it would fake a part. Audio on Responses degrades.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
)

// File wire form names (the resolver's vocabulary; encoders switch on fields).
const (
	FileFormFileID = ImageFormFileID
	FileFormURL    = ImageFormURL
	FileFormInline = ImageFormBase64
)

// FileCaps is the document transport declaration of one wire protocol.
type FileCaps struct {
	// SupportsFileID declares a provider-side file handle round trip (Files API
	// upload + a file_id/file reference in the request).
	SupportsFileID bool
	// SupportsFileURL declares the wire fetches an absolute document URL.
	SupportsFileURL bool
	// SupportsInline declares base64 bytes in the request body.
	SupportsInline bool
	// Mimes lists the document MIME types this wire can carry natively. Only
	// application/pdf is claimed today: the adapters encode that shape.
	Mimes []string
	// MaxFileBytes bounds the original document regardless of form.
	MaxFileBytes int64
	// MaxInlineBytes bounds the base64 fallback.
	MaxInlineBytes int64
}

// SupportsMime reports whether the wire carries mime natively.
func (c FileCaps) SupportsMime(mime string) bool {
	mime = normalizeMime(mime)
	for _, supported := range c.Mimes {
		if mime == supported {
			return true
		}
	}
	return false
}

// BestForm reports the preferred document form, following file_id > url > inline.
func (c FileCaps) BestForm() string {
	switch {
	case c.SupportsFileID:
		return FileFormFileID
	case c.SupportsFileURL:
		return FileFormURL
	case c.SupportsInline:
		return FileFormInline
	default:
		return ""
	}
}

// AudioCaps is the audio transport declaration of one wire protocol. Audio is
// inline-only: no wire this package speaks accepts an audio URL or handle.
type AudioCaps struct {
	SupportsInline bool
	// Formats lists the wire audio formats (extension-style tokens, not MIME).
	Formats []string
	// MaxInlineBytes bounds the inline payload.
	MaxInlineBytes int64
}

// SupportsFormat reports whether the wire accepts format ("wav"/"mp3").
func (c AudioCaps) SupportsFormat(format string) bool {
	format = strings.ToLower(strings.TrimSpace(format))
	for _, supported := range c.Formats {
		if format == supported {
			return true
		}
	}
	return false
}

// MediaCaps is one wire protocol's complete input-media declaration.
type MediaCaps struct {
	Image ImageCaps
	File  FileCaps
	Audio AudioCaps
	// Video reports native video input. No wire this package speaks carries it
	// today; the field exists so a future adapter can declare it without a new
	// type, and so the dispatch has one place to read.
	Video bool
}

// IsEmpty reports that the declaration carries no capability at all. It is the
// seam the runtime uses to tell "no MediaCaps was set" from a deliberate zero
// declaration (for example a model that denies every modality).
func (c MediaCaps) IsEmpty() bool {
	return !c.Image.SupportsFileID && !c.Image.SupportsImageURL && c.Image.MaxImageBytes == 0 &&
		!c.File.SupportsFileID && !c.File.SupportsFileURL && !c.File.SupportsInline &&
		len(c.File.Mimes) == 0 && c.File.MaxFileBytes == 0 &&
		!c.Audio.SupportsInline && len(c.Audio.Formats) == 0 && c.Audio.MaxInlineBytes == 0 &&
		!c.Video
}

// MediaCapsForAPI returns the media declaration of one wire protocol. An
// unknown protocol declares nothing: the dispatcher then uses the text fallback
// for PDF/audio/video and the universally supported base64 image shape.
func MediaCapsForAPI(apiType string) MediaCaps {
	switch apiType {
	case "openai-chat-completions":
		return MediaCaps{
			Image: ImageCaps{SupportsImageURL: true, MaxImageBytes: 20 << 20},
			File: FileCaps{
				SupportsFileID: true,
				SupportsInline: true,
				Mimes:          []string{"application/pdf"},
				MaxFileBytes:   50 << 20,
				MaxInlineBytes: 20 << 20,
			},
			Audio: AudioCaps{SupportsInline: true, Formats: []string{"wav", "mp3"}, MaxInlineBytes: 25 << 20},
		}
	case "openai-responses":
		return MediaCaps{
			Image: ImageCaps{SupportsFileID: true, SupportsImageURL: true, MaxImageBytes: 20 << 20},
			File: FileCaps{
				SupportsFileID:  true,
				SupportsFileURL: true,
				SupportsInline:  true,
				Mimes:           []string{"application/pdf"},
				MaxFileBytes:    50 << 20,
				MaxInlineBytes:  20 << 20,
			},
			// Audio: see the matrix note (1) above; not native.
		}
	case "anthropic-messages":
		return MediaCaps{
			// Image: url/base64 only, for the same reason the document file_id
			// form is not claimed below: the message request carries no Files
			// API beta header.
			Image: ImageCaps{SupportsImageURL: true, MaxImageBytes: 20 << 20},
			File: FileCaps{
				// The Messages API document source accepts base64 and url today.
				// A file_id source additionally requires the Files API beta header on
				// the *message* request, which this driver does not send, so that form
				// is not claimed (see docs/MODEL-INPUT-MODALITIES.md).
				SupportsFileID:  false,
				SupportsFileURL: true,
				SupportsInline:  true,
				Mimes:           []string{"application/pdf"},
				MaxFileBytes:    50 << 20,
				MaxInlineBytes:  32 << 20,
			},
			// Audio/video: the Messages API has no such content block.
		}
	default:
		return MediaCaps{}
	}
}

// normalizeMime lowercases and strips parameters from a MIME type.
func normalizeMime(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	return mime
}

// MediaKindFromMime classifies an attachment by MIME type, falling back to the
// filename extension. It returns one of the agentcore.MediaKind* constants.
func MediaKindFromMime(mime, name string) string {
	mime = normalizeMime(mime)
	switch {
	case mime == "application/pdf":
		return agentcore.MediaKindPDF
	case strings.HasPrefix(mime, "audio/"):
		return agentcore.MediaKindAudio
	case strings.HasPrefix(mime, "video/"):
		return agentcore.MediaKindVideo
	case strings.HasPrefix(mime, "image/"):
		// Images have their own durable reference; callers that reach here with
		// an image MIME should treat it as a generic file rather than guess.
		return agentcore.MediaKindFile
	}
	switch lowerExt(name) {
	case ".pdf":
		return agentcore.MediaKindPDF
	case ".wav", ".mp3", ".m4a", ".aac", ".ogg", ".flac":
		return agentcore.MediaKindAudio
	case ".mp4", ".mov", ".webm", ".mkv", ".avi", ".m4v":
		return agentcore.MediaKindVideo
	}
	return agentcore.MediaKindFile
}

// lowerExt returns the lowercase extension of a display name ("a.PDF" → ".pdf").
func lowerExt(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return strings.ToLower(name[i:])
	}
	return ""
}

// AudioFormatFromMime maps an audio MIME type to the wire format token the
// OpenAI chat input_audio part accepts. An unsupported type yields "".
func AudioFormatFromMime(mime, name string) string {
	switch normalizeMime(mime) {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	}
	switch lowerExt(name) {
	case ".wav":
		return "wav"
	case ".mp3":
		return "mp3"
	}
	return ""
}

// ResolvedFile is the resolver's verdict for one document reference.
type ResolvedFile struct {
	Content agentcore.FileContent
	Form    string
	Note    string
}

// ResolvedAudio is the resolver's verdict for one audio reference.
type ResolvedAudio struct {
	Content agentcore.AudioContent
	Form    string
	Note    string
}

// fileWire is the neutral description of one outbound document. Exactly one
// source is set.
type fileWire struct {
	Mime   string
	Name   string
	Data   string // base64 payload without the data: prefix
	URL    string
	FileID string
}

// form names the populated source.
func (w fileWire) form() string {
	switch {
	case w.FileID != "":
		return FileFormFileID
	case w.URL != "":
		return FileFormURL
	case w.Data != "":
		return FileFormInline
	}
	return ""
}

// dataURI renders the inline form as the data URL the OpenAI wires require.
func (w fileWire) dataURI() string {
	return "data:" + w.mime() + ";base64," + w.Data
}

// filename is the wire filename (an inline document needs one).
func (w fileWire) filename() string {
	if name := strings.TrimSpace(w.Name); name != "" {
		return name
	}
	return "attachment" + extensionForMime(w.mime())
}

func (w fileWire) mime() string {
	if w.Mime != "" {
		return w.Mime
	}
	return "application/octet-stream"
}

// splitFile decodes one FileContent block into its wire description. A block
// with no source, or with more than one, is a programming error (the resolver
// sets exactly one) and is reported rather than guessed at.
func splitFile(c agentcore.FileContent) (fileWire, error) {
	wire := fileWire{Mime: c.MimeType, Name: c.Name, Data: c.Data, URL: c.URL, FileID: c.FileID}
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
		return fileWire{}, fmt.Errorf("document block carries no source (fileId, url or data)")
	default:
		return fileWire{}, fmt.Errorf("document block carries more than one source (fileId=%t url=%t data=%t)",
			wire.FileID != "", wire.URL != "", wire.Data != "")
	}
}

// MediaResolver is the image resolver plus the non-image attachment resolvers
// and the bounded metadata probe. Embedding ImageResolver keeps one blob source,
// one file cache and one uploader stack: an attachment and an image uploaded for
// the same account must share the handle cache.
type MediaResolver struct {
	ImageResolver
}

// mediaHeaderBytes bounds the header read of the metadata probe. It is small
// enough that a 2GB video costs 256KiB of transient memory, and large enough for
// a JPEG whose EXIF block precedes the frame header.
const mediaHeaderBytes = 256 << 10

// MediaFacts are the cheap metadata the fallback text names. None of these
// require reading the whole attachment: size/MIME come from blob metadata,
// Width/Height from client metadata or a bounded image header, Pages/Duration
// only when a cheap header parser produced them. Zero means unknown and the
// fallback says so explicitly.
type MediaFacts struct {
	BlobID     string
	Name       string
	Mime       string
	Size       int64
	Width      int
	Height     int
	Pages      int
	DurationMs int64
	// HeaderRead reports whether a bounded header was actually read (so a
	// "unknown" dimension can be distinguished from "no bytes readable").
	HeaderRead bool
}

// Probe reads the cheap metadata of one stored attachment. It never reads the
// whole blob: metadata comes from BlobInfo, dimensions from a bounded image
// header, and duration from a WAV header. Unsupported formats return what is
// known (size/MIME) and leave the rest zero — the fallback text then says
// "unknown" instead of guessing.
func (r *MediaResolver) Probe(ctx context.Context, workspace string, ref agentcore.MediaRefContent) (MediaFacts, error) {
	facts := MediaFacts{
		BlobID: strings.TrimSpace(ref.BlobID),
		Name:   ref.Name,
		Mime:   normalizeMime(ref.MimeType),
		Size:   ref.Size,
		Width:  ref.Width,
		Height: ref.Height,
	}
	if r == nil || r.Blobs == nil {
		return facts, errors.New("provider: media resolver has no blob source")
	}
	if facts.BlobID == "" {
		return facts, errors.New("provider: attachment carries no blobId")
	}
	info, err := r.Blobs.Info(ctx, workspace, facts.BlobID)
	if err != nil {
		return facts, fmt.Errorf("provider: attachment %s: %w", facts.BlobID, err)
	}
	if info.Size >= 0 {
		facts.Size = info.Size
	}
	if facts.Mime == "" {
		facts.Mime = normalizeMime(info.Mime)
	}
	// Dimensions already supplied by the client (the uploader read them from the
	// decoded image) cost no I/O and win.
	if facts.Width > 0 && facts.Height > 0 {
		return facts, nil
	}
	header, err := r.readHeader(ctx, workspace, facts.BlobID)
	if err != nil {
		// Metadata is best-effort: the reference is still named by blob id,
		// size and MIME, so a header failure must not become a second failure.
		return facts, nil
	}
	facts.HeaderRead = true
	probeHeader(&facts, header)
	return facts, nil
}

// readHeader opens the blob and reads at most mediaHeaderBytes. The reader is
// closed immediately; a stream that errors after the header still yields the
// bytes read so far.
func (r *MediaResolver) readHeader(ctx context.Context, workspace, blobID string) ([]byte, error) {
	rc, err := r.Blobs.Open(ctx, workspace, blobID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	buf := make([]byte, mediaHeaderBytes)
	n, err := readAtMost(rc, buf)
	if n == 0 && err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// readAtMost fills buf until EOF or an error, reporting how many bytes arrived.
func readAtMost(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, nil
		}
	}
	return total, nil
}

// ResolveFile turns one durable document reference into the wire form the target
// accepts, following file_id > url > inline. A refusal is an error naming the
// reason; the caller turns it into the fallback text block.
func (r *MediaResolver) ResolveFile(ctx context.Context, workspace string, target ImageTarget, ref agentcore.MediaRefContent, caps FileCaps) (ResolvedFile, error) {
	if r == nil || r.Blobs == nil {
		return ResolvedFile{}, errors.New("provider: media resolver has no blob source")
	}
	blobID := strings.TrimSpace(ref.BlobID)
	if blobID == "" {
		return ResolvedFile{}, errors.New("provider: attachment carries no blobId")
	}
	info, err := r.Blobs.Info(ctx, workspace, blobID)
	if err != nil {
		return ResolvedFile{}, fmt.Errorf("provider: attachment %s: %w", blobID, err)
	}
	mime := normalizeMime(info.Mime)
	if mime == "" {
		mime = normalizeMime(ref.MimeType)
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	// The form check precedes the MIME check: a wire that declares no document
	// form at all cannot carry any document, and that is the more informative
	// refusal (the caller's fallback text states it once).
	if caps.BestForm() == "" {
		return ResolvedFile{}, fmt.Errorf("provider: document %s cannot travel natively: the wire has no file_id, url or inline form", blobID)
	}
	if !caps.SupportsMime(mime) {
		// Also refuse a document whose MIME the blob store did not know but whose
		// name says PDF: the bytes may still be a PDF, but the wire's native claim
		// is only made for the declared MIME.
		if kind := MediaKindFromMime(mime, ref.Name); kind == agentcore.MediaKindPDF {
			mime = "application/pdf"
		}
		if !caps.SupportsMime(mime) {
			return ResolvedFile{}, fmt.Errorf("provider: document type %q is not carried natively by this wire (supported: %s)",
				mime, strings.Join(caps.Mimes, ", "))
		}
	}
	if caps.MaxFileBytes > 0 && info.Size > caps.MaxFileBytes {
		return ResolvedFile{}, fmt.Errorf("provider: attachment %s is %d bytes, provider %s accepts at most %d",
			blobID, info.Size, target.Provider, caps.MaxFileBytes)
	}
	name := displayFilename(ref.Name, blobID, mime)

	note := ""
	// 1. file_id — cheapest, requires a vendor upload path and the same account.
	if caps.SupportsFileID && r.Files != nil {
		uploader := r.uploaderFor(target)
		if uploader == nil {
			note = fmt.Sprintf("provider %s declares file support but has no document upload path wired", target.Provider)
		} else {
			file, err := r.Files.GetOrUpload(ctx, uploader, workspace, info, name, r.blobOpener())
			if err != nil {
				note = fmt.Sprintf("provider file upload failed (%v)", err)
			} else {
				return ResolvedFile{
					Content: agentcore.FileContent{
						Type:     agentcore.ContentTypeFile,
						FileID:   file.FileID,
						MimeType: mime,
						Name:     name,
					},
					Form: FileFormFileID,
				}, nil
			}
		}
	}

	// 2. url — the provider fetches the bytes itself.
	if caps.SupportsFileURL && r.URLs != nil {
		url, urlErr := r.URLs.ImageURL(ctx, workspace, info, r.urlTTL())
		if urlErr == nil && strings.TrimSpace(url) != "" {
			content := agentcore.FileContent{Type: agentcore.ContentTypeFile, URL: url, MimeType: mime, Name: name}
			if note != "" {
				return ResolvedFile{Content: content, Form: FileFormURL, Note: note + "; degraded to url"}, nil
			}
			return ResolvedFile{Content: content, Form: FileFormURL}, nil
		}
		if urlErr != nil && note == "" {
			note = fmt.Sprintf("document url unavailable (%v)", urlErr)
		}
	}

	// 3. inline base64, bounded by the declared inline cap.
	if !caps.SupportsInline {
		reason := fmt.Sprintf("provider: document %s cannot travel natively: the wire has no file_id, url or inline form", blobID)
		if note != "" {
			reason = note + "; " + reason
		}
		return ResolvedFile{}, errors.New(reason)
	}
	limit := caps.MaxInlineBytes
	if limit <= 0 {
		limit = caps.MaxFileBytes
	}
	if limit > 0 && base64WireLen(info.Size) > limit {
		reason := fmt.Sprintf("document %s is %d bytes (%d base64), inline limit for provider %s is %d",
			blobID, info.Size, base64WireLen(info.Size), target.Provider, limit)
		if note != "" {
			reason = note + "; " + reason
		}
		return ResolvedFile{}, fmt.Errorf("provider: %s", reason)
	}
	wire, err := r.encodeBase64(ctx, workspace, blobID, info.Size)
	if err != nil {
		if note != "" {
			return ResolvedFile{}, fmt.Errorf("provider: document %s: %s; inline fallback failed: %w", blobID, note, err)
		}
		return ResolvedFile{}, fmt.Errorf("provider: document %s: %w", blobID, err)
	}
	return ResolvedFile{
		Content: agentcore.FileContent{Type: agentcore.ContentTypeFile, Data: wire, MimeType: mime, Name: name},
		Form:    FileFormInline,
		Note:    note,
	}, nil
}

// ResolveAudio turns one durable audio reference into inline base64. No wire this
// package speaks accepts an audio URL or handle, so the only native form is
// inline; a format the wire does not accept is refused (the caller falls back to
// text rather than sending bytes the upstream would reject).
func (r *MediaResolver) ResolveAudio(ctx context.Context, workspace string, target ImageTarget, ref agentcore.MediaRefContent, caps AudioCaps) (ResolvedAudio, error) {
	if r == nil || r.Blobs == nil {
		return ResolvedAudio{}, errors.New("provider: media resolver has no blob source")
	}
	blobID := strings.TrimSpace(ref.BlobID)
	if blobID == "" {
		return ResolvedAudio{}, errors.New("provider: attachment carries no blobId")
	}
	if !caps.SupportsInline {
		return ResolvedAudio{}, fmt.Errorf("provider: audio %s cannot travel natively: this wire has no audio input part", blobID)
	}
	info, err := r.Blobs.Info(ctx, workspace, blobID)
	if err != nil {
		return ResolvedAudio{}, fmt.Errorf("provider: attachment %s: %w", blobID, err)
	}
	mime := normalizeMime(info.Mime)
	if mime == "" {
		mime = normalizeMime(ref.MimeType)
	}
	format := AudioFormatFromMime(mime, ref.Name)
	if format == "" || !caps.SupportsFormat(format) {
		return ResolvedAudio{}, fmt.Errorf("provider: audio format %q is not carried natively by this wire (supported: %s)",
			mime, strings.Join(caps.Formats, ", "))
	}
	if caps.MaxInlineBytes > 0 && info.Size > caps.MaxInlineBytes {
		return ResolvedAudio{}, fmt.Errorf("provider: audio %s is %d bytes, provider %s accepts at most %d",
			blobID, info.Size, target.Provider, caps.MaxInlineBytes)
	}
	wire, err := r.encodeBase64(ctx, workspace, blobID, info.Size)
	if err != nil {
		return ResolvedAudio{}, fmt.Errorf("provider: audio %s: %w", blobID, err)
	}
	return ResolvedAudio{
		Content: agentcore.AudioContent{
			Type:     agentcore.ContentTypeAudio,
			Data:     wire,
			MimeType: mime,
			Format:   format,
			Name:     ref.Name,
		},
		Form: FileFormInline,
	}, nil
}

// displayFilename picks the wire filename: the client's display name when it
// looks like a bare name, otherwise a synthesized one that keeps the extension
// the provider keys off.
func displayFilename(name, blobID, mime string) string {
	base := strings.TrimSpace(name)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if base != "" && len(base) <= 120 {
		return base
	}
	ext := extensionForMime(mime)
	if blobID != "" {
		return "attachment-" + blobID + ext
	}
	return "attachment" + ext
}
