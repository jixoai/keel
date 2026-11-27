package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/boardintegration"
	"github.com/jixoai/keel/internal/store"
)

// boardDeploymentEnabled reads the deployment-level board switch.
//
// The board is OFF by default (keel-board-v1 design §20: /v1/board/* returns
// 404 board_disabled until a deployment opts in). KEEL_BOARD_ENABLED=1 (or
// true/yes/on) enables the surface; an empty or 0/false/no/off value keeps the
// routes mounted but answers 404 board_disabled. The tenant-level board.enabled
// and board.sync.enabled flags narrow this deployment switch further; they
// cannot enable a deployment that never opted in. An unrecognized value fails
// startup instead of guessing.
func boardDeploymentEnabled() (bool, error) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("KEEL_BOARD_ENABLED")))
	switch raw {
	case "1", "true", "yes", "on":
		return true, nil
	case "", "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("KEEL_BOARD_ENABLED=%q: want 1/true/yes/on or 0/false/no/off", raw)
	}
}

// boardDSN returns the database the board's own migrations and RLS use.
//
// KEEL_BOARD_DSN is the dedicated-database/role option: board's policies read
// keel.tenant_id while keel's own policies read app.tenant_id, and keeping the
// board's tables out of keel's rlsTenantTables avoids applying two tenant
// predicates to one table. A deployment that wants a hard non-owner boundary
// points this at a non-superuser role that owns the board tables (FORCE RLS
// then applies to the owner too). Empty falls back to keel's DSN, which keeps
// one database for local/single-tenant deployments.
func boardDSN(keelDSN string) string {
	if dedicated := strings.TrimSpace(os.Getenv("KEEL_BOARD_DSN")); dedicated != "" {
		return dedicated
	}
	return keelDSN
}

// boardFileHosts reads the inbound file/link URL host allowlist. Empty is the
// safe default: the sync host refuses every inbound URL rather than fetching an
// arbitrary host named by a platform payload.
func boardFileHosts() []string {
	raw := strings.TrimSpace(os.Getenv("KEEL_BOARD_FILE_HOSTS"))
	if raw == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if host := strings.TrimSpace(part); host != "" {
			out = append(out, host)
		}
	}
	return out
}

// boardProviderHosts reads the custom platform API endpoint allowlist. Empty
// keeps every adapter on its built-in endpoint.
func boardProviderHosts() []string {
	raw := strings.TrimSpace(os.Getenv("KEEL_BOARD_PROVIDER_HOSTS"))
	if raw == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if host := strings.TrimSpace(part); host != "" {
			out = append(out, host)
		}
	}
	return out
}

// boardWiring is the board surface as the serve loop mounts it. A disabled
// deployment still supplies handlers (404 board_disabled) so the route shape
// does not change with the flag.
type boardWiring struct {
	API     http.Handler
	Webhook http.Handler
	Surface *boardintegration.Surface
}

// newBoardWiring selects the board handlers for this deployment. It is the
// tested seam between the flag and the HTTP mux; the disabled path never opens
// a board connection.
func newBoardWiring(ctx context.Context, keelDSN string, tenantID uuid.UUID, access *accesswire.Bridge, st *store.Store, logger *slog.Logger) (*boardWiring, error) {
	return newBoardWiringWithOptions(ctx, keelDSN, tenantID, access, st, logger, nil)
}

// newBoardWiringWithOptions is newBoardWiring plus the sync wiring a serve
// process supplies (tenant secret box, blob directory, file/link allowlist,
// limits). Keeping it separate leaves the default-off seam testable without
// constructing a store or a secret key.
func newBoardWiringWithOptions(ctx context.Context, keelDSN string, tenantID uuid.UUID, access *accesswire.Bridge, st *store.Store, logger *slog.Logger, configure func(*boardintegration.Options)) (*boardWiring, error) {
	enabled, err := boardDeploymentEnabled()
	if err != nil {
		return nil, err
	}
	if !enabled {
		return &boardWiring{
			API:     boardintegration.DisabledHandler(),
			Webhook: boardintegration.DisabledHandler(),
		}, nil
	}
	options := boardintegration.Options{
		DSN:                 boardDSN(keelDSN),
		Schema:              strings.TrimSpace(os.Getenv("KEEL_BOARD_SCHEMA")),
		Access:              access,
		Store:               st,
		TenantID:            tenantID,
		Logger:              logger,
		ChannelScanInterval: envDuration("KEEL_BOARD_CHANNEL_SCAN", 0),
	}
	if configure != nil {
		configure(&options)
	}
	surface, err := boardintegration.New(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("board surface: %w", err)
	}
	return &boardWiring{API: surface, Webhook: surface.Webhook(), Surface: surface}, nil
}
