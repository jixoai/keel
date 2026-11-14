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
	case "key":
		return runAdminKey(args[1:], stdout, stderr)
	case "quota":
		return runAdminQuota(args[1:], stdout, stderr)
	case "usage":
		return runAdminUsage(args[1:], stdout, stderr)
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
  keel admin tenant create --name <name> [--tenant <uuid>] [--role tenant|admin] [--no-key]
  keel admin tenant list
  keel admin tenant show <uuid>
  keel admin key list --tenant <uuid>
  keel admin key create --tenant <uuid> [--role tenant|admin] [--label <text>]
  keel admin key revoke --tenant <uuid> --key <uuid>
  keel admin quota set --tenant <uuid> [--streams n] [--bytes-per-sec n] [--storage-bytes n]
  keel admin quota show --tenant <uuid>

The plaintext API key is printed exactly once, by "tenant create" / "key create".
It is stored only as a SHA-256 hash; there is no command that shows it again.

Environment:
  KEEL_DSN                 PostgreSQL DSN (required)
`)
}

// adminStore opens the store for one command. It is closed before the process
// exits; schema migration runs (idempotent) so the first command on a fresh
// database works.
func adminStore(ctx context.Context) (*store.Store, error) {
	dsn := strings.TrimSpace(os.Getenv("KEEL_DSN"))
	if dsn == "" {
		return nil, errors.New("KEEL_DSN (or --dsn) is required")
	}
	return store.Open(ctx, store.Config{
		DSN:        dsn,
		DisableRLS: strings.EqualFold(strings.TrimSpace(os.Getenv("KEEL_RLS")), "off"),
	})
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
		role   = fs.String("role", auth.RoleTenant, "role of the minted key: tenant|admin")
		label  = fs.String("label", "provisioned", "label of the minted key")
		noKey  = fs.Bool("no-key", false, "create the tenant without an API key")
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
	if !auth.ValidRole(*role) {
		fmt.Fprintf(stderr, "keel admin tenant create: --role %q (want tenant|admin)\n", *role)
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

	created, err := st.CreateTenantReport(ctx, tenantID, tenantName)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant:  %s\n", tenantID)
	fmt.Fprintf(stdout, "name:    %s\n", tenantName)
	if created {
		fmt.Fprintln(stdout, "created: yes")
	} else {
		fmt.Fprintln(stdout, "created: no (tenant already existed; name unchanged)")
	}
	if *noKey {
		return 0
	}
	if code := mintAndPrintKey(ctx, st, tenantID, *role, *label, stdout, stderr); code != 0 {
		return code
	}
	return 0
}

// mintAndPrintKey mints a key, persists its hash and prints the plaintext once.
// The plaintext never touches the store, a log or an error path.
func mintAndPrintKey(ctx context.Context, st *store.Store, tenantID uuid.UUID, role, label string, stdout, stderr io.Writer) int {
	minted, err := auth.MintKey(role)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	keyID := uuid.New()
	if _, err := st.CreateAPIKey(ctx, tenantID, keyID, minted.Hash, minted.Prefix, minted.Role, strings.TrimSpace(label)); err != nil {
		fmt.Fprintf(stderr, "keel admin: mint api key: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "key id:  %s\n", keyID)
	fmt.Fprintf(stdout, "prefix:  %s\n", minted.Prefix)
	fmt.Fprintf(stdout, "role:    %s\n", minted.Role)
	// The one and only appearance of the secret. Everything around it is
	// deliberate: it is on its own line, labelled, and the notice follows.
	fmt.Fprintf(stdout, "\nAPI KEY (shown once, store it now):\n%s\n\n", minted.Plaintext)
	fmt.Fprintln(stdout, "This key cannot be shown again. Mint a new one if it is lost.")
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
	keys, err := st.ListAPIKeys(ctx, tenantID)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	if len(keys) == 0 {
		fmt.Fprintln(stdout, "keys:    none")
		return 0
	}
	fmt.Fprintln(stdout, "keys:")
	tw := tabwriter.NewWriter(stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  KEY\tPREFIX\tROLE\tLABEL\tCREATED\tREVOKED")
	for _, key := range keys {
		revoked := "-"
		if key.RevokedAt != nil {
			revoked = key.RevokedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", key.KeyID, key.Prefix, key.Role,
			key.Label, key.CreatedAt.UTC().Format(time.RFC3339), revoked)
	}
	_ = tw.Flush()
	return 0
}

func runAdminKey(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		adminUsage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runAdminKeyList(args[1:], stdout, stderr)
	case "create":
		return runAdminKeyCreate(args[1:], stdout, stderr)
	case "revoke":
		return runAdminKeyRevoke(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "keel admin key: unknown command %q\n", args[0])
		return 2
	}
}

func runAdminKeyList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel admin key list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenant := fs.String("tenant", "", "tenant UUID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tenantID, ok := parseAdminTenant(*tenant, "keel admin key list", stderr)
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
	keys, err := st.ListAPIKeys(ctx, tenantID)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	if len(keys) == 0 {
		fmt.Fprintln(stdout, "no keys")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tPREFIX\tROLE\tLABEL\tCREATED\tREVOKED")
	for _, key := range keys {
		revoked := "-"
		if key.RevokedAt != nil {
			revoked = key.RevokedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", key.KeyID, key.Prefix, key.Role,
			key.Label, key.CreatedAt.UTC().Format(time.RFC3339), revoked)
	}
	_ = tw.Flush()
	return 0
}

func runAdminKeyCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel admin key create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		tenant = fs.String("tenant", "", "tenant UUID (required)")
		role   = fs.String("role", auth.RoleTenant, "role: tenant|admin")
		label  = fs.String("label", "", "label for the key")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tenantID, ok := parseAdminTenant(*tenant, "keel admin key create", stderr)
	if !ok {
		return 2
	}
	if !auth.ValidRole(*role) {
		fmt.Fprintf(stderr, "keel admin key create: --role %q (want tenant|admin)\n", *role)
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
		fmt.Fprintf(stderr, "keel admin: tenant %s not found\n", tenantID)
		return 1
	}
	return mintAndPrintKey(ctx, st, tenantID, *role, *label, stdout, stderr)
}

func runAdminKeyRevoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel admin key revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		tenant = fs.String("tenant", "", "tenant UUID (required)")
		key    = fs.String("key", "", "key UUID (required)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tenantID, ok := parseAdminTenant(*tenant, "keel admin key revoke", stderr)
	if !ok {
		return 2
	}
	keyID, err := uuid.Parse(strings.TrimSpace(*key))
	if err != nil {
		fmt.Fprintln(stderr, "keel admin key revoke: --key must be a UUID")
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
	found, err := st.RevokeAPIKey(ctx, tenantID, keyID)
	if err != nil {
		fmt.Fprintf(stderr, "keel admin: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintf(stderr, "keel admin: key %s not found for tenant %s\n", keyID, tenantID)
		return 1
	}
	fmt.Fprintf(stdout, "revoked: %s\n", keyID)
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
