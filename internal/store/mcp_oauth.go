// MCP OAuth persistence (internal/mcp/oauth): encrypted credentials, pending
// browser steps, and the cross-process refresh lock.
//
// The runtime replacement for PI's JSON files and proper-lockfile is
// PostgreSQL: credentials are AES-256-GCM ciphertext keyed by
// (tenant, server) and bound to that pair as additional authenticated data,
// pending rows make a sign-in completable in another request, and a session
// advisory lock on a dedicated lock pool keeps concurrent processes from
// refreshing (and rotating) the same refresh token twice.

package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSecretKeyMissing means KEEL_SECRET_KEY is not configured. MCP OAuth
// features must fail with this error instead of storing plaintext.
var ErrSecretKeyMissing = errors.New("store: KEEL_SECRET_KEY is required for MCP OAuth")

// ErrMCPOAuthNotFound means no row exists for the tenant/server (or the state).
var ErrMCPOAuthNotFound = errors.New("store: MCP OAuth record not found")

// ErrMCPOAuthRefreshLockBusy means another process held the refresh lock (or
// the lock pool had no free connection) for the whole wait.
var ErrMCPOAuthRefreshLockBusy = errors.New("store: another process is refreshing the MCP server's tokens")

const (
	// secretPayloadVersion is the envelope version. A future key rotation writes
	// a new version and key id, so old payloads stay readable.
	secretPayloadVersion byte = 1
	secretKeyIDSize           = 8
	secretHeaderSize          = 1 + secretKeyIDSize
)

// SecretBox seals and opens MCP OAuth payloads with AES-256-GCM. The envelope is
// version || key id || nonce || ciphertext; the caller supplies additional
// authenticated data (tenant id + server) so a ciphertext cannot be moved
// between rows.
type SecretBox struct {
	aead  cipher.AEAD
	keyID [secretKeyIDSize]byte
}

// NewSecretBox builds a box from a 32-byte key.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("store: MCP OAuth secret key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("store: MCP OAuth cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: MCP OAuth GCM: %w", err)
	}
	digest := sha256.Sum256(key)
	box := &SecretBox{aead: aead}
	copy(box.keyID[:], digest[:secretKeyIDSize])
	return box, nil
}

// SecretKeyFromEnv reads KEEL_SECRET_KEY.
func SecretKeyFromEnv(lookup func(string) string) ([]byte, error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	raw := lookup("KEEL_SECRET_KEY")
	if raw == "" {
		return nil, ErrSecretKeyMissing
	}
	return ParseSecretKey(raw)
}

// ParseSecretKey decodes a configured key. Only 32-byte keys encoded as base64
// (standard or URL, padded or not) or hex are accepted: a 32-character raw
// string would be an ambiguous, likely weak passphrase.
func ParseSecretKey(raw string) ([]byte, error) {
	if decoded, err := hex.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(raw); err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("store: KEEL_SECRET_KEY must be a 32-byte key encoded as base64 or hex")
}

// Seal encrypts payload with a fresh nonce. aad must be the same value at Open;
// see MCPOAuthAAD.
func (b *SecretBox) Seal(payload, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, secretHeaderSize+len(nonce)+len(payload)+b.aead.Overhead())
	out = append(out, secretPayloadVersion)
	out = append(out, b.keyID[:]...)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, payload, aad), nil
}

// Open decrypts a sealed payload and verifies its version, key id, and AAD.
func (b *SecretBox) Open(sealed, aad []byte) ([]byte, error) {
	nonceSize := b.aead.NonceSize()
	if len(sealed) < secretHeaderSize+nonceSize {
		return nil, errors.New("store: MCP OAuth payload is truncated")
	}
	if sealed[0] != secretPayloadVersion {
		return nil, fmt.Errorf("store: unsupported MCP OAuth payload version %d", sealed[0])
	}
	if !hmac.Equal(sealed[1:secretHeaderSize], b.keyID[:]) {
		return nil, errors.New("store: MCP OAuth payload was sealed with a different key (rotate KEEL_SECRET_KEY deliberately)")
	}
	nonce := sealed[secretHeaderSize : secretHeaderSize+nonceSize]
	plaintext, err := b.aead.Open(nil, nonce, sealed[secretHeaderSize+nonceSize:], aad)
	if err != nil {
		return nil, fmt.Errorf("store: MCP OAuth payload cannot be decrypted: %w", err)
	}
	return plaintext, nil
}

// MCPOAuthAAD binds a sealed payload to its tenant and server.
func MCPOAuthAAD(tenantID uuid.UUID, server string) []byte {
	aad := make([]byte, 0, len(tenantID)+1+len(server))
	aad = append(aad, tenantID[:]...)
	aad = append(aad, 0)
	aad = append(aad, server...)
	return aad
}

// MCPOAuthCredentials is one encrypted OAuth state document.
type MCPOAuthCredentials struct {
	TenantID uuid.UUID
	Server   string
	// Payload is plaintext JSON in memory and ciphertext at rest.
	Payload   []byte
	Scopes    string
	ExpiresAt *time.Time
	Version   int
	UpdatedAt time.Time
}

// LoadMCPOAuthCredentials reads and decrypts one server's credentials. It
// returns ErrMCPOAuthNotFound when no row exists and ErrSecretKeyMissing when
// box is nil (OAuth is not configured, so nothing is ever stored plaintext).
func (s *Store) LoadMCPOAuthCredentials(ctx context.Context, tenantID uuid.UUID, server string, box *SecretBox) (MCPOAuthCredentials, error) {
	if box == nil {
		return MCPOAuthCredentials{}, ErrSecretKeyMissing
	}
	var row MCPOAuthCredentials
	var sealed []byte
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, server, payload, COALESCE(scopes, ''), expires_at, version, updated_at
		 FROM mcp_oauth_credentials WHERE tenant_id = $1 AND server = $2`,
		tenantID, server).Scan(&row.TenantID, &row.Server, &sealed, &row.Scopes, &row.ExpiresAt, &row.Version, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MCPOAuthCredentials{}, ErrMCPOAuthNotFound
	}
	if err != nil {
		return MCPOAuthCredentials{}, fmt.Errorf("store: load MCP OAuth credentials: %w", err)
	}
	plaintext, err := box.Open(sealed, MCPOAuthAAD(row.TenantID, row.Server))
	if err != nil {
		return MCPOAuthCredentials{}, err
	}
	row.Payload = plaintext
	return row, nil
}

// SaveMCPOAuthCredentials encrypts and upserts one server's credentials.
func (s *Store) SaveMCPOAuthCredentials(ctx context.Context, credentials MCPOAuthCredentials, box *SecretBox) error {
	if box == nil {
		return ErrSecretKeyMissing
	}
	if credentials.Server == "" {
		return errors.New("store: MCP OAuth server name is required")
	}
	sealed, err := box.Seal(credentials.Payload, MCPOAuthAAD(credentials.TenantID, credentials.Server))
	if err != nil {
		return err
	}
	version := credentials.Version
	if version <= 0 {
		version = 1
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO mcp_oauth_credentials (tenant_id, server, payload, scopes, expires_at, version)
		 VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6)
		 ON CONFLICT (tenant_id, server) DO UPDATE
		 SET payload = EXCLUDED.payload, scopes = EXCLUDED.scopes,
		     expires_at = EXCLUDED.expires_at, version = EXCLUDED.version,
		     updated_at = now()`,
		credentials.TenantID, credentials.Server, sealed, credentials.Scopes, credentials.ExpiresAt, version)
	if err != nil {
		return fmt.Errorf("store: save MCP OAuth credentials: %w", err)
	}
	return nil
}

// DeleteMCPOAuthCredentials drops one server's credentials (sign-out).
func (s *Store) DeleteMCPOAuthCredentials(ctx context.Context, tenantID uuid.UUID, server string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM mcp_oauth_credentials WHERE tenant_id = $1 AND server = $2`,
		tenantID, server); err != nil {
		return fmt.Errorf("store: delete MCP OAuth credentials: %w", err)
	}
	return nil
}

// TenantExists reports whether the tenant row exists. Sign-in refuses unknown
// tenants instead of creating credential rows for arbitrary ids.
func (s *Store) TenantExists(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tenants WHERE tenant_id = $1)`, tenantID).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: tenant exists: %w", err)
	}
	return exists, nil
}

// MCPOAuthPending is one pending browser step.
type MCPOAuthPending struct {
	TenantID         uuid.UUID
	Server           string
	State            string
	CodeVerifier     string
	RedirectURL      string
	AuthorizationURL string
	Scopes           string
	ExpiresAt        time.Time
	ConsumedAt       *time.Time
	CreatedAt        time.Time
}

// StartMCPOAuthPending supersedes the server's active pending steps and inserts
// this one. A per-(tenant, server) transaction advisory lock serializes
// concurrent starts, and a partial unique index allows at most one active row.
func (s *Store) StartMCPOAuthPending(ctx context.Context, pending MCPOAuthPending) error {
	if pending.State == "" || pending.CodeVerifier == "" || pending.RedirectURL == "" {
		return errors.New("store: MCP OAuth pending needs state, verifier, and redirect URI")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: pending begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", mcpOAuthPendingLockKey(pending.TenantID, pending.Server)); err != nil {
		return fmt.Errorf("store: pending lock: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE mcp_oauth_pending SET consumed_at = now()
		 WHERE tenant_id = $1 AND server = $2 AND consumed_at IS NULL`,
		pending.TenantID, pending.Server); err != nil {
		return fmt.Errorf("store: supersede MCP OAuth pending: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO mcp_oauth_pending
		 (tenant_id, server, state, code_verifier, redirect_uri, authorization_url, scopes, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8)`,
		pending.TenantID, pending.Server, pending.State, pending.CodeVerifier,
		pending.RedirectURL, pending.AuthorizationURL, pending.Scopes, pending.ExpiresAt); err != nil {
		return fmt.Errorf("store: insert MCP OAuth pending: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: pending commit: %w", err)
	}
	return nil
}

func scanPending(row pgx.Row) (MCPOAuthPending, error) {
	var pending MCPOAuthPending
	err := row.Scan(&pending.TenantID, &pending.Server, &pending.State, &pending.CodeVerifier,
		&pending.RedirectURL, &pending.AuthorizationURL, &pending.Scopes,
		&pending.ExpiresAt, &pending.ConsumedAt, &pending.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MCPOAuthPending{}, ErrMCPOAuthNotFound
	}
	if err != nil {
		return MCPOAuthPending{}, err
	}
	return pending, nil
}

const pendingColumns = `tenant_id, server, state, code_verifier, redirect_uri,
	authorization_url, COALESCE(scopes, ''), expires_at, consumed_at, created_at`

// GetMCPOAuthPending reads one pending step, consumed or not.
func (s *Store) GetMCPOAuthPending(ctx context.Context, tenantID uuid.UUID, server, state string) (MCPOAuthPending, error) {
	pending, err := scanPending(s.pool.QueryRow(ctx,
		`SELECT `+pendingColumns+` FROM mcp_oauth_pending
		 WHERE tenant_id = $1 AND server = $2 AND state = $3`, tenantID, server, state))
	if err != nil {
		if errors.Is(err, ErrMCPOAuthNotFound) {
			return MCPOAuthPending{}, err
		}
		return MCPOAuthPending{}, fmt.Errorf("store: get MCP OAuth pending: %w", err)
	}
	return pending, nil
}

// FindMCPOAuthPendingByState finds a pending step by its unguessable state. The
// browser callback route has no tenant header, so state is the capability.
func (s *Store) FindMCPOAuthPendingByState(ctx context.Context, state string) (MCPOAuthPending, error) {
	pending, err := scanPending(s.pool.QueryRow(ctx,
		`SELECT `+pendingColumns+` FROM mcp_oauth_pending WHERE state = $1`, state))
	if err != nil {
		if errors.Is(err, ErrMCPOAuthNotFound) {
			return MCPOAuthPending{}, err
		}
		return MCPOAuthPending{}, fmt.Errorf("store: find MCP OAuth pending: %w", err)
	}
	return pending, nil
}

// ConsumeMCPOAuthPending atomically marks the pending step used and returns it.
// Exactly one concurrent caller receives the row; the others get
// ErrMCPOAuthNotFound. Consume before exchanging the code so only one caller
// can spend it.
func (s *Store) ConsumeMCPOAuthPending(ctx context.Context, tenantID uuid.UUID, server, state string) (MCPOAuthPending, error) {
	pending, err := scanPending(s.pool.QueryRow(ctx,
		`UPDATE mcp_oauth_pending SET consumed_at = now()
		 WHERE tenant_id = $1 AND server = $2 AND state = $3 AND consumed_at IS NULL
		 RETURNING `+pendingColumns, tenantID, server, state))
	if errors.Is(err, ErrMCPOAuthNotFound) {
		return MCPOAuthPending{}, err
	}
	if err != nil {
		return MCPOAuthPending{}, fmt.Errorf("store: consume MCP OAuth pending: %w", err)
	}
	return pending, nil
}

// PruneMCPOAuthPending deletes browser steps that can no longer complete:
// consumed rows (finished or superseded) and rows past their expiry, both only
// once they are older than the caller's cutoff. The cutoff is a retention
// window: a recent row stays readable for duplicate-callback diagnostics, and
// an unexpired, unconsumed row is never deleted because a browser callback may
// still arrive for it. It returns the number of rows removed.
//
// This is housekeeping, not authentication: pruning a consumed row only loses
// the audit of the browser step; credentials live in mcp_oauth_credentials.
func (s *Store) PruneMCPOAuthPending(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		before = time.Now()
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM mcp_oauth_pending
		 WHERE (consumed_at IS NOT NULL AND consumed_at <= $1) OR expires_at <= $1`, before)
	if err != nil {
		return 0, fmt.Errorf("store: prune MCP OAuth pending: %w", err)
	}
	return tag.RowsAffected(), nil
}

// WithMCPOAuthRefreshLock runs fn while holding the (tenant, server) refresh
// lock across processes: a session advisory lock on a connection from the
// dedicated lock pool, released when fn returns. A wait longer than wait
// reports ErrMCPOAuthRefreshLockBusy. The lock is not reentrant.
//
// The lock connection never comes from the main pool: callbacks load and save
// state on the main pool, and sharing one pool would let MaxConns lock holders
// deadlock the whole runtime.
func (s *Store) WithMCPOAuthRefreshLock(ctx context.Context, tenantID uuid.UUID, server string, wait time.Duration, fn func(ctx context.Context) error) error {
	if fn == nil {
		return nil
	}
	if wait <= 0 {
		wait = 30 * time.Second
	}
	key := mcpOAuthLockKey(tenantID, server)

	// The pool acquire is bounded by wait; the lock wait gets a small grace so
	// lock_timeout (not the client context) decides whether another process is
	// holding the lock too long.
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, wait)
	defer cancelAcquire()
	conn, err := s.lockPool.Acquire(acquireCtx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrMCPOAuthRefreshLockBusy
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, wait+5*time.Second)
	defer cancelLock()
	lockHeld := false
	finished := false
	defer func() {
		if finished {
			return
		}
		if lockHeld {
			if err := s.unlockMCPOAuth(conn, key); err == nil {
				s.releaseMCPOAuthLockConn(conn)
			}
			return
		}
		s.releaseMCPOAuthLockConn(conn)
	}()

	if _, err := conn.Exec(lockCtx, fmt.Sprintf("SET lock_timeout = %d", wait.Milliseconds())); err != nil {
		return fmt.Errorf("store: MCP OAuth lock setup: %w", err)
	}
	if _, err := conn.Exec(lockCtx, "SELECT pg_advisory_lock($1)", key); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "57014") {
			return ErrMCPOAuthRefreshLockBusy
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(lockCtx.Err(), context.DeadlineExceeded) {
			return ErrMCPOAuthRefreshLockBusy
		}
		return fmt.Errorf("store: MCP OAuth lock: %w", err)
	}
	lockHeld = true

	runErr := fn(ctx)

	if err := s.unlockMCPOAuth(conn, key); err != nil {
		finished = true // the connection was destroyed by unlockMCPOAuth
		return runErr
	}
	lockHeld = false
	s.releaseMCPOAuthLockConn(conn)
	finished = true
	return runErr
}

// unlockMCPOAuth releases the advisory lock. If that fails the connection is
// destroyed instead of being pooled, so a lock can never leak to a later user.
func (s *Store) unlockMCPOAuth(conn *pgxpool.Conn, key int64) error {
	unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlocked bool
	err := conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", key).Scan(&unlocked)
	if err == nil && unlocked {
		return nil
	}
	if raw := conn.Hijack(); raw != nil {
		_ = raw.Close(context.Background())
	}
	if err != nil {
		return fmt.Errorf("store: MCP OAuth unlock: %w", err)
	}
	return fmt.Errorf("store: MCP OAuth lock was not held at unlock")
}

// releaseMCPOAuthLockConn resets the per-session lock timeout before returning
// the connection to the lock pool. A failed reset destroys the connection: a
// stale lock_timeout must not leak into a later holder.
func (s *Store) releaseMCPOAuthLockConn(conn *pgxpool.Conn) {
	resetCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := conn.Exec(resetCtx, "SET lock_timeout = 0"); err != nil {
		if raw := conn.Hijack(); raw != nil {
			_ = raw.Close(context.Background())
		}
		return
	}
	conn.Release()
}

func mcpOAuthLockKey(tenantID uuid.UUID, server string) int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("keel:mcp-oauth\x00"))
	_, _ = hash.Write([]byte(tenantID.String()))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(server))
	return int64(hash.Sum64())
}

func mcpOAuthPendingLockKey(tenantID uuid.UUID, server string) int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("keel:mcp-oauth-pending\x00"))
	_, _ = hash.Write([]byte(tenantID.String()))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(server))
	return int64(hash.Sum64())
}
