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
	"github.com/jixoai/keel/internal/store"
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
		authRaw  = fs.String("auth", envOr("KEEL_AUTH", string(authModeRequired)), "authentication mode: required|local-only|off")
		insecure = fs.Bool("insecure", envBool("KEEL_INSECURE"), "allow auth=off (loopback only)")
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
	mode, modeErr := parseAuthMode(*authRaw)
	switch {
	case modeErr != nil:
		checks = append(checks, doctorCheck{"auth", "fail", modeErr.Error()})
	case mode == authModeRequired && tenantID.String() == "00000000-0000-0000-0000-000000000000":
		checks = append(checks, doctorCheck{"auth", "fail", "auth=required but KEEL_SERVE_TENANT is unset"})
	case mode == authModeRequired:
		if _, err := store.SecretKeyFromEnv(os.Getenv); err != nil {
			checks = append(checks, doctorCheck{"auth", "fail", "auth=required but KEEL_SECRET_KEY is not a valid 32-byte key"})
		} else {
			checks = append(checks, doctorCheck{"auth", "ok", "auth=required, account password + signed session cookie"})
		}
	default:
		if err := validateAuthBinding(mode, *insecure, *addr); err != nil {
			checks = append(checks, doctorCheck{"auth", "fail", err.Error()})
		} else {
			checks = append(checks, doctorCheck{"auth", "warn",
				"auth=" + string(mode) + " maps loopback requests to the local-admin super admin; never expose it"})
		}
	}

	checks = append(checks, clientCredentialCheck())

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

// clientCredentialCheck reports which credential `keel bot|team-bot|model|skills`
// will use. Values are never printed: only which variable is configured. The
// retired passphrase is a warning, not a failure — a shell that still exports it
// is not a broken deployment, but it must be told the variable no longer logs
// anyone in.
func clientCredentialCheck() doctorCheck {
	legacy := strings.TrimSpace(os.Getenv("KEEL_SERVE_PASSWORD"))
	token := strings.TrimSpace(os.Getenv("KEEL_AGENT_TOKEN"))
	username := strings.TrimSpace(os.Getenv("KEEL_ACCOUNT_USERNAME"))
	password := os.Getenv("KEEL_ACCOUNT_PASSWORD")
	switch {
	case legacy != "":
		return doctorCheck{"client credential", "warn",
			"KEEL_SERVE_PASSWORD is retired and no longer read; unset it and use KEEL_ACCOUNT_USERNAME + KEEL_ACCOUNT_PASSWORD (or --password-file), or KEEL_AGENT_TOKEN"}
	case token != "":
		return doctorCheck{"client credential", "ok",
			"bot key configured (KEEL_AGENT_TOKEN); machine clients present it as a Bearer credential"}
	case username != "" && password != "":
		return doctorCheck{"client credential", "ok",
			"account credentials configured for keel bot/team-bot/model/skills (POST /v1/auth/login)"}
	case username != "" || password != "":
		return doctorCheck{"client credential", "fail",
			"KEEL_ACCOUNT_USERNAME and KEEL_ACCOUNT_PASSWORD must be set together (or use KEEL_AGENT_TOKEN)"}
	default:
		return doctorCheck{"client credential", "info",
			"no CLI credential configured (auth=local-only works; auth=required needs KEEL_ACCOUNT_USERNAME + KEEL_ACCOUNT_PASSWORD, --password-file, or KEEL_AGENT_TOKEN)"}
	}
}
