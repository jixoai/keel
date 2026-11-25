// `keel update`: unsigned self-update from the project's GitHub releases.
//
// The update is HTTPS + a SHA-256 from the same release + an atomic replace of
// this binary. No macOS notarization or Windows code-signing certificate is
// required: the binary is already unsigned, and the .app is re-signed ad-hoc
// after the swap. The checksum detects a truncated/corrupted download, not a
// compromised release account; signed manifests can be added later without
// changing this command.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// defaultUpdateManifest is the public release page, not the GitHub API: the
// API allows only 60 unauthenticated requests per IP per hour, which breaks
// updates for users behind shared NAT. The page redirects to the tag page; the
// command reads the tag from that redirect and downloads assets directly.
const defaultUpdateManifest = "https://github.com/jixoai/keel/releases/latest"

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type releaseDoc struct {
	TagName      string         `json:"tag_name"`
	Assets       []releaseAsset `json:"assets"`
	DownloadBase string         `json:"-"`
}

func runUpdateCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		check    = fs.Bool("check", false, "only report whether a newer release exists")
		force    = fs.Bool("force", false, "reinstall the latest release even when the version matches")
		manifest = fs.String("manifest", envOr("KEEL_UPDATE_MANIFEST", defaultUpdateManifest), "release page or GitHub API JSON (project release page by default)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if u, err := url.Parse(*manifest); err == nil && u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		fmt.Fprintln(stderr, "keel update: warning: plain HTTP manifest; checksums can be tampered with")
	}
	doc, err := fetchRelease(*manifest)
	if err != nil {
		fmt.Fprintf(stderr, "keel update: %v\n", err)
		return 1
	}
	latest := strings.TrimPrefix(strings.TrimSpace(doc.TagName), "v")
	if latest == "" {
		fmt.Fprintln(stderr, "keel update: the release document has no tag_name")
		return 1
	}
	cmp, err := compareVersions(latest, version)
	if err != nil {
		fmt.Fprintf(stderr, "keel update: %v\n", err)
		return 1
	}
	if cmp < 0 {
		fmt.Fprintf(stdout, "keel %s is newer than the latest release %s; nothing to do\n", version, latest)
		return 0
	}
	if cmp == 0 && !*force {
		fmt.Fprintf(stdout, "keel %s is up to date (latest %s)\n", version, latest)
		return 0
	}
	assetName, archiveKind := updateAssetName(latest)
	assetURL, ok := releaseAssetURL(doc, assetName)
	if !ok {
		fmt.Fprintf(stderr, "keel update: release %s has no asset %s\n", doc.TagName, assetName)
		return 1
	}
	sumsURL, ok := releaseAssetURL(doc, "SHA256SUMS.txt")
	if !ok {
		fmt.Fprintf(stderr, "keel update: release %s has no SHA256SUMS.txt\n", doc.TagName)
		return 1
	}
	if *check {
		fmt.Fprintf(stdout, "keel %s → %s available (%s)\n", version, latest, assetName)
		return 0
	}

	sumBytes, err := fetch(sumsURL)
	if err != nil {
		fmt.Fprintf(stderr, "keel update: download checksums: %v\n", err)
		return 1
	}
	want, err := checksumFor(string(sumBytes), assetName)
	if err != nil {
		fmt.Fprintf(stderr, "keel update: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "downloading %s (%s)\n", assetName, latest)
	archiveBytes, err := fetch(assetURL)
	if err != nil {
		fmt.Fprintf(stderr, "keel update: download: %v\n", err)
		return 1
	}
	got := sha256.Sum256(archiveBytes)
	if hex.EncodeToString(got[:]) != want {
		fmt.Fprintf(stderr, "keel update: checksum mismatch for %s\n", assetName)
		return 1
	}
	tmp, err := os.MkdirTemp("", "keel-update-")
	if err != nil {
		fmt.Fprintf(stderr, "keel update: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmp)
	binary := filepath.Join(tmp, "keel")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if err := extractUpdateBinary(archiveBytes, archiveKind, binary); err != nil {
		fmt.Fprintf(stderr, "keel update: %v\n", err)
		return 1
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		fmt.Fprintf(stderr, "keel update: %v\n", err)
		return 1
	}
	if err := verifyStagedBinary(binary, latest); err != nil {
		fmt.Fprintf(stderr, "keel update: %v\n", err)
		return 1
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "keel update: %v\n", err)
		return 1
	}
	// Stage next to the target, not in TMPDIR: rename(2) is only atomic within
	// one filesystem, and /tmp is commonly a different mount on Linux.
	staged := exe + ".new"
	if runtime.GOOS != "windows" {
		// On Windows the detached helper consumes the staged file after this
		// process exits; removing it here would delete the update.
		defer os.Remove(staged)
	}
	if err := copyStagedBinary(binary, staged); err != nil {
		fmt.Fprintf(stderr, "keel update: stage: %v\n", err)
		return 1
	}
	if err := applyUpdate(staged, exe, stderr); err != nil {
		fmt.Fprintf(stderr, "keel update: %v\n", err)
		return 1
	}
	if runtime.GOOS == "windows" {
		fmt.Fprintf(stdout, "update staged; close Keel if it is running — it will be replaced and restarted (failure log: %s.update.log)\n", exe)
	} else {
		fmt.Fprintf(stdout, "updated to %s; restart keel to use it\n", latest)
	}
	return 0
}

func copyStagedBinary(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// verifyStagedBinary catches a wrong-architecture or corrupted extraction
// before the running binary is replaced. The release always reports its tag.
func verifyStagedBinary(binary, want string) error {
	out, err := exec.Command(binary, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("downloaded binary did not run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || fields[0] != "keel" {
		return fmt.Errorf("downloaded binary printed unexpected version output: %q", strings.TrimSpace(string(out)))
	}
	got := strings.TrimPrefix(fields[1], "v")
	if got != want {
		return fmt.Errorf("downloaded binary reports %s, release is %s", got, want)
	}
	return nil
}

func fetchRelease(manifest string) (releaseDoc, error) {
	resp, body, err := httpGet(manifest)
	if err != nil {
		return releaseDoc{}, fmt.Errorf("fetch release: %w", err)
	}
	var doc releaseDoc
	if json.Unmarshal(body, &doc) == nil && strings.TrimSpace(doc.TagName) != "" {
		return doc, nil
	}
	// The public release page redirects to /releases/tag/<tag>. Derive the
	// direct download base from that URL so no API token is needed.
	tag, base, ok := releaseFromURL(resp.Request.URL)
	if !ok {
		return releaseDoc{}, fmt.Errorf("cannot read a release tag from %s", resp.Request.URL)
	}
	doc.TagName = tag
	doc.DownloadBase = base
	return doc, nil
}

func releaseFromURL(u *url.URL) (tag, base string, ok bool) {
	const marker = "/releases/tag/"
	i := strings.Index(u.Path, marker)
	if i < 0 {
		return "", "", false
	}
	tag = strings.TrimSuffix(strings.TrimPrefix(u.Path[i:], marker), "/")
	if tag == "" {
		return "", "", false
	}
	base = u.Scheme + "://" + u.Host + strings.TrimSuffix(u.Path[:i], "/") + "/releases/download/" + tag
	return tag, base, true
}

func releaseAssetURL(doc releaseDoc, name string) (string, bool) {
	if asset, ok := assetByName(doc, name); ok {
		return asset.BrowserDownloadURL, true
	}
	if doc.DownloadBase != "" {
		return strings.TrimSuffix(doc.DownloadBase, "/") + "/" + name, true
	}
	return "", false
}

func httpGet(url string) (*http.Response, []byte, error) {
	client := &http.Client{Timeout: 15 * time.Minute}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "keel/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

func fetch(url string) ([]byte, error) {
	_, body, err := httpGet(url)
	return body, err
}

func assetByName(doc releaseDoc, name string) (releaseAsset, bool) {
	for _, asset := range doc.Assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return releaseAsset{}, false
}

func updateAssetName(ver string) (name, kind string) {
	name = fmt.Sprintf("keel-%s-%s-%s", ver, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		return name + ".zip", "zip"
	}
	return name + ".tar.gz", "tar.gz"
}

func compareVersions(a, b string) (int, error) {
	pa, err := versionNumbers(a)
	if err != nil {
		return 0, err
	}
	pb, err := versionNumbers(b)
	if err != nil {
		return 0, err
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x < y {
			return -1, nil
		}
		if x > y {
			return 1, nil
		}
	}
	return 0, nil
}

func versionNumbers(v string) ([]int, error) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	nums := make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid version %q", v)
		}
		nums = append(nums, n)
	}
	return nums, nil
}

func checksumFor(sums, name string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		file := strings.TrimPrefix(fields[1], "*")
		if file == name {
			if len(fields[0]) != sha256.Size*2 {
				return "", fmt.Errorf("invalid checksum for %s", name)
			}
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS.txt has no entry for %s", name)
}

func extractUpdateBinary(archive []byte, kind, dst string) error {
	switch kind {
	case "tar.gz":
		gz, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("read archive: %w", err)
			}
			if path.Base(hdr.Name) != "keel" || hdr.Typeflag != tar.TypeReg {
				continue
			}
			return writeReader(dst, tr)
		}
		return errors.New("archive has no keel binary")
	case "zip":
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		for _, file := range zr.File {
			if path.Base(file.Name) != "keel.exe" || file.FileInfo().IsDir() {
				continue
			}
			rc, err := file.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeReader(dst, rc)
		}
		return errors.New("archive has no keel.exe")
	default:
		return fmt.Errorf("unknown archive kind %q", kind)
	}
}

func writeReader(dst string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// applyUpdate replaces the running executable. Unix allows renaming over a
// running binary; Windows locks it, so a detached helper waits for this process
// to exit and then swaps and starts the new one.
func applyUpdate(staged, exe string, stderr io.Writer) error {
	if runtime.GOOS == "windows" {
		return applyWindowsUpdate(staged, exe)
	}
	if err := os.Rename(staged, exe); err != nil {
		return err
	}
	if err := os.Chmod(exe, 0o755); err != nil {
		return err
	}
	if app := appBundleFor(exe); app != "" {
		signApp(app, stderr)
		if _, err := exec.LookPath("codesign"); err == nil {
			if out, err := exec.Command("codesign", "--verify", "--deep", "--strict", app).CombinedOutput(); err != nil {
				return fmt.Errorf("binary replaced but app signature verification failed (%v): %s; reinstall with keel install --force", err, strings.TrimSpace(string(out)))
			}
		}
	}
	return nil
}

func appBundleFor(exe string) string {
	macOS := filepath.Dir(exe)
	if filepath.Base(macOS) != "MacOS" {
		return ""
	}
	contents := filepath.Dir(macOS)
	if filepath.Base(contents) != "Contents" {
		return ""
	}
	return filepath.Dir(contents)
}

func windowsImageName(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func windowsUpdateScript(staged, exe string, pid int) string {
	image := windowsImageName(exe)
	// Wait for this updater and for every process of the same image name: a
	// running installed app also locks the target. Bound the wait, then retry
	// the move to ride out antivirus briefly; leave a log on final failure.
	return fmt.Sprintf(`@echo off
setlocal
set /a waits=0
:wait_pid
tasklist /FI "PID eq %d" | find "%d" >nul
if not errorlevel 1 (
  ping -n 2 127.0.0.1 >nul
  goto wait_pid
)
:wait_image
tasklist /FI "IMAGENAME eq %s" | find /I "%s" >nul
if not errorlevel 1 goto image_seen
goto replace
:image_seen
set /a waits+=1
if %%waits%% geq 60 goto replace
ping -n 2 127.0.0.1 >nul
goto wait_image
:replace
set /a tries=0
:retry
move /Y "%s" "%s" >nul 2>&1
if not errorlevel 1 goto done
set /a tries+=1
if %%tries%% geq 12 goto failed
ping -n 2 127.0.0.1 >nul
goto retry
:failed
echo keel update: could not replace "%s" > "%s.update.log"
exit /b 1
:done
start "" "%s" start
del "%%~f0"
`, pid, pid, image, image, staged, exe, exe, exe, exe)
}

func applyWindowsUpdate(staged, exe string) error {
	helper := exe + ".update.cmd"
	script := windowsUpdateScript(staged, exe, os.Getpid())
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		return err
	}
	return exec.Command("cmd", "/c", "start", "", "cmd", "/c", helper).Start()
}
