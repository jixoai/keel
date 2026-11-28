package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		// Windows has no POSIX group/other bits; the file is protected by the
		// user-profile ACL instead.
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

// fakePGDaemonScript writes a fake pg_ctl that prints one line, optionally
// starts a detached child that keeps the inherited stdio open for about five
// seconds, and exits with code. spawnChild reproduces the Windows postmaster
// handle inheritance that made the old CombinedOutput path wait forever.
func fakePGDaemonScript(t *testing.T, dir string, spawnChild bool, code int) (string, []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		script := filepath.Join(dir, "fake-pgctl.cmd")
		var b strings.Builder
		b.WriteString("@echo off\r\n")
		b.WriteString("echo fake pg_ctl start\r\n")
		if spawnChild {
			b.WriteString("start \"\" /b cmd /c \"ping -n 6 127.0.0.1 >nul\"\r\n")
		}
		fmt.Fprintf(&b, "exit /b %d\r\n", code)
		if err := os.WriteFile(script, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return os.Getenv("COMSPEC"), []string{"/c", script}
	}
	script := filepath.Join(dir, "fake-pgctl.sh")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("echo fake pg_ctl start\n")
	if spawnChild {
		b.WriteString("(sleep 5) &\n")
	}
	fmt.Fprintf(&b, "exit %d\n", code)
	if err := os.WriteFile(script, []byte(b.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, nil
}

func TestStartPGDaemonDoesNotWaitForInheritedStdio(t *testing.T) {
	dir := t.TempDir()
	// The fake child keeps the ctl log open for ~5s. POSIX lets TempDir unlink
	// an open file; Windows cannot, so give the child time to exit before the
	// cleanup registered by t.TempDir runs (cleanup is LIFO).
	t.Cleanup(func() {
		if runtime.GOOS == "windows" {
			time.Sleep(5 * time.Second)
		}
	})
	logPath := filepath.Join(dir, "dev-pg.log")
	bin, args := fakePGDaemonScript(t, dir, true, 0)
	started := time.Now()
	if err := startPGDaemon(bin, args, logPath); err != nil {
		t.Fatalf("startPGDaemon: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("startPGDaemon waited %v for the daemon's inherited stdio; want < 2s", elapsed)
	}
	ctl, err := os.ReadFile(logPath + ".ctl")
	if err != nil {
		t.Fatalf("ctl log: %v", err)
	}
	if !strings.Contains(string(ctl), "fake pg_ctl start") {
		t.Fatalf("ctl log = %q; want pg_ctl output", string(ctl))
	}
}

func TestStartPGDaemonErrorNamesLogs(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dev-pg.log")
	bin, args := fakePGDaemonScript(t, dir, false, 3)
	err := startPGDaemon(bin, args, logPath)
	if err == nil {
		t.Fatal("startPGDaemon succeeded; want an error")
	}
	for _, want := range []string{".ctl", "dev-pg.log", "fake pg_ctl start"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
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
