package main

import (
	"fmt"
	"os"
	"strings"
)

// boardDeploymentEnabled reads the deployment-level board switch.
//
// The board is OFF by default (keel-board-v1 design §20: /v1/board/* returns
// 404 board_disabled until a deployment opts in). KEEL_BOARD_ENABLED=1 (or
// true/yes/on) enables the surface; an empty or 0/false/no/off value keeps the
// routes mounted but answers 404 board_disabled. The tenant-level board.enabled
// flag (10.6) narrows this deployment switch further; it cannot enable a
// deployment that never opted in. An unrecognized value fails startup instead
// of guessing.
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
