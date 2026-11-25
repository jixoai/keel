package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.1", "0.3.1", 0},
		{"v0.3.1", "0.3.0", 1},
		{"0.4", "0.3.9", 1},
		{"0.3.1-rc1", "0.3.1", 0},
		{"1.0.0+build", "1.0.0", 0},
	}
	for _, tc := range cases {
		got, err := compareVersions(tc.a, tc.b)
		if err != nil {
			t.Fatalf("compareVersions(%q,%q): %v", tc.a, tc.b, err)
		}
		if got != tc.want {
			t.Errorf("compareVersions(%q,%q)=%d want %d", tc.a, tc.b, got, tc.want)
		}
	}
	if _, err := compareVersions("dev", "0.3.1"); err == nil {
		t.Error("compareVersions accepted a non-numeric version")
	}
}

func TestChecksumFor(t *testing.T) {
	sum := strings.Repeat("a", 64)
	sums := "deadbeef  other.tar.gz\n" + sum + "  keel-0.4.0-darwin-arm64.tar.gz\n"
	got, err := checksumFor(sums, "keel-0.4.0-darwin-arm64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if got != sum {
		t.Fatalf("checksum=%q want %q", got, sum)
	}
	if _, err := checksumFor(sums, "missing.tar.gz"); err == nil {
		t.Fatal("checksumFor accepted a missing asset")
	}
}

func TestUpdateAssetName(t *testing.T) {
	name, kind := updateAssetName("0.4.0")
	want := "keel-0.4.0-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		want += ".zip"
		if kind != "zip" {
			t.Errorf("kind=%q want zip", kind)
		}
	} else {
		want += ".tar.gz"
		if kind != "tar.gz" {
			t.Errorf("kind=%q want tar.gz", kind)
		}
	}
	if name != want {
		t.Errorf("asset=%q want %q", name, want)
	}
}

func TestExtractUpdateBinaryTar(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("new-keel")
	if err := tw.WriteHeader(&tar.Header{Name: "./keel", Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "keel")
	if err := extractUpdateBinary(buf.Bytes(), "tar.gz", dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("binary=%q want %q", got, body)
	}
}

func TestExtractUpdateBinaryZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("keel.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("windows-keel")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "keel.exe")
	if err := extractUpdateBinary(buf.Bytes(), "zip", dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "windows-keel" {
		t.Fatalf("binary=%q", got)
	}
}

func TestRunUpdateCheckNewer(t *testing.T) {
	asset, _ := updateAssetName("9.9.9")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := releaseDoc{TagName: "v9.9.9", Assets: []releaseAsset{
			{Name: asset, BrowserDownloadURL: "http://example.invalid/" + asset},
			{Name: "SHA256SUMS.txt", BrowserDownloadURL: "http://example.invalid/SHA256SUMS.txt"},
		}}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()
	var out, errOut bytes.Buffer
	code := runUpdateCmd([]string{"--check", "--manifest", srv.URL}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "available") || !strings.Contains(out.String(), "9.9.9") {
		t.Fatalf("output=%q", out.String())
	}
}

func TestRunUpdateUpToDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(releaseDoc{TagName: "v" + version})
	}))
	defer srv.Close()
	var out, errOut bytes.Buffer
	code := runUpdateCmd([]string{"--manifest", srv.URL}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("output=%q", out.String())
	}
}

func TestAppBundleFor(t *testing.T) {
	app := filepath.Join(t.TempDir(), "Keel.app")
	exe := filepath.Join(app, "Contents", "MacOS", "keel-bin")
	if got := appBundleFor(exe); got != app {
		t.Fatalf("appBundleFor=%q want %q", got, app)
	}
	if got := appBundleFor("/usr/local/bin/keel"); got != "" {
		t.Fatalf("appBundleFor plain exe=%q want empty", got)
	}
}

func TestReleaseFromURL(t *testing.T) {
	u, _ := url.Parse("https://github.com/jixoai/keel/releases/tag/v0.4.0")
	tag, base, ok := releaseFromURL(u)
	if !ok || tag != "v0.4.0" {
		t.Fatalf("tag=%q ok=%v", tag, ok)
	}
	if base != "https://github.com/jixoai/keel/releases/download/v0.4.0" {
		t.Fatalf("base=%q", base)
	}
	if _, _, ok := releaseFromURL(&url.URL{Path: "/jixoai/keel"}); ok {
		t.Fatal("accepted a URL without a tag")
	}
}

func TestRunUpdateCheckFromReleasePage(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jixoai/keel/releases/latest" {
			http.Redirect(w, r, "/jixoai/keel/releases/tag/v9.9.9", http.StatusFound)
			return
		}
		hits++
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>release</html>"))
	}))
	defer srv.Close()
	var out, errOut bytes.Buffer
	code := runUpdateCmd([]string{"--check", "--manifest", srv.URL + "/jixoai/keel/releases/latest"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "9.9.9") {
		t.Fatalf("output=%q", out.String())
	}
	if hits != 1 {
		t.Fatalf("tag page hits=%d want 1", hits)
	}
}

func TestVerifyStagedBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture")
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("#!/bin/sh\necho 'keel 9.9.9'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyStagedBinary(good, "9.9.9"); err != nil {
		t.Fatalf("good binary: %v", err)
	}
	if err := verifyStagedBinary(good, "1.2.3"); err == nil {
		t.Fatal("accepted a mismatched version")
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho nope\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyStagedBinary(bad, "9.9.9"); err == nil {
		t.Fatal("accepted unexpected output")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if !isLoopbackHost(host) {
			t.Errorf("%q should be loopback", host)
		}
	}
	if isLoopbackHost("example.com") {
		t.Error("example.com treated as loopback")
	}
}

func TestWindowsUpdateScript(t *testing.T) {
	script := windowsUpdateScript(`C:\Users\a b\AppData\Local\Keel\keel.exe.new`, `C:\Users\a b\AppData\Local\Keel\keel.exe`, 4242)
	for _, want := range []string{
		`tasklist /FI "PID eq 4242"`,
		`tasklist /FI "IMAGENAME eq keel.exe"`,
		`move /Y "C:\Users\a b\AppData\Local\Keel\keel.exe.new" "C:\Users\a b\AppData\Local\Keel\keel.exe"`,
		`start "" "C:\Users\a b\AppData\Local\Keel\keel.exe" start`,
		`del "%~f0"`,
		`if %tries% geq 12`,
		`if %waits% geq 60`,
		`:image_seen`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n%s", want, script)
		}
	}
	if strings.Contains(script, "%%") {
		t.Errorf("script kept a doubled percent escape:\n%s", script)
	}
}

func TestRunUpdateRefusesDowngrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(releaseDoc{TagName: "v0.0.1"})
	}))
	defer srv.Close()
	var out, errOut bytes.Buffer
	code := runUpdateCmd([]string{"--force", "--manifest", srv.URL}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "newer than") {
		t.Fatalf("output=%q", out.String())
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/plain/path":      "'/plain/path'",
		"/with space/keel": "'/with space/keel'",
		"/it's/keel":       `'/it'\''s/keel'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q)=%q want %q", in, got, want)
		}
	}
}

func TestInstallCLIShim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var out, errOut bytes.Buffer
	app := filepath.Join(home, "Applications", "Keel.app")
	installCLIShim(app, &out, &errOut)
	path := filepath.Join(home, ".local", "bin", "keel")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	if !strings.Contains(script, cliShimMarker) || !strings.Contains(script, "keel-bin") {
		t.Fatalf("unexpected shim:\n%s", script)
	}
	// A foreign file at the same path must survive a reinstall.
	if err := os.WriteFile(path, []byte("#!/bin/sh\\necho user\\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	installCLIShim(app, &out, &errOut)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "#!/bin/sh\\necho user\\n" {
		t.Fatalf("foreign cli was overwritten: %q", after)
	}
	// A generated shim is removed on uninstall; the foreign one is not.
	removeCLIShim(&out)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("foreign cli should not be removed")
	}
}

func TestPathHasDir(t *testing.T) {
	t.Setenv("PATH", strings.Join([]string{"/a", "/b c"}, string(os.PathListSeparator)))
	if !pathHasDir("/a") || !pathHasDir("/b c") {
		t.Fatal("existing PATH entries not found")
	}
	if pathHasDir("/a/b") {
		t.Fatal("substring matched a different PATH entry")
	}
}

func TestInstallCLIShimSkipsSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "marker-file")
	if err := os.WriteFile(target, []byte(cliShimMarker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "keel")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	installCLIShim(filepath.Join(home, "Keel.app"), &out, &errOut)
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != cliShimMarker+"\n" {
		t.Fatalf("symlink target was overwritten: %q", data)
	}
}
