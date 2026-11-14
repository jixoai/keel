package blob

// The image resolver's view of the blob store (keel-ga §3 provider-files).
//
// The properties pinned here are the ones the outbound path depends on:
//
//   - Info reads metadata only (the resolver sizes and form-selects before it
//     touches a byte);
//   - EncodeBase64 fills a caller-sized buffer and refuses a wrong-sized one (a
//     short buffer would silently truncate the image);
//   - Open streams, so an upload never materialises the image.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"testing"

	"github.com/google/uuid"
)

func TestImageSourceMetadataAndBase64(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "image-source"); err != nil {
		t.Fatal(err)
	}
	uploader := &Uploader{Store: st, CASDir: t.TempDir(), PGThreshold: 8 << 20}
	data := pattern(200_000)
	meta, err := uploader.Put(ctx, tenant, "image/png", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	source := &ImageSource{Store: st, Reader: NewReader(st, 4, 1)}

	info, err := source.Info(ctx, tenant.String(), meta.BlobID.String())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Size != int64(len(data)) || info.Mime != "image/png" || info.BlobID != meta.BlobID.String() {
		t.Fatalf("info = %+v, want the stored metadata", info)
	}
	sum := sha256.Sum256(data)
	if info.SHA256 != hexDigest(sum[:]) {
		t.Fatalf("sha256 = %q, want the content digest", info.SHA256)
	}

	// Exactly-sized buffer: encoded bytes match the canonical base64 form.
	want := base64.StdEncoding.EncodeToString(data)
	dst := make([]byte, WireLen(meta.Size))
	if err := source.EncodeBase64(ctx, tenant.String(), meta.BlobID.String(), dst); err != nil {
		t.Fatalf("EncodeBase64: %v", err)
	}
	if string(dst) != want {
		t.Fatal("encoded bytes differ from the canonical base64 form")
	}

	// A wrong-sized buffer is refused before any read: a short buffer would
	// truncate the image silently.
	if err := source.EncodeBase64(ctx, tenant.String(), meta.BlobID.String(), make([]byte, 16)); err == nil {
		t.Fatal("a short buffer must be refused")
	}
}

func TestImageSourceStreamsRawBytes(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "image-open"); err != nil {
		t.Fatal(err)
	}
	uploader := &Uploader{Store: st, CASDir: t.TempDir(), PGThreshold: 8 << 20}
	data := pattern(150_000)
	meta, err := uploader.Put(ctx, tenant, "image/jpeg", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	source := &ImageSource{Store: st, Reader: NewReader(st, 4, 1)}

	stream, err := source.Open(ctx, tenant.String(), meta.BlobID.String())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close()
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("streamed %d bytes, want the original %d", len(got), len(data))
	}
}

func TestImageSourceRefusesBadScopes(t *testing.T) {
	st := testStore(t)
	source := &ImageSource{Store: st, Reader: NewReader(st, 2, 1)}
	ctx := context.Background()

	if _, err := source.Info(ctx, "", uuid.NewString()); err == nil {
		t.Fatal("an empty workspace must be refused")
	}
	if _, err := source.Info(ctx, "not-a-uuid", uuid.NewString()); err == nil {
		t.Fatal("a malformed workspace must be refused")
	}
	if _, err := source.Info(ctx, uuid.NewString(), "not-a-uuid"); err == nil {
		t.Fatal("a malformed blob id must be refused")
	}
	if _, err := source.Info(ctx, uuid.NewString(), uuid.NewString()); err == nil {
		t.Fatal("an unknown blob must be an error, not an empty image")
	}
}

// hexDigest mirrors the source's digest rendering for the assertion.
func hexDigest(sum []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range sum {
		out[i*2] = digits[b>>4]
		out[i*2+1] = digits[b&0x0f]
	}
	return string(out)
}
