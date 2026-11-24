// `keel start`: the one command a downloaded binary needs. It extracts the
// embedded runtime (PostgreSQL + web client), lets the existing auto-DB path
// initialize the local cluster, starts the product server on loopback, and
// opens the browser. No KEEL_DSN, no Docker, no manual initdb.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

func runStartCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		addr   = fs.String("addr", envOr("KEEL_SERVE_ADDR", "127.0.0.1:13750"), "listen address")
		dsn    = fs.String("dsn", os.Getenv("KEEL_DSN"), "PostgreSQL DSN (default: the bundled local cluster)")
		noOpen = fs.Bool("no-open", false, "do not open the browser")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if hasEmbeddedPG() {
		if _, err := ensureRuntime(stderr); err != nil {
			fmt.Fprintf(stderr, "keel start: %v\n", err)
			return 1
		}
	} else {
		fmt.Fprintln(stderr, "keel start: this build has no bundled PostgreSQL; using the local PostgreSQL on PATH")
	}

	tenant, err := localTenant()
	if err != nil {
		fmt.Fprintf(stderr, "keel start: %v\n", err)
		return 1
	}

	serveArgs := []string{"--addr", *addr, "--auth", "local-only", "--tenant", tenant}
	if web := bundledWebDir(); web != "" {
		serveArgs = append(serveArgs, "--web-dir", web)
	}
	if strings.TrimSpace(*dsn) != "" {
		serveArgs = append(serveArgs, "--dsn", *dsn)
	}
	if !*noOpen {
		go openWhenReady(*addr, stderr)
	}
	return runServeCmd(serveArgs, stdout, stderr)
}

// localTenant keeps one UUID per installation so the local-only product has a
// stable tenant across restarts. An explicit KEEL_SERVE_TENANT always wins.
func localTenant() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("KEEL_SERVE_TENANT")); explicit != "" {
		return explicit, nil
	}
	home := keelHome()
	if home == "" {
		return "", errors.New("cannot resolve KEEL_HOME for the local tenant")
	}
	path := filepath.Join(home, "tenant")
	if data, err := os.ReadFile(path); err == nil {
		tenant := strings.TrimSpace(string(data))
		if _, err := uuid.Parse(tenant); err == nil {
			return tenant, nil
		}
	}
	tenant := uuid.NewString()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tenant+"\n"), 0o600); err != nil {
		return "", err
	}
	return tenant, nil
}

func openWhenReady(addr string, stderr io.Writer) {
	url := browserURL(addr)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url + "/v1/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if err := openBrowser(url); err != nil {
					fmt.Fprintf(stderr, "keel start: open %s: %v\n", url, err)
				}
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "keel start: server did not become ready; open %s manually\n", url)
}

func browserURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Run()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Run()
	default:
		return exec.Command("xdg-open", url).Run()
	}
}
