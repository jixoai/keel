package attachment

// AttachmentPart is the read-only attachment projection the host publishes to a
// plugin UI (change keel-plugin-attachment, spec: 插件只看到附件元数据).
//
// It carries metadata and same-origin URLs only: no bytes, base64, data URI,
// CAS path, tenant header, cookie or credential ever belongs in this shape.
// Name and width/height are display facts supplied by the event payload; MIME,
// size and thumbnail lifecycle come from the blob record (the authority).

import "strings"

// Part is the projection. Field names are the contract the web client reads.
type Part struct {
	FileID       string   `json:"fileId"`
	Name         string   `json:"name,omitempty"`
	Mime         string   `json:"mime"`
	Size         int64    `json:"size"`
	Kind         string   `json:"kind"`
	Width        int      `json:"width,omitempty"`
	Height       int      `json:"height,omitempty"`
	ThumbStatus  string   `json:"thumbStatus"`
	ThumbnailURL string   `json:"thumbnailUrl,omitempty"`
	DownloadURL  string   `json:"downloadUrl"`
	Delivery     Delivery `json:"delivery"`
}

// Delivery is the attempt's transport form. The host computes it from the model
// declaration and wire capability; a plugin never decides "can the model see
// this".
type Delivery struct {
	Native bool   `json:"native"`
	Reason string `json:"reason,omitempty"`
}

// BlobFacts are the server-authoritative facts a projection needs. They mirror
// the blob row; keeping them here avoids this package depending on the store.
type BlobFacts struct {
	Mime        string
	Size        int64
	ThumbStatus string // none | pending | ready | failed
}

// BuildPart merges the durable reference's display metadata with the blob's
// authoritative facts and the attempt's delivery result. The MIME is the blob
// record's (normalized), never the client's declaration and never inferred from
// a display name.
func BuildPart(ref Ref, facts BlobFacts, delivery Delivery) Part {
	mime := NormalizeMime(facts.Mime)
	part := Part{
		FileID:      ref.FileID,
		Name:        strings.TrimSpace(ref.Name),
		Mime:        mime,
		Size:        facts.Size,
		Kind:        KindForMime(mime, ref.Name),
		Width:       ref.Width,
		Height:      ref.Height,
		ThumbStatus: thumbStatus(facts.ThumbStatus),
		DownloadURL: "/v1/files/" + ref.FileID,
		Delivery:    delivery,
	}
	if part.ThumbStatus == "ready" {
		part.ThumbnailURL = "/v1/files/" + ref.FileID + "/thumbnail"
	}
	return part
}

func thumbStatus(value string) string {
	switch value {
	case "pending", "ready", "failed":
		return value
	default:
		return "none"
	}
}

// KindForMime classifies an attachment for the projection. A non-empty MIME is
// the only authority; the display name's extension is consulted solely when the
// blob record has no MIME at all (a name can lie, a stored type cannot).
func KindForMime(mime, name string) string {
	normalized := NormalizeMime(mime)
	if normalized != "" {
		switch {
		case normalized == "application/pdf":
			return "pdf"
		case strings.HasPrefix(normalized, "image/"):
			return "image"
		case strings.HasPrefix(normalized, "audio/"):
			return "audio"
		case strings.HasPrefix(normalized, "video/"):
			return "video"
		default:
			return "file"
		}
	}
	switch strings.ToLower(extension(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic", ".heif", ".bmp", ".tif", ".tiff":
		return "image"
	case ".pdf":
		return "pdf"
	case ".wav", ".mp3", ".m4a", ".aac", ".ogg", ".flac":
		return "audio"
	case ".mp4", ".mov", ".webm", ".mkv", ".avi", ".m4v":
		return "video"
	}
	return "file"
}

func extension(name string) string {
	name = strings.TrimSpace(name)
	if index := strings.LastIndexByte(name, '.'); index >= 0 {
		return name[index:]
	}
	return ""
}
