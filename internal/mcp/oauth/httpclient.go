// Keel extension: OAuth requests run through an SSRF-guarded HTTP client.
// PI opens authorization URLs with the platform handler and fetches discovery
// documents with the global fetch. A server-side runtime must not be tricked
// into fetching private addresses, so discovery and token endpoints are
// restricted to http/https and to public addresses; loopback is only allowed
// for an explicit local CLI helper (or tests).

package oauth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned when a request resolves to a private,
// loopback, link-local, or otherwise non-public address.
var ErrBlockedAddress = errors.New("oauth: refusing to fetch a private or loopback address")

// blockedPrefixes are non-public ranges the standard library helpers do not
// fully cover: "this network", benchmark, reserved/broadcast, documentation,
// and the NAT64 well-known prefix (which can embed a private IPv4 address).
var blockedPrefixes = mustParseCIDRs(
	"0.0.0.0/8",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"240.0.0.0/4",
	"255.255.255.255/32",
	"2001:db8::/32",
	"64:ff9b::/96",
)

func mustParseCIDRs(values ...string) []*net.IPNet {
	networks := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			panic("oauth: invalid blocked CIDR " + value)
		}
		networks = append(networks, network)
	}
	return networks
}

// HTTPClientOptions configures the guarded client.
type HTTPClientOptions struct {
	// AllowLoopback permits 127.0.0.0/8 and ::1, for a local CLI helper that
	// talks to a loopback authorization server (and for tests).
	AllowLoopback bool
	// Timeout bounds one request. Zero uses 30 seconds.
	Timeout time.Duration
}

// NewHTTPClient builds the SSRF-guarded client. Redirects are followed (up to
// ten) but every hop is validated again.
func NewHTTPClient(options HTTPClientOptions) *http.Client {
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return ErrBlockedAddress
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return ErrBlockedAddress
			}
			return checkPublicAddress(ip, options.AllowLoopback)
		},
	}
	transport := &http.Transport{
		// No proxy: a proxy would resolve the target itself, bypassing the
		// private-address check that runs on this machine's dialer.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("oauth: stopped after 10 redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return ErrBlockedAddress
			}
			return nil
		},
	}
}

func checkPublicAddress(ip net.IP, allowLoopback bool) error {
	if ip.IsLoopback() {
		if allowLoopback {
			return nil
		}
		return ErrBlockedAddress
	}
	if ip.IsUnspecified() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return ErrBlockedAddress
	}
	// IPv4 carrier-grade NAT (100.64.0.0/10) is not covered by IsPrivate.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return ErrBlockedAddress
	}
	for _, network := range blockedPrefixes {
		if network.Contains(ip) {
			return ErrBlockedAddress
		}
	}
	return nil
}

// validateHTTPURL checks the scheme and host of a URL before a request.
func validateHTTPURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || !IsHTTPURL(parsed) {
		return fmt.Errorf("Invalid OAuth URL %s", value)
	}
	return nil
}

// checkEndpoint validates a configured endpoint and returns it parsed.
func checkEndpoint(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || !IsHTTPURL(parsed) {
		return nil, fmt.Errorf("Invalid OAuth endpoint %s", value)
	}
	return parsed, nil
}

func clientOr(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return NewHTTPClient(HTTPClientOptions{})
}
