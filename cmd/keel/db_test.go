package main

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePGMajor(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"initdb (PostgreSQL) 18.7 (Homebrew)", 18, true},
		{"initdb (PostgreSQL) 17.2", 17, true},
		{"initdb (PostgreSQL) 19beta1", 19, true},
		{"initdb version unknown", 0, false},
	}
	for _, tc := range cases {
		got, err := parsePGMajor(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("parsePGMajor(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Fatalf("parsePGMajor(%q) succeeded; want an error", tc.in)
		}
	}
}

func TestDevDBDSN(t *testing.T) {
	dir := t.TempDir()
	d := devDB{dir: filepath.Join(dir, "pg"), port: 13741, database: "keel", user: "keel"}
	if err := os.WriteFile(d.passFile(), []byte("deadbeef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dsn, err := d.dsn()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "postgres" || u.Host != "127.0.0.1:13741" || u.Path != "/keel" {
		t.Fatalf("unexpected DSN %q", dsn)
	}
	if u.User.Username() != "keel" {
		t.Fatalf("unexpected user in %q", dsn)
	}
	if pass, _ := u.User.Password(); pass != "deadbeef" {
		t.Fatalf("unexpected password in %q", dsn)
	}
	if u.Query().Get("sslmode") != "disable" {
		t.Fatalf("unexpected query in %q", dsn)
	}
}

func TestDevDBMetaRoundTrip(t *testing.T) {
	d := devDB{dir: filepath.Join(t.TempDir(), "pg")}
	want := devDBMeta{Port: 15432, Database: "app", User: "dev"}
	if err := d.saveMeta(want); err != nil {
		t.Fatal(err)
	}
	got, err := d.loadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("meta = %+v; want %+v", got, want)
	}
	if info, err := os.Stat(d.metaFile()); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("metadata file is accessible to group/other: %v", info.Mode())
	}
}

func TestDefaultDevDBDirHonoursKeelHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KEEL_HOME", home)
	if got, want := defaultDevDBDir(), filepath.Join(home, "dev-pg"); got != want {
		t.Fatalf("defaultDevDBDir() = %q; want %q", got, want)
	}
}

func TestResetRefusesForeignDirectory(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "important.txt")
	if err := os.WriteFile(keep, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runDBReset([]string{"--dir", dir, "--yes"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("reset exit = %d; want 1 (stderr: %s)", code, stderr.String())
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("reset removed a foreign file: %v", err)
	}
	if !strings.Contains(stderr.String(), "refusing") {
		t.Fatalf("stderr = %q; want a refusal", stderr.String())
	}
}

func TestResetRequiresYes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runDBReset([]string{"--dir", t.TempDir()}, &stdout, &stderr); code != 2 {
		t.Fatalf("reset without --yes exit = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "--yes") {
		t.Fatalf("stderr = %q; want a --yes hint", stderr.String())
	}
}

func TestDBHelpAndUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"db", "help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("db help exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "keel db start") {
		t.Fatalf("help output = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"db", "nope"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown db command exit = %d; want 2", code)
	}
}

func TestAdminStoreHonoursNoAutoDB(t *testing.T) {
	t.Setenv("KEEL_DSN", "")
	t.Setenv("KEEL_NO_AUTO_DB", "1")
	if _, err := adminStore(context.Background(), io.Discard); err == nil || !strings.Contains(err.Error(), "KEEL_NO_AUTO_DB") {
		t.Fatalf("adminStore error = %v; want a KEEL_NO_AUTO_DB hint", err)
	}
}

func TestServeNoAutoDBWithoutDSN(t *testing.T) {
	t.Setenv("KEEL_DSN", "")
	t.Setenv("KEEL_NO_AUTO_DB", "")
	var stdout, stderr bytes.Buffer
	code := runServeCmd([]string{"--no-auto-db", "--tenant", "11111111-2222-3333-4444-555555555555"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("serve --no-auto-db exit = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "--no-auto-db") {
		t.Fatalf("stderr = %q; want the opt-out hint", stderr.String())
	}
}
