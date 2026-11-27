// Operator fixture for the attachment E2E's cross-tenant step.
//
// Creates a second tenant row and a super-admin account in it, so a second
// `keel serve` process (KEEL_SERVE_TENANT=<tenant>) can authenticate a real
// tenant-B session. This is provisioning via the product's own store and auth
// packages, not a second auth path: the password hash is the same Argon2id
// envelope the login service verifies.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

func main() {
	tenantFlag := flag.String("tenant", "", "tenant UUID (required)")
	username := flag.String("username", "", "tenant-B super admin username (required)")
	password := flag.String("password", "", "password (required; supplied through the environment or argv by the harness)")
	display := flag.String("display", "Tenant B Admin", "display name")
	flag.Parse()
	if *tenantFlag == "" || *username == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "seed-tenant: -tenant, -username and -password are required")
		os.Exit(2)
	}
	tenantID, err := uuid.Parse(*tenantFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-tenant: bad tenant uuid: %v\n", err)
		os.Exit(2)
	}
	dsn := os.Getenv("KEEL_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "seed-tenant: KEEL_DSN is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-tenant: open store: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()
	created, err := st.CreateTenantReport(ctx, tenantID, "tenant-b")
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-tenant: create tenant: %v\n", err)
		os.Exit(1)
	}
	hash, err := auth.HashPassword(*password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-tenant: hash password: %v\n", err)
		os.Exit(1)
	}
	existing, err := st.GetAccountByUsername(ctx, tenantID, *username)
	if err == nil {
		fmt.Printf("seed-tenant: tenant=%s account=%s (already present)\n", tenantID, existing.ID)
		return
	}
	account, err := st.CreateAccount(ctx, store.AccountRecord{
		TenantID:     tenantID,
		ID:           uuid.New(),
		Username:     *username,
		DisplayName:  *display,
		Kind:         "human",
		PasswordHash: hash,
		IsSuperAdmin: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-tenant: create account: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("seed-tenant: tenant=%s created=%v account=%s\n", tenantID, created, account.ID)
}
