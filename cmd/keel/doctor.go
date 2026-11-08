package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// `keel doctor` is a read-only diagnostic: build identity, configuration
// pairing (password/tenant), the web artifact, the TUI artifact and, with
// --db, database reachability and schema presence. It never migrates, never
// writes and never starts the worker.
type doctorCheck struct {
	name   string
	status string // ok | warn | fail | info
	detail string
}

func runDoctorCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dsn      = fs.String("dsn", os.Getenv("KEEL_DSN"), "PostgreSQL DSN to test")
		checkDB  = fs.Bool("db", false, "connect to the database and check the schema (read-only)")
		webDir   = fs.String("web-dir", envOr("KEEL_SERVE_WEB_DIR", defaultServeWebDir), "web build directory")
		tuiBin   = fs.String("tui-bin", os.Getenv("KEEL_TUI_BIN"), "NSH TUI binary")
		tenant   = fs.String("tenant", os.Getenv("KEEL_SERVE_TENANT"), "tenant UUID the server acts as")
		password = fs.String("password", os.Getenv("KEEL_SERVE_PASSWORD"), "local passphrase")
		addr     = fs.String("addr", envOr("KEEL_SERVE_ADDR", defaultServeAddr), "planned listen address")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var checks []doctorCheck
	commit, modified := "", false
	if _, revision, dirty := buildInfo(); revision != "" {
		commit, modified = revision, dirty
	}
	build := fmt.Sprintf("keel %s (%s %s/%s)", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if commit != "" {
		suffix := ""
		if modified {
			suffix = " dirty"
		}
		build += fmt.Sprintf(" commit=%s%s", commit, suffix)
	}
	checks = append(checks, doctorCheck{"build", "ok", build})

	tenantID, tenantErr := parseTenant(*tenant)
	switch {
	case tenantErr != nil:
		checks = append(checks, doctorCheck{"tenant", "fail", tenantErr.Error()})
	case tenantID.String() == "00000000-0000-0000-0000-000000000000":
		checks = append(checks, doctorCheck{"tenant", "warn", "KEEL_SERVE_TENANT is unset; /v1 is unusable for a browser session"})
	default:
		checks = append(checks, doctorCheck{"tenant", "ok", tenantID.String()})
	}
	switch {
	case *password == "":
		checks = append(checks, doctorCheck{"auth", "warn", "KEEL_SERVE_PASSWORD is unset; authentication is disabled (loopback only)"})
	case tenantErr == nil && tenantID.String() == "00000000-0000-0000-0000-000000000000":
		checks = append(checks, doctorCheck{"auth", "fail", "authentication is enabled but KEEL_SERVE_TENANT is unset"})
	default:
		checks = append(checks, doctorCheck{"auth", "ok", "local passphrase + HMAC cookie"})
	}

	if summary := webBuildSummary(*webDir); strings.HasPrefix(summary, "missing:") {
		checks = append(checks, doctorCheck{"web build", "warn",
			summary + " (serve answers 404 on non-API paths; build with nsh package --target web)"})
	} else {
		checks = append(checks, doctorCheck{"web build", "ok", summary})
	}
	if path, _ := resolveTUI(*tuiBin, *webDir); path != "" {
		checks = append(checks, doctorCheck{"tui artifact", "ok", path})
	} else {
		checks = append(checks, doctorCheck{"tui artifact", "info", "not built (keel tui explains how to build it)"})
	}
	checks = append(checks, doctorCheck{"listen", "info", *addr})

	switch {
	case *dsn == "":
		checks = append(checks, doctorCheck{"database", "fail", "KEEL_DSN is unset"})
	case !*checkDB:
		checks = append(checks, doctorCheck{"database", "info", "DSN configured; pass --db to test connectivity and schema"})
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pool, err := pgxpool.New(ctx, *dsn)
		if err != nil {
			cancel()
			checks = append(checks, doctorCheck{"database", "fail", "connect: " + err.Error()})
			break
		}
		var hasSessions bool
		err = pool.QueryRow(ctx, `SELECT to_regclass('public.sessions') IS NOT NULL`).Scan(&hasSessions)
		pool.Close()
		cancel()
		switch {
		case err != nil:
			checks = append(checks, doctorCheck{"database", "fail", "query: " + err.Error()})
		case !hasSessions:
			checks = append(checks, doctorCheck{"schema", "fail", "sessions table is missing: run keel serve once to migrate"})
		default:
			checks = append(checks, doctorCheck{"database", "ok", "reachable, schema present"})
		}
	}

	width := 0
	for _, check := range checks {
		if len(check.name) > width {
			width = len(check.name)
		}
	}
	fmt.Fprintln(stdout, "keel doctor")
	fails, warns := 0, 0
	for _, check := range checks {
		switch check.status {
		case "fail":
			fails++
		case "warn":
			warns++
		}
		fmt.Fprintf(stdout, "  [%s] %-*s  %s\n", check.status, width, check.name, check.detail)
	}
	fmt.Fprintf(stdout, "\nsummary: %d checks, %d warning(s), %d failure(s)\n", len(checks), warns, fails)
	if fails > 0 {
		return 1
	}
	return 0
}
