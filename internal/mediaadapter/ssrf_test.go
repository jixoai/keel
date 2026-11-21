package mediaadapter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestHTTPFetcherRefusesLoopback pins the default SSRF boundary: a provider
// that answers with a loopback asset URL must not reach the local server.
func TestHTTPFetcherRefusesLoopback(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("internal-secret"))
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher(0)
	_, err := fetcher.Fetch(context.Background(), server.URL+"/asset.png")
	if !errors.Is(err, ErrAssetHostNotPublic) {
		t.Fatalf("error = %v, want ErrAssetHostNotPublic", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the loopback fixture was reached")
	}
	if fetcher.client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("the asset transport must not use an environment proxy: it would bypass the address guard")
	}
}

// TestHTTPFetcherRefusesNonPublicLiterals covers every address class the guard
// exists for, including the cloud metadata endpoint.
func TestHTTPFetcherRefusesNonPublicLiterals(t *testing.T) {
	urls := []string{
		"http://0.0.0.0/a",
		"http://10.1.2.3/a",
		"http://100.64.0.1/a",
		"http://127.0.0.1/a",
		"http://169.254.169.254/latest/meta-data",
		"http://172.16.0.1/a",
		"http://192.168.1.1/a",
		"http://224.0.0.1/a",
		"http://[::1]/a",
		"http://[fc00::1]/a",
		"http://[fe80::1]/a",
	}
	fetcher := NewHTTPFetcher(0)
	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			_, err := fetcher.Fetch(context.Background(), raw)
			if !errors.Is(err, ErrAssetHostNotPublic) {
				t.Fatalf("error = %v, want ErrAssetHostNotPublic", err)
			}
		})
	}
}

// TestHTTPFetcherRefusesHostnameResolvingToPrivate proves the guard is not a
// string check on the host: a public-looking name whose DNS answer is private
// (or mixed public/private, a rebinding pattern) is refused before dialing.
func TestHTTPFetcherRefusesHostnameResolvingToPrivate(t *testing.T) {
	cases := []struct {
		name string
		ips  []string
	}{
		{name: "private", ips: []string{"10.1.2.3"}},
		{name: "loopback", ips: []string{"127.0.0.1"}},
		{name: "metadata", ips: []string{"169.254.169.254"}},
		{name: "mixed public and private", ips: []string{"93.184.216.34", "192.168.1.10"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := NewHTTPFetcher(0)
			fetcher.resolver = func(context.Context, string) ([]net.IPAddr, error) {
				addresses := make([]net.IPAddr, 0, len(tc.ips))
				for _, raw := range tc.ips {
					addresses = append(addresses, net.IPAddr{IP: net.ParseIP(raw)})
				}
				return addresses, nil
			}
			fetcher.dial = func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("dial must not run for a refused host")
			}
			_, err := fetcher.Fetch(context.Background(), "http://cdn.example/asset.png")
			if !errors.Is(err, ErrAssetHostNotPublic) {
				t.Fatalf("error = %v, want ErrAssetHostNotPublic", err)
			}
		})
	}
}

// TestHTTPFetcherRefusesRedirectToPrivate pins the per-hop boundary: a public
// first hop that redirects to a private host is refused before the second dial.
func TestHTTPFetcherRefusesRedirectToPrivate(t *testing.T) {
	var firstHits, privateHits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		http.Redirect(w, r, "http://internal.example/secret", http.StatusFound)
	}))
	defer server.Close()
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		privateHits.Add(1)
		_, _ = w.Write([]byte("internal-secret"))
	}))
	defer private.Close()

	fetcher := NewHTTPFetcher(0)
	fetcher.resolver = func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "cdn.example":
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
		case "internal.example":
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil
		default:
			return nil, fmt.Errorf("unexpected host %q", host)
		}
	}
	fetcher.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if strings.HasPrefix(address, "93.184.216.34:") {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		return nil, fmt.Errorf("unexpected dial %q", address)
	}

	_, err := fetcher.Fetch(context.Background(), "http://cdn.example/asset")
	if !errors.Is(err, ErrAssetHostNotPublic) {
		t.Fatalf("error = %v, want ErrAssetHostNotPublic", err)
	}
	if firstHits.Load() != 1 {
		t.Fatalf("first hop hits = %d, want 1", firstHits.Load())
	}
	if privateHits.Load() != 0 {
		t.Fatal("the private redirect target was reached")
	}
}

// TestHTTPFetcherFetchesPublicAsset is the control: a host resolving to a
// public address is fetched normally.
func TestHTTPFetcherFetchesPublicAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("asset fetch carried a credential: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte("public-bytes"))
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher(0)
	fetcher.resolver = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "cdn.example" {
			return nil, fmt.Errorf("unexpected host %q", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	}
	fetcher.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if !strings.HasPrefix(address, "93.184.216.34:") {
			return nil, fmt.Errorf("unexpected dial %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}

	asset, err := fetcher.Fetch(context.Background(), "http://cdn.example/asset.png")
	if err != nil {
		t.Fatalf("public asset fetch: %v", err)
	}
	if string(asset.Data) != "public-bytes" {
		t.Fatalf("asset bytes = %q", asset.Data)
	}
}

// TestHTTPFetcherAllowPrivateHostsIsExplicitOptIn pins the test/local escape
// hatch; it is a constructor option, never a runtime default.
func TestHTTPFetcherAllowPrivateHostsIsExplicitOptIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("local-bytes"))
	}))
	defer server.Close()
	fetcher := NewHTTPFetcherWithOptions(HTTPFetcherOptions{AllowPrivateHosts: true})
	asset, err := fetcher.Fetch(context.Background(), server.URL+"/asset")
	if err != nil {
		t.Fatalf("allow-private fetch: %v", err)
	}
	if string(asset.Data) != "local-bytes" {
		t.Fatalf("asset bytes = %q", asset.Data)
	}
}
