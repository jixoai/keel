// Error classification for the bot-identity persistence layer
// (change keel-bot-identity-access, A1).
//
// The identity store methods answer three questions a caller must be able to
// tell apart without parsing prose:
//
//   - ErrNotFound: the tenant-scoped row does not exist (another tenant's row
//     answers the same way);
//   - ErrConflict: a uniqueness/foreign-key conflict with stored state (a
//     duplicate username, a profile that does not exist, a moved revision);
//   - ErrInvalid: the caller-supplied value fails store-level validation (a
//     nil tenant id, a non-JSON document, an unknown scope).
//
// Every returned error wraps the sentinel, so errors.Is is the contract.
package store

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Identity-store sentinels. They are separate from ErrNoSession/ErrNoTenant/
// ErrNoAPIKey, which stay the older surfaces' contracts.
var (
	// ErrNotFound reports a tenant-scoped row that does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict reports a uniqueness or reference conflict with stored state.
	ErrConflict = errors.New("store: conflict")
	// ErrInvalid reports a caller-supplied value that fails store validation.
	ErrInvalid = errors.New("store: invalid input")
)

// identityWriteError classifies one write error. No rows is "not found", unique
// and foreign-key violations are conflicts, not-null/check/format violations
// are invalid input, and everything else is returned wrapped but unclassified.
func identityWriteError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%s: %w: %v", op, ErrConflict, err)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%s: %w: %v", op, ErrConflict, err)
		case "23502", "23514", "22P02": // not_null, check, invalid_text_representation
			return fmt.Errorf("%s: %w: %v", op, ErrInvalid, err)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

// invalidIdentity wraps a caller-validation failure with ErrInvalid.
func invalidIdentity(op, format string, args ...any) error {
	return fmt.Errorf("%s: %w: %s", op, ErrInvalid, fmt.Sprintf(format, args...))
}

// conflictIdentity wraps a compare-and-swap failure with ErrConflict.
func conflictIdentity(op, format string, args ...any) error {
	return fmt.Errorf("%s: %w: %s", op, ErrConflict, fmt.Sprintf(format, args...))
}

// identityListLimit bounds every identity listing. Zero uses the default; a
// larger value is clamped, never honoured unbounded. Keyset pagination can be
// added later without changing the "bounded" contract.
const (
	defaultIdentityListLimit = 100
	maxIdentityListLimit     = 500
)

func identityListLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultIdentityListLimit
	case limit > maxIdentityListLimit:
		return maxIdentityListLimit
	default:
		return limit
	}
}
