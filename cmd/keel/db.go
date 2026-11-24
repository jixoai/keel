// `keel db ...`: a local, disposable PostgreSQL 18 cluster for development.
//
// This is deliberately part of the product binary rather than a shell script:
// the quickstart must work for anyone who has the binary (or the checkout), not
// only for people sitting in a POSIX shell inside the repository. It still uses
// the PostgreSQL server tools (`initdb`, `pg_ctl`, ...): keel does not embed a
// database engine.
//
// The cluster is single-user and local: it listens on 127.0.0.1 only, gets a
// random password in a 0600 file, and refuses to reset a directory it cannot
// recognise as its own (no PG_VERSION).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	defaultDevDBPort     = 13741
	defaultDevDBDatabase = "keel"
	defaultDevDBUser     = "keel"
	devDBReadyTimeout    = 10 * time.Second
)

type devDBMeta struct {
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
}

type devDB struct {
	dir      string
	port     int
	database string
	user     string
}

func (d devDB) passFile() string { return d.dir + ".pass" }
func (d devDB) logFile() string  { return d.dir + ".log" }
func (d devDB) metaFile() string { return d.dir + ".meta" }

func runDBCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		dbUsage(stderr)
		return 2
	}
	switch args[0] {
	case "start":
		return runDBStart(args[1:], stdout, stderr)
	case "stop":
		return runDBStop(args[1:], stdout, stderr)
	case "status":
		return runDBStatus(args[1:], stdout, stderr)
	case "dsn":
		return runDBDSN(args[1:], stdout, stderr)
	case "env":
		return runDBEnv(args[1:], stdout, stderr)
	case "reset":
		return runDBReset(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		dbUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "keel db: unknown command %q\n\n", args[0])
		dbUsage(stderr)
		return 2
	}
}

func dbUsage(w io.Writer) {
	fmt.Fprint(w, `keel db — manage a local development PostgreSQL 18 cluster (no Docker)

Usage:
  keel db start [--dir <path>] [--port 13741] [--database keel] [--user keel]
  keel db stop [--dir <path>]
  keel db status [--dir <path>]
  keel db dsn [--dir <path>]
  keel db env [--dir <path>]          print `+"`export KEEL_DSN='...'`"+`
  keel db reset --yes [--dir <path>]  stop and delete this cluster

The data directory defaults to $KEEL_HOME/dev-pg, then ~/.keel/dev-pg. The
cluster listens on 127.0.0.1 only, uses a random password stored in a 0600
file, and requires PostgreSQL 18 or newer (initdb/pg_ctl on PATH or via
pg_config --bindir). `+"`reset`"+` refuses directories it did not create.

keel serve and keel admin use this cluster automatically when KEEL_DSN is
unset; run these commands only for explicit control (status/stop/reset/custom
port or directory) or set KEEL_NO_AUTO_DB=1 to disable auto-init.
`)
}

func defaultDevDBDir() string {
	home := os.Getenv("KEEL_HOME")
	if home == "" {
		if dir, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(dir, ".keel")
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "dev-pg")
}

type devDBFlags struct {
	dir      string
	port     int
	database string
	user     string
}

func (f *devDBFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.dir, "dir", defaultDevDBDir(), "cluster data directory")
	fs.IntVar(&f.port, "port", defaultDevDBPort, "loopback port")
	fs.StringVar(&f.database, "database", defaultDevDBDatabase, "database name")
	fs.StringVar(&f.user, "user", defaultDevDBUser, "superuser name")
}

func parseDevDBFlags(name string, args []string, stderr io.Writer) (devDBFlags, map[string]bool, bool) {
	f := devDBFlags{}
	fs := flag.NewFlagSet("keel db "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	f.bind(fs)
	if err := fs.Parse(args); err != nil {
		return f, nil, false
	}
	if strings.TrimSpace(f.dir) == "" {
		fmt.Fprintln(stderr, "keel db: cannot resolve a data directory; pass --dir")
		return f, nil, false
	}
	if f.port < 1 || f.port > 65535 {
		fmt.Fprintf(stderr, "keel db: invalid port %d\n", f.port)
		return f, nil, false
	}
	if strings.TrimSpace(f.database) == "" || strings.TrimSpace(f.user) == "" {
		fmt.Fprintln(stderr, "keel db: --database and --user must not be empty")
		return f, nil, false
	}
	explicit := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { explicit[fl.Name] = true })
	return f, explicit, true
}

func (f devDBFlags) db() devDB {
	return devDB{dir: f.dir, port: f.port, database: f.database, user: f.user}
}

func (d devDB) initialized() bool {
	_, err := os.Stat(filepath.Join(d.dir, "PG_VERSION"))
	return err == nil
}

func (d devDB) loadMeta() (devDBMeta, error) {
	b, err := os.ReadFile(d.metaFile())
	if err != nil {
		return devDBMeta{}, err
	}
	var m devDBMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return devDBMeta{}, fmt.Errorf("corrupt metadata file %s: %w", d.metaFile(), err)
	}
	if m.Port == 0 || m.Database == "" || m.User == "" {
		return devDBMeta{}, fmt.Errorf("incomplete metadata file %s", d.metaFile())
	}
	return m, nil
}

func (d devDB) saveMeta(m devDBMeta) error {
	if err := os.MkdirAll(filepath.Dir(d.metaFile()), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(d.metaFile(), append(b, '\n'), 0o600)
}

func runDBStart(args []string, stdout, stderr io.Writer) int {
	f, explicit, ok := parseDevDBFlags("start", args, stderr)
	if !ok {
		return 2
	}
	d := f.db()

	// Metadata is authoritative once the cluster exists; flags may only repeat
	// it. Silently switching ports or roles after init would point `dsn` at a
	// cluster that does not exist.
	if d.initialized() {
		meta, err := d.loadMeta()
		if err == nil {
			if explicit["port"] && d.port != meta.Port {
				fmt.Fprintf(stderr, "keel db: cluster was initialized on port %d, not %d (pass --port %d or run reset --yes)\n", meta.Port, d.port, meta.Port)
				return 1
			}
			if explicit["database"] && d.database != meta.Database {
				fmt.Fprintf(stderr, "keel db: cluster was initialized with database %q, not %q\n", meta.Database, d.database)
				return 1
			}
			if explicit["user"] && d.user != meta.User {
				fmt.Fprintf(stderr, "keel db: cluster was initialized with user %q, not %q\n", meta.User, d.user)
				return 1
			}
		} else if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "keel db: %s is initialized but has no metadata file %s; run reset --yes and start again\n", d.dir, d.metaFile())
			return 1
		} else {
			fmt.Fprintf(stderr, "keel db: %v\n", err)
			return 1
		}
	}

	dsn, err := d.ensure(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "keel db: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "ready: %s\n", dsn)
	return 0
}

// ensure makes the cluster usable and returns its DSN. It is shared by
// `keel db start` and by the implicit auto-start in `keel serve`/`keel admin`:
// a stopped cluster is started, a missing cluster is initialized, and the
// database is created if needed.
func (d devDB) ensure(stderr io.Writer) (string, error) {
	if d.initialized() {
		meta, err := d.loadMeta()
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s is initialized but has no metadata file %s; run reset --yes and start again", d.dir, d.metaFile())
		}
		if err != nil {
			return "", err
		}
		d.port, d.database, d.user = meta.Port, meta.Database, meta.User
	} else {
		if err := initDevDBCluster(d, stderr); err != nil {
			return "", err
		}
		if err := d.saveMeta(devDBMeta{Port: d.port, Database: d.database, User: d.user}); err != nil {
			return "", err
		}
	}
	pgCtl, err := pgTool("pg_ctl")
	if err != nil {
		return "", err
	}
	if !devDBRunning(pgCtl, d.dir) {
		if err := portFree(d.port); err != nil {
			return "", err
		}
		if err := startDevDBCluster(d, pgCtl); err != nil {
			return "", err
		}
	}
	if err := waitDevDBReady(d); err != nil {
		return "", err
	}
	if err := ensureDevDBDatabase(d); err != nil {
		return "", err
	}
	return d.dsn()
}

// ensureAutoDevDB is the no-configuration path used when KEEL_DSN is unset.
// It returns the DSN and the data directory so callers can say where the data
// lives (never the password).
func ensureAutoDevDB(stderr io.Writer) (dsn, dir string, err error) {
	dir = defaultDevDBDir()
	if dir == "" {
		return "", "", errors.New("cannot resolve a data directory; set KEEL_HOME or KEEL_DSN")
	}
	d := devDB{dir: dir, port: defaultDevDBPort, database: defaultDevDBDatabase, user: defaultDevDBUser}
	dsn, err = d.ensure(stderr)
	return dsn, dir, err
}

func runDBStop(args []string, stdout, stderr io.Writer) int {
	f, _, ok := parseDevDBFlags("stop", args, stderr)
	if !ok {
		return 2
	}
	d := f.db()
	if !d.initialized() {
		fmt.Fprintf(stdout, "not initialized (data: %s)\n", d.dir)
		return 0
	}
	pgCtl, err := pgTool("pg_ctl")
	if err != nil {
		fmt.Fprintf(stderr, "keel db: %v\n", err)
		return 1
	}
	if !devDBRunning(pgCtl, d.dir) {
		fmt.Fprintln(stdout, "not running")
		return 0
	}
	if err := runPGCommand(pgCtl, "-D", d.dir, "-m", "fast", "-w", "stop"); err != nil {
		fmt.Fprintf(stderr, "keel db: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "stopped (data kept in %s)\n", d.dir)
	return 0
}

func runDBStatus(args []string, stdout, stderr io.Writer) int {
	f, _, ok := parseDevDBFlags("status", args, stderr)
	if !ok {
		return 2
	}
	d := f.db()
	if !d.initialized() {
		fmt.Fprintf(stdout, "not initialized (data: %s)\n", d.dir)
		return 0
	}
	meta, err := d.loadMeta()
	if err != nil {
		fmt.Fprintf(stderr, "keel db: %v\n", err)
		return 1
	}
	d.port, d.database, d.user = meta.Port, meta.Database, meta.User
	pgCtl, err := pgTool("pg_ctl")
	if err != nil {
		fmt.Fprintf(stderr, "keel db: %v\n", err)
		return 1
	}
	if !devDBRunning(pgCtl, d.dir) {
		fmt.Fprintf(stdout, "not running (data: %s)\n", d.dir)
		return 0
	}
	fmt.Fprintf(stdout, "running on 127.0.0.1:%d (data: %s)\n", d.port, d.dir)
	if dsn, err := d.dsn(); err == nil {
		fmt.Fprintf(stdout, "dsn: %s\n", dsn)
	}
	return 0
}

func runDBDSN(args []string, stdout, stderr io.Writer) int {
	f, _, ok := parseDevDBFlags("dsn", args, stderr)
	if !ok {
		return 2
	}
	d := f.db()
	meta, err := d.loadMeta()
	if err != nil {
		fmt.Fprintf(stderr, "keel db: no cluster metadata at %s (run start first)\n", d.metaFile())
		return 1
	}
	d.port, d.database, d.user = meta.Port, meta.Database, meta.User
	dsn, err := d.dsn()
	if err != nil {
		fmt.Fprintf(stderr, "keel db: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, dsn)
	return 0
}

func runDBEnv(args []string, stdout, stderr io.Writer) int {
	f, _, ok := parseDevDBFlags("env", args, stderr)
	if !ok {
		return 2
	}
	d := f.db()
	meta, err := d.loadMeta()
	if err != nil {
		fmt.Fprintf(stderr, "keel db: no cluster metadata at %s (run start first)\n", d.metaFile())
		return 1
	}
	d.port, d.database, d.user = meta.Port, meta.Database, meta.User
	dsn, err := d.dsn()
	if err != nil {
		fmt.Fprintf(stderr, "keel db: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "export KEEL_DSN=%s\n", shellQuote(dsn))
	return 0
}

func runDBReset(args []string, stdout, stderr io.Writer) int {
	f := devDBFlags{}
	fs := flag.NewFlagSet("keel db reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f.bind(fs)
	yes := fs.Bool("yes", false, "confirm deleting the cluster")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(f.dir) == "" {
		fmt.Fprintln(stderr, "keel db: cannot resolve a data directory; pass --dir")
		return 2
	}
	d := f.db()
	if !*yes {
		fmt.Fprintf(stderr, "keel db: reset deletes %s; pass --yes to confirm\n", d.dir)
		return 2
	}
	info, err := os.Stat(d.dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		_ = os.Remove(d.passFile())
		_ = os.Remove(d.logFile())
		_ = os.Remove(d.metaFile())
		fmt.Fprintln(stdout, "not initialized")
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "keel db: %v\n", err)
		return 1
	case !info.IsDir():
		fmt.Fprintf(stderr, "keel db: %s is not a directory; refusing to remove it\n", d.dir)
		return 1
	}
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		fmt.Fprintf(stderr, "keel db: %v\n", err)
		return 1
	}
	if !d.initialized() {
		if len(entries) != 0 {
			fmt.Fprintf(stderr, "keel db: %s has no PG_VERSION; refusing to remove a directory this command did not create\n", d.dir)
			return 1
		}
		_ = os.Remove(d.dir)
		_ = os.Remove(d.passFile())
		_ = os.Remove(d.logFile())
		_ = os.Remove(d.metaFile())
		fmt.Fprintln(stdout, "cluster removed")
		return 0
	}
	pgCtl, err := pgTool("pg_ctl")
	if err != nil {
		fmt.Fprintf(stderr, "keel db: %v\n", err)
		return 1
	}
	if devDBRunning(pgCtl, d.dir) {
		if err := runPGCommand(pgCtl, "-D", d.dir, "-m", "fast", "-w", "stop"); err != nil {
			fmt.Fprintf(stderr, "keel db: %v\n", err)
			return 1
		}
	}
	if err := os.RemoveAll(d.dir); err != nil {
		fmt.Fprintf(stderr, "keel db: %v\n", err)
		return 1
	}
	_ = os.Remove(d.passFile())
	_ = os.Remove(d.logFile())
	_ = os.Remove(d.metaFile())
	fmt.Fprintln(stdout, "cluster removed")
	return 0
}

func initDevDBCluster(d devDB, stderr io.Writer) error {
	initdb, err := pgTool("initdb")
	if err != nil {
		return err
	}
	major, err := initdbMajor(initdb)
	if err != nil {
		return err
	}
	if major < 18 {
		return fmt.Errorf("PostgreSQL 18+ is required (initdb reports %d)", major)
	}
	if info, err := os.Stat(d.dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", d.dir)
		}
		entries, err := os.ReadDir(d.dir)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("%s is not empty and has no PG_VERSION; refusing to initialize over it", d.dir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(d.dir, 0o700); err != nil {
		return err
	}
	if _, err := ensurePasswordFile(d.passFile()); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "initializing a PostgreSQL cluster in %s (superuser %s, loopback only)\n", d.dir, d.user)
	args := []string{
		"-D", d.dir,
		"-U", d.user,
		"--pwfile=" + d.passFile(),
		"--auth-local=trust",
		"--auth-host=scram-sha-256",
		"--encoding=UTF8",
		"--no-locale",
	}
	if out, err := exec.Command(initdb, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("initdb failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ensurePasswordFile(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		pass := strings.TrimSpace(string(b))
		if pass == "" {
			return "", fmt.Errorf("password file %s is empty", path)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return "", err
		}
		return pass, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	pass := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(pass+"\n"), 0o600); err != nil {
		return "", err
	}
	return pass, nil
}

func startDevDBCluster(d devDB, pgCtl string) error {
	if err := os.MkdirAll(filepath.Dir(d.logFile()), 0o700); err != nil {
		return err
	}
	opts := fmt.Sprintf("-p %d -h 127.0.0.1", d.port)
	if err := runPGCommand(pgCtl, "-D", d.dir, "-l", d.logFile(), "-o", opts, "-w", "start"); err != nil {
		return fmt.Errorf("%v (see %s)", err, d.logFile())
	}
	return nil
}

func waitDevDBReady(d devDB) error {
	postgresDSN, err := d.dsnFor("postgres")
	if err != nil {
		return err
	}
	deadline := time.Now().Add(devDBReadyTimeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		conn, err := pgx.Connect(ctx, postgresDSN)
		if err == nil {
			_ = conn.Close(ctx)
			cancel()
			return nil
		}
		cancel()
		if time.Now().After(deadline) {
			return fmt.Errorf("cluster did not become ready; see %s", d.logFile())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ensureDevDBDatabase talks to the server over TCP instead of shelling out to
// psql/createdb: a bundled PostgreSQL runtime carries only initdb/pg_ctl/
// postgres, and the client tools are not part of the minimal distribution.
func ensureDevDBDatabase(d devDB) error {
	postgresDSN, err := d.dsnFor("postgres")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, postgresDSN)
	if err != nil {
		return fmt.Errorf("connect to postgres database: %w", err)
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", d.database).Scan(&exists); err != nil {
		return fmt.Errorf("query database existence: %w", err)
	}
	if exists {
		return nil
	}
	identifier := pgx.Identifier{d.database}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		return fmt.Errorf("create database %q: %w", d.database, err)
	}
	return nil
}

func (d devDB) dsn() (string, error) {
	return d.dsnFor(d.database)
}

func (d devDB) dsnFor(database string) (string, error) {
	b, err := os.ReadFile(d.passFile())
	if err != nil {
		return "", fmt.Errorf("no password file at %s (run start first)", d.passFile())
	}
	pass := strings.TrimSpace(string(b))
	if pass == "" {
		return "", fmt.Errorf("password file %s is empty", d.passFile())
	}
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(d.user, pass),
		Host:     net.JoinHostPort("127.0.0.1", strconv.Itoa(d.port)),
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}
	return u.String(), nil
}

func pgTool(name string) (string, error) {
	for _, dir := range pgSearchDirs() {
		if path := findPGTool(dir, name); path != "" {
			return path, nil
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	if pgConfig, err := exec.LookPath("pg_config"); err == nil {
		if out, err := exec.Command(pgConfig, "--bindir").Output(); err == nil {
			if path := findPGTool(strings.TrimSpace(string(out)), name); path != "" {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("missing PostgreSQL tool %q (bundled runtime or PATH); install PostgreSQL 18 or use the self-contained release", name)
}

// findPGTool resolves the Windows `.exe` suffix as well as the POSIX name.
func findPGTool(dir, name string) string {
	candidates := []string{name}
	if !strings.HasSuffix(strings.ToLower(name), ".exe") {
		candidates = append(candidates, name+".exe")
	}
	for _, candidate := range candidates {
		path := filepath.Join(dir, candidate)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func initdbMajor(initdb string) (int, error) {
	out, err := exec.Command(initdb, "--version").Output()
	if err != nil {
		return 0, fmt.Errorf("initdb --version: %w", err)
	}
	return parsePGMajor(string(out))
}

var pgMajorPattern = regexp.MustCompile(`PostgreSQL\)\s+(\d+)`)

func parsePGMajor(version string) (int, error) {
	m := pgMajorPattern.FindStringSubmatch(version)
	if m == nil {
		return 0, fmt.Errorf("cannot parse initdb version %q", strings.TrimSpace(version))
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, err
	}
	return n, nil
}

func devDBRunning(pgCtl, dir string) bool {
	return exec.Command(pgCtl, "-D", dir, "status").Run() == nil
}

func portFree(port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("port %d is already in use on 127.0.0.1; stop that process or pass --port", port)
	}
	return ln.Close()
}

func runPGCommand(bin string, args ...string) error {
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
