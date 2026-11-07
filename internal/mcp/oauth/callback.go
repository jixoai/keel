// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/oauth/callback.ts (MIT License); implemented independently
// in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// The loopback callback server of an interactive sign-in: it serves the
// redirect URI, matches the `state`, and hands the authorization response to
// the waiting sign-in.

package oauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Callback is an authorization response.
type Callback struct {
	Code  string
	State string
	ISS   string
}

// CallbackPage is the browser page after the redirect.
type CallbackPage struct {
	OK      bool
	Message string
	Details string
}

// CallbackServerOptions configures the loopback callback server.
type CallbackServerOptions struct {
	// Host is the listen address. Empty uses 127.0.0.1.
	Host string
	// RedirectHost is the host name in the redirect URI, for example localhost
	// for a client registered with it while listening on 127.0.0.1.
	RedirectHost string
	Port         int
	Path         string
	// ExtraPaths also receive the callback, for example a server-specific path.
	ExtraPaths []string
	Timeout    time.Duration
	// RenderPage renders the browser page as HTML. Nil renders plain text.
	RenderPage func(page CallbackPage) string
}

// CallbackResult is the outcome of one WaitForCallback.
type CallbackResult struct {
	Callback Callback
	Err      error
}

// CallbackFuture is one pending callback.
type CallbackFuture struct {
	ch   chan CallbackResult
	once sync.Once
}

// Await waits for the authorization response, the server closing, the timeout,
// or ctx.
func (f *CallbackFuture) Await(ctx context.Context) (Callback, error) {
	select {
	case result := <-f.ch:
		return result.Callback, result.Err
	case <-ctx.Done():
		return Callback{}, ctx.Err()
	}
}

func (f *CallbackFuture) resolve(result CallbackResult) {
	f.once.Do(func() { f.ch <- result })
}

type pendingCallback struct {
	future *CallbackFuture
	timer  *time.Timer
	path   string
}

// CallbackServer is the loopback callback HTTP server.
type CallbackServer struct {
	redirectURL string
	paths       []string
	timeout     time.Duration
	render      func(CallbackPage) string

	mu      sync.Mutex
	pending map[string]*pendingCallback
	closed  bool
	server  *http.Server
	ln      net.Listener
}

// ListenCallbackServer binds the callback server.
func ListenCallbackServer(options CallbackServerOptions) (*CallbackServer, error) {
	host := options.Host
	if host == "" {
		host = "127.0.0.1"
	}
	redirectHost := options.RedirectHost
	if redirectHost == "" {
		redirectHost = host
	}
	path := options.Path
	if path == "" {
		path = "/callback"
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(options.Port)))
	if err != nil {
		return nil, err
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		listener.Close()
		return nil, fmt.Errorf("OAuth callback server did not bind to TCP")
	}
	displayHost := redirectHost
	if strings.Contains(displayHost, ":") {
		displayHost = "[" + displayHost + "]"
	}
	server := &CallbackServer{
		redirectURL: fmt.Sprintf("http://%s:%d%s", displayHost, address.Port, path),
		paths:       append([]string{path}, options.ExtraPaths...),
		timeout:     timeout,
		render:      options.RenderPage,
		pending:     map[string]*pendingCallback{},
	}
	server.server = &http.Server{Handler: http.HandlerFunc(server.handle)}
	go func() { _ = server.server.Serve(listener) }()
	server.ln = listener
	return server, nil
}

// RedirectURL is the redirect URI the server serves.
func (s *CallbackServer) RedirectURL() string { return s.redirectURL }

// WaitForCallback waits for the authorization response with state. With path,
// a response on another path fails, so a server-specific redirect URI can tell
// authorization servers apart (RFC 9700 section 4.4.2.2).
func (s *CallbackServer) WaitForCallback(state, path string) (*CallbackFuture, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("OAuth callback server closed")
	}
	if _, exists := s.pending[state]; exists {
		return nil, fmt.Errorf("OAuth state is already pending")
	}
	future := &CallbackFuture{ch: make(chan CallbackResult, 1)}
	entry := &pendingCallback{future: future, path: path}
	entry.timer = time.AfterFunc(s.timeout, func() {
		s.mu.Lock()
		if current, ok := s.pending[state]; ok && current == entry {
			delete(s.pending, state)
		}
		s.mu.Unlock()
		future.resolve(CallbackResult{Err: fmt.Errorf("OAuth callback timed out")})
	})
	s.pending[state] = entry
	return future, nil
}

// Close stops the server and rejects every pending callback.
func (s *CallbackServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	pending := s.pending
	s.pending = map[string]*pendingCallback{}
	s.mu.Unlock()
	for _, entry := range pending {
		entry.timer.Stop()
		entry.future.resolve(CallbackResult{Err: fmt.Errorf("OAuth callback server closed")})
	}
	return s.server.Close()
}

func (s *CallbackServer) reply(w http.ResponseWriter, status int, page CallbackPage) {
	if s.render != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(s.render(page)))
		return
	}
	text := ""
	if page.OK {
		text = "Authorization complete. You may close this window."
	} else if page.Details != "" {
		text = page.Message + "\n\n" + page.Details
	} else {
		text = page.Message
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(text))
}

func (s *CallbackServer) handle(w http.ResponseWriter, r *http.Request) {
	base, _ := url.Parse(s.redirectURL)
	requestURL := base.ResolveReference(&url.URL{Path: r.URL.Path, RawQuery: r.URL.RawQuery})
	allowed := false
	for _, path := range s.paths {
		if requestURL.Path == path {
			allowed = true
			break
		}
	}
	if !allowed {
		s.reply(w, http.StatusNotFound, CallbackPage{Message: "Not found"})
		return
	}
	state := requestURL.Query().Get("state")
	s.mu.Lock()
	entry := s.pending[state]
	if entry != nil {
		delete(s.pending, state)
	}
	s.mu.Unlock()
	if state == "" || entry == nil {
		s.reply(w, http.StatusBadRequest, CallbackPage{Message: "Invalid or expired OAuth state"})
		return
	}
	entry.timer.Stop()
	if entry.path != "" && requestURL.Path != entry.path {
		entry.future.resolve(CallbackResult{Err: fmt.Errorf("The authorization response arrived on another redirect URI")})
		s.reply(w, http.StatusBadRequest, CallbackPage{Message: "Unexpected redirect URI"})
		return
	}
	if errorCode := requestURL.Query().Get("error"); errorCode != "" {
		description := requestURL.Query().Get("error_description")
		if description == "" {
			description = errorCode
		}
		entry.future.resolve(CallbackResult{Err: fmt.Errorf("%s", description)})
		s.reply(w, http.StatusOK, CallbackPage{
			Message: "Authorization failed. You may close this window.",
			Details: description,
		})
		return
	}
	code := requestURL.Query().Get("code")
	if code == "" {
		entry.future.resolve(CallbackResult{Err: fmt.Errorf("OAuth callback did not include an authorization code")})
		s.reply(w, http.StatusBadRequest, CallbackPage{Message: "Missing authorization code"})
		return
	}
	entry.future.resolve(CallbackResult{Callback: Callback{
		Code:  code,
		State: state,
		ISS:   requestURL.Query().Get("iss"),
	}})
	s.reply(w, http.StatusOK, CallbackPage{OK: true})
}
