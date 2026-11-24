// `keel admin ...`: operator provisioning of tenants and API keys.
//
// This is the CLI half of the tenancy surface (change keel-ga §2). It talks to
// PostgreSQL directly rather than to a running `keel serve`, for one reason: the
// bootstrap case. Minting the first key for a deployment that has no key cannot
// require a key — the operator already holds the DSN (KEEL_DSN), which is a
// stronger credential than any key this command prints.
//
// # The plaintext key appears exactly once
//
// `keel admin tenant create` prints the key to stdout and nothing else in the
// process keeps it: it is not written to the database (only its SHA-256 hash and
// a display prefix are), not logged, and there is no "show" subcommand. Losing it
// means minting a new one. The notice the command prints says so.
//
// # What it will not do
//
// It never prints a DSN, a password or an existing key; it never rewrites an
// existing tenant's name (create is create-or-report); and it refuses a role it
// does not know.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

func runAdminCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		adminUsage(stderr)
		return 2
	}
	switch args[0] {
	case "tenant":
		return runAdminTenant(args[1:], stdout, stderr)
	case "quota":
		return runAdminQuota(args[1:], stdout, stderr)
	case "usage":
		return runAdminUsage(args[1:], stdout, stderr)
	case "bootstrap":
		return runAdminBootstrap(args[1:], stdout, stderr)
	case "reset":
		return runAdminReset(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		adminUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "keel admin: unknown command %q\n\n", args[0])
		adminUsage(stderr)
		return 2
	}
}

func adminUsage(w io.Writer) {
	fmt.Fprint(w, `keel admin — operator provisioning of tenants, keys and quotas

Usage:
  keel admin bootstrap [--tenant <uuid>] [--name <text>] [--ttl 30m] [--token-file <path>]
  keel admin reset --username <name> [--tenant <uuid>]
  keel admin tenant create --name <name> [--tenant <uuid>]
  keel admin tenant list
  keel admin tenant show <uuid>
  keel admin quota set --tenant <uuid> [--streams n] [--bytes-per-sec n] [--storage-bytes n]
  keel admin quota show --tenant <uuid>

"bootstrap" mints a one-time setup token (printed to stderr once, or written to a
0600 file) for the first super admin. "reset" sets a temporary password, revokes
every session and forces a change at next login; both are local CLI operations.

The plaintext tenant API key is printed exactly once, by "tenant create" /
"key create". It is stored only as a SHA-256 hash; there is no command that shows
it again.

Environment:
  KEEL_DSN                 PostgreSQL DSN (required)
  KEEL_SERVE_TENANT        default tenant UUID for bootstrap/reset
`)
}

// adminStore opens the store for one command. It is closed before the process
// exits; schema migration runs (idempotent) so the first command on a fresh
// database works.
func adminStore(ctx context.Context) (*store.Store, error) {
	dsn := strings.TrimSpace(os.Getenv("KEEL_DSN"))
	if dsn == "" {
		return nil, errors.New("KEEL_DSN (or --dsn) is required; for a local cluster without Docker run keel db start")
	}
	return store.Open(ctx, store.Config{
		DSN:        dsn,
		DisableRLS: strings.EqualFold(strings.TrimSpace(os.Getenv("KEEL_RLS")), "off"),
	})
}

// runAdminBootstrap mints the one-time setup token for the first super admin.
// The plaintext goes to stderr (or a 0600 file), never to the database: only
// its SHA-256 hash is recorded in auth_events.
func runAdminBootstrap(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel admin bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		tenant    = fs.String("tenant", "", "tenant UUID (default KEEL_SERVE_TENANT)")
		name      = fs.String("name", "keel", "tenant name when the tenant row does not exist")
		ttl       = fs.Duration("ttl", store.BootstrapTokenTTL, "setup token lifetime")
		tokenFile = fs.String("token-file", "", "write the token to this file with mode 0600")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *ttl <= 0 || *ttl > 24*time.Hour {
		fmt.Fprintln(stderr, "keel admin bootstrap: --ttl must be between 1s and 24h")
		return 2
	}
	tenantID, ok := resolveAdminTenant(*tenant, "keel admin bootstrap", stderr)
	if !ok {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := adminStore(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	defer st.Close()

	created, err := st.CreateTenantReport(ctx, tenantID, strings.TrimSpace(*name))
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	hasSuper, err := st.HasSuperAdmin(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	if hasSuper {
		fmt.Fprintln(stderr, "keel admin bootstrap: a super admin already exists; use `keel admin reset --username ...` for recovery")
		return 1
	}
	token, hash, err := auth.MintSetupToken()
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	expiresAt := time.Now().UTC().Add(*ttl)
	if _, err := st.CreateBootstrapToken(ctx, tenantID, hash, expiresAt, "local-cli"); err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant:  %s\n", tenantID)
	if created {
		fmt.Fprintf(stdout, "created: tenant %q\n", strings.TrimSpace(*name))
	}
	fmt.Fprintf(stdout, "expires: %s\n", expiresAt.Format(time.RFC3339))
	// The one and only appearance of the setup token. stderr is deliberate: a
	// pipeline that captures stdout for machine output does not swallow it.
	fmt.Fprintf(stderr, "\nSETUP TOKEN (shown once, expires %s):\n%s\n\n", expiresAt.Format(time.RFC3339), token)
	fmt.Fprintln(stderr, "Open the login page and submit this token with the username and password for the first super admin.")
	if strings.TrimSpace(*tokenFile) != "" {
		if err := os.WriteFile(*tokenFile, []byte(token+"\n"), 0o600); err != nil {
			fmt.Fprintf(stderr, "keel admin: write token file: %v\n", err)
			return 1
		}
		if err := os.Chmod(*tokenFile, 0o600); err != nil {
			fmt.Fprintf(stderr, "keel admin: chmod token file: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "token file: %s (mode 0600)\n", *tokenFile)
	}
	return 0
}

// runAdminReset is the local recovery path: a temporary password, every session
// revoked and a forced change at next login. It never runs over HTTP.
func runAdminReset(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel admin reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		tenant   = fs.String("tenant", "", "tenant UUID (default KEEL_SERVE_TENANT)")
		username = fs.String("username", "", "account username (required)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*username) == "" {
		fmt.Fprintln(stderr, "keel admin reset: --username is required")
		return 2
	}
	tenantID, ok := resolveAdminTenant(*tenant, "keel admin reset", stderr)
	if !ok {
		return 2
	}
	tempPassword, err := auth.MintTemporaryPassword()
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	hash, err := auth.HashPassword(tempPassword)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := adminStore(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	defer st.Close()
	account, err := st.ResetAccountPassword(ctx, tenantID, strings.TrimSpace(*username), hash, "local-cli")
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: reset %q: %v\n", strings.TrimSpace(*username), err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant:  %s\n", tenantID)
	fmt.Fprintf(stdout, "account: %s (%s)\n", account.Username, account.ID)
	fmt.Fprintf(stdout, "super admin: %v\n", account.IsSuperAdmin)
	fmt.Fprintln(stdout, "sessions: all revoked")
	fmt.Fprintf(stdout, "\nTEMPORARY PASSWORD (shown once, change at next login):\n%s\n\n", tempPassword)
	return 0
}

func resolveAdminTenant(raw, command string, stderr io.Writer) (uuid.UUID, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("KEEL_SERVE_TENANT"))
	}
	if raw == "" {
		fmt.Fprintf(stderr, "%s: --tenant (or KEEL_SERVE_TENANT) is required\n", command)
		return uuid.Nil, false
	}
	tenantID, err := uuid.Parse(raw)
	if err != nil {
		fmt.Fprintf(stderr, "%s: tenant id must be a UUID\n", command)
		return uuid.Nil, false
	}
	return tenantID, true
}

func runAdminTenant(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		adminUsage(stderr)
		return 2
	}
	switch args[0] {
	case "create":
		return runAdminTenantCreate(args[1:], stdout, stderr)
	case "list":
		return runAdminTenantList(args[1:], stdout, stderr)
	case "show":
		return runAdminTenantShow(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "keel admin tenant: unknown command %q\n", args[0])
		return 2
	}
}

func runAdminTenantCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel admin tenant create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		name   = fs.String("name", "", "tenant display name (required)")
		tenant = fs.String("tenant", "", "tenant UUID (minted when omitted)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tenantName := strings.TrimSpace(*name)
	if tenantName == "" {
		fmt.Fprintln(stderr, "keel admin tenant create: --name is required")
		return 2
	}
	tenantID := uuid.New()
	if raw := strings.TrimSpace(*tenant); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			fmt.Fprintln(stderr, "keel admin tenant create: --tenant must be a UUID")
			return 2
		}
		tenantID = parsed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := adminStore(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	defer st.Close()

	created, err := st.CreateTenantReport(ctx, tenantID, tenantName)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	if err := accesswire.EnsureBuiltinGroups(ctx, st, tenantID); err != nil {
		fmt.Fprintf(stderr, "keel admin: seed built-in permission groups: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant:  %s\n", tenantID)
	fmt.Fprintf(stdout, "name:    %s\n", tenantName)
	if created {
		fmt.Fprintln(stdout, "created: yes")
	} else {
		fmt.Fprintln(stdout, "created: no (tenant already existed; name unchanged)")
	}
	return 0
}

func runAdminTenantList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel admin tenant list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := adminStore(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	defer st.Close()
	tenants, err := st.ListTenants(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	if len(tenants) == 0 {
		fmt.Fprintln(stdout, "no tenants")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TENANT\tNAME\tSTREAMS\tBYTES/S\tSTORAGE\tCREATED")
	for _, tenant := range tenants {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\n", tenant.TenantID, tenant.Name,
			tenant.Quota.Streams, tenant.Quota.BytesPerSec, tenant.Quota.StorageBytes,
			tenant.CreatedAt.UTC().Format(time.RFC3339))
	}
	_ = tw.Flush()
	return 0
}

func runAdminTenantShow(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "keel admin tenant show: exactly one tenant UUID is required")
		return 2
	}
	tenantID, err := uuid.Parse(strings.TrimSpace(args[0]))
	if err != nil {
		fmt.Fprintln(stderr, "keel admin tenant show: tenant id must be a UUID")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := adminStore(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	defer st.Close()
	tenant, found, err := st.GetTenant(ctx, tenantID)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintf(stderr, "keel admin: tenant %s not found\n", tenantID)
		return 1
	}
	fmt.Fprintf(stdout, "tenant:  %s\n", tenant.TenantID)
	fmt.Fprintf(stdout, "name:    %s\n", tenant.Name)
	fmt.Fprintf(stdout, "created: %s\n", tenant.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(stdout, "quota:   streams=%d bytes/sec=%d storage=%d\n",
		tenant.Quota.Streams, tenant.Quota.BytesPerSec, tenant.Quota.StorageBytes)
	return 0
}

// parseInt64Flag parses a decimal flag value.
func parseInt64Flag(raw string) (int64, error) {
	var value int64
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &value); err != nil {
		return 0, fmt.Errorf("not an integer: %q", raw)
	}
	if value < 0 {
		return 0, errors.New("must be >= 0")
	}
	return value, nil
}

func parseAdminTenant(raw, command string, stderr io.Writer) (uuid.UUID, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		fmt.Fprintf(stderr, "%s: --tenant is required\n", command)
		return uuid.Nil, false
	}
	tenantID, err := uuid.Parse(raw)
	if err != nil {
		fmt.Fprintf(stderr, "%s: --tenant must be a UUID\n", command)
		return uuid.Nil, false
	}
	return tenantID, true
}

func runAdminQuota(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		adminUsage(stderr)
		return 2
	}
	switch args[0] {
	case "set":
		return runAdminQuotaSet(args[1:], stdout, stderr)
	case "show":
		return runAdminQuotaShow(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "keel admin quota: unknown command %q\n", args[0])
		return 2
	}
}

func runAdminQuotaSet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel admin quota set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		tenant    = fs.String("tenant", "", "tenant UUID (required)")
		streams   = fs.String("streams", "", "max concurrent streams (0 = unlimited)")
		bytesRate = fs.String("bytes-per-sec", "", "outbound bytes per second (0 = unlimited)")
		storage   = fs.String("storage-bytes", "", "stored-byte ceiling (0 = unlimited)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tenantID, ok := parseAdminTenant(*tenant, "keel admin quota set", stderr)
	if !ok {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := adminStore(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	defer st.Close()
	current, found, err := st.GetTenant(ctx, tenantID)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintf(stderr, "keel admin quota set: tenant %s not found\n", tenantID)
		return 1
	}
	streamsValue, bytesValue, storageValue := current.Quota.Streams, current.Quota.BytesPerSec, current.Quota.StorageBytes
	parseInto := func(raw string, dst *int64) bool {
		if strings.TrimSpace(raw) == "" {
			return true
		}
		parsed, err := parseInt64Flag(raw)
		if err != nil {
			fmt.Fprintf(stderr, "keel admin quota set: %v\n", err)
			return false
		}
		*dst = parsed
		return true
	}
	var s64, b64, st64 = int64(streamsValue), bytesValue, storageValue
	if !parseInto(*streams, &s64) || !parseInto(*bytesRate, &b64) || !parseInto(*storage, &st64) {
		return 2
	}
	if err := st.SetTenantQuota(ctx, tenantID, int(s64), b64, st64); err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant:  %s\nstreams: %d\nbytes/sec: %d\nstorage: %d\n",
		tenantID, s64, b64, st64)
	return 0
}

func runAdminQuotaShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel admin quota show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenant := fs.String("tenant", "", "tenant UUID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tenantID, ok := parseAdminTenant(*tenant, "keel admin quota show", stderr)
	if !ok {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := adminStore(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	defer st.Close()
	record, found, err := st.GetTenant(ctx, tenantID)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintf(stderr, "keel admin quota show: tenant %s not found\n", tenantID)
		return 1
	}
	stored, err := st.TenantStoredBytes(ctx, tenantID)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant:     %s\n", tenantID)
	fmt.Fprintf(stdout, "streams:    %d\n", record.Quota.Streams)
	fmt.Fprintf(stdout, "bytes/sec:  %d\n", record.Quota.BytesPerSec)
	fmt.Fprintf(stdout, "storage:    %d (limit, 0 = unlimited)\n", record.Quota.StorageBytes)
	fmt.Fprintf(stdout, "stored now: %d\n", stored)
	return 0
}

func runAdminUsage(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		adminUsage(stderr)
		return 2
	}
	switch args[0] {
	case "show":
		return runAdminUsageShow(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "keel admin usage: unknown command %q\n", args[0])
		return 2
	}
}

// runAdminUsageShow prints a tenant's objective usage: token counts from the
// usage ledger and byte counts from tenant_usage. There is no money in this
// output by design — prices live in contracts.
func runAdminUsageShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel admin usage show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenant := fs.String("tenant", "", "tenant UUID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tenantID, ok := parseAdminTenant(*tenant, "keel admin usage show", stderr)
	if !ok {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := adminStore(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	defer st.Close()
	if exists, err := st.TenantExists(ctx, tenantID); err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	} else if !exists {
		fmt.Fprintf(stderr, "keel admin usage show: tenant %s not found\n", tenantID)
		return 1
	}
	tokens, err := st.SumUsage(ctx, store.UsageQuery{TenantID: tenantID})
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	bytes, err := st.SumByteUsage(ctx, store.ByteUsageQuery{TenantID: tenantID})
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant: %s\n", tenantID)
	fmt.Fprintln(stdout, "\ntokens (by session/provider/model):")
	if len(tokens) == 0 {
		fmt.Fprintln(stdout, "  none")
	}
	var in, out, entries int64
	for _, group := range tokens {
		in += group.InputTokens
		out += group.OutputTokens
		entries += group.Entries
		fmt.Fprintf(stdout, "  %s %s/%s: entries=%d in=%d out=%d cacheRead=%d cacheWrite=%d reasoning=%d unaccounted=%d\n",
			group.SessionID, group.Provider, group.Model, group.Entries,
			group.InputTokens, group.OutputTokens, group.CacheReadTokens,
			group.CacheWriteTokens, group.ReasoningTokens, group.UnaccountedEntries)
	}
	fmt.Fprintf(stdout, "  total: entries=%d input=%d output=%d\n", entries, in, out)
	fmt.Fprintln(stdout, "\nbytes:")
	if len(bytes) == 0 {
		fmt.Fprintln(stdout, "  none")
	}
	for _, group := range bytes {
		fmt.Fprintf(stdout, "  %s: entries=%d bytes=%d\n", group.Kind, group.Entries, group.Bytes)
	}
	return 0
}
