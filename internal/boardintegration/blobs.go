package boardintegration

// keelBlobs adapts keel's tenant blob store to the board module's key-addressed
// blob.Store (keel-board-v1 task 12.4).
//
// The board names a blob by a key of the form
// tenants/<sha256(tenant)>/attachments/<id>; keel stores bytes by
// (tenant, sha256). board_tenant_blob_keys is the translation row. The bytes
// themselves go through keel's normal uploader/reader, so attachment reads
// return real bytes and the existing lifecycle (dedupe/CAS/GC) still applies.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/store"

	boardblob "github.com/jixoai/keel-board/blob"
)

// keelBlobs implements boardblob.Store.
type keelBlobs struct {
	store    *store.Store
	reader   *blob.Reader
	uploader *blob.Uploader

	mu      sync.Mutex
	tenants map[string]uuid.UUID
}

func newKeelBlobs(st *store.Store, casDir string) *keelBlobs {
	if st == nil {
		return nil
	}
	return &keelBlobs{
		store:  st,
		reader: blob.NewReaderWithCAS(st, 256, 4, casDir),
		uploader: &blob.Uploader{
			Store: st, CASDir: casDir, TempDir: "",
			// Board attachments are bounded by the board's own 25 MiB limit;
			// keel's default ceiling is higher, so it never truncates one.
		},
		tenants: make(map[string]uuid.UUID),
	}
}

func (b *keelBlobs) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if b == nil {
		return errors.New("boardintegration: blob store is not wired")
	}
	tenantID, err := b.tenantForKey(ctx, key)
	if err != nil {
		return err
	}
	meta, err := b.uploader.Put(ctx, tenantID, contentType, bytes.NewReader(data))
	if err != nil {
		return err
	}
	return b.store.PutBoardBlobKey(ctx, store.BoardBlobKey{
		Key: key, TenantID: tenantID, BlobID: meta.BlobID,
		ContentType: contentType, Size: meta.Size,
	})
}

func (b *keelBlobs) Get(ctx context.Context, key string) ([]byte, string, error) {
	if b == nil {
		return nil, "", boardblob.ErrNotFound
	}
	mapping, found, err := b.mappingForKey(ctx, key)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "", boardblob.ErrNotFound
	}
	meta, err := b.store.BlobMetaOf(ctx, mapping.TenantID, mapping.BlobID)
	if err != nil {
		if errors.Is(err, store.ErrNoBlob) {
			return nil, "", boardblob.ErrNotFound
		}
		return nil, "", err
	}
	var buffer bytes.Buffer
	if _, err := b.reader.StreamRaw(ctx, mapping.TenantID, mapping.BlobID, meta, &buffer, 0, 0); err != nil {
		return nil, "", err
	}
	return buffer.Bytes(), meta.Mime, nil
}

func (b *keelBlobs) Stat(ctx context.Context, key string) (int64, error) {
	if b == nil {
		return 0, boardblob.ErrNotFound
	}
	mapping, found, err := b.mappingForKey(ctx, key)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, boardblob.ErrNotFound
	}
	return mapping.Size, nil
}

func (b *keelBlobs) Delete(ctx context.Context, key string) error {
	if b == nil {
		return nil
	}
	// Deleting a key that isn't there is not an error, per the board's
	// contract. Verify the key's tenant segment matches the mapping before the
	// delete so a forged cross-tenant key cannot remove another tenant's row.
	mapping, found, err := b.mappingForKey(ctx, key)
	if err != nil || !found {
		return err
	}
	_, _, err = b.store.DeleteBoardBlobKey(ctx, mapping.Key)
	return err
}

// mappingForKey reads a translation row only when the key's tenant segment
// matches the row's tenant. The key is otherwise opaque; this is the tenant
// predicate the board's key-addressed interface cannot supply.
func (b *keelBlobs) mappingForKey(ctx context.Context, key string) (store.BoardBlobKey, bool, error) {
	mapping, found, err := b.store.GetBoardBlobKey(ctx, key)
	if err != nil || !found {
		return mapping, found, err
	}
	hash, err := tenantHashFromKey(key)
	if err != nil {
		return store.BoardBlobKey{}, false, nil
	}
	if hashTenant(mapping.TenantID) != hash {
		return store.BoardBlobKey{}, false, nil
	}
	return mapping, true, nil
}

// tenantForKey resolves the tenant hash the board placed in a key. The board
// hashes the tenant string (sha256), and keel's tenant ids are UUID strings, so
// the adapter matches against the tenant list. The list is small (100-200 by
// architecture design) and cached; a miss refreshes once so a tenant created
// after boot is found.
func (b *keelBlobs) tenantForKey(ctx context.Context, key string) (uuid.UUID, error) {
	hash, err := tenantHashFromKey(key)
	if err != nil {
		return uuid.Nil, err
	}
	b.mu.Lock()
	cached, ok := b.tenants[hash]
	b.mu.Unlock()
	if ok {
		return cached, nil
	}
	if err := b.refreshTenants(ctx); err != nil {
		return uuid.Nil, err
	}
	b.mu.Lock()
	cached, ok = b.tenants[hash]
	b.mu.Unlock()
	if !ok {
		return uuid.Nil, fmt.Errorf("boardintegration: no keel tenant matches the blob key's tenant")
	}
	return cached, nil
}

func (b *keelBlobs) refreshTenants(ctx context.Context) error {
	tenants, err := b.store.ListTenants(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]uuid.UUID, len(tenants))
	for _, record := range tenants {
		next[hashTenant(record.TenantID)] = record.TenantID
	}
	b.mu.Lock()
	b.tenants = next
	b.mu.Unlock()
	return nil
}

func hashTenant(tenantID uuid.UUID) string {
	sum := sha256.Sum256([]byte(tenantID.String()))
	return hex.EncodeToString(sum[:])
}

func tenantHashFromKey(key string) (string, error) {
	const prefix = "tenants/"
	if !strings.HasPrefix(key, prefix) {
		return "", errors.New("boardintegration: blob key is outside tenants/")
	}
	rest := strings.TrimPrefix(key, prefix)
	index := strings.IndexByte(rest, '/')
	if index <= 0 {
		return "", errors.New("boardintegration: blob key has no tenant segment")
	}
	hash := rest[:index]
	if len(hash) != 64 {
		return "", errors.New("boardintegration: blob key tenant segment is not a sha256")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", errors.New("boardintegration: blob key tenant segment is not hex")
	}
	return hash, nil
}
