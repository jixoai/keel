// Shared CLI client plumbing for the product surfaces (`keel bot`,
// `keel team-bot`, `keel model`, `keel skills`).
//
// Every command is a thin HTTP client of a running `keel serve`: one write
// path, one validation path, one tenant boundary. Nothing here opens the
// database or re-implements a rule — the server answers 400 with field paths,
// 404 for a missing id, 409 for a reference conflict and 403 for a missing
// permission, and this file renders them.
//
// Authentication (change keel-bot-identity-access, A4.3/A4.2b3a): a human
// caller logs in with KEEL_ACCOUNT_USERNAME + KEEL_ACCOUNT_PASSWORD (or
// --password-file) and the session cookie carries the login; a machine caller
// presents a bot key (KEEL_AGENT_TOKEN, keel_bot_...) as a Bearer credential and
// never logs in. Every non-GET request carries the X-Keel-CSRF token issued by
// /v1/auth/session and refreshed by login. A password is never accepted on the
// command line (a flag shows up in `ps`) and no credential is ever printed; the
// only credential-shaped output is the server's own error text, which is
// redacted on the server side.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"

	"github.com/jixoai/keel/internal/botteam"
	"github.com/jixoai/keel/internal/modelcfg"
)

// cliCredential is the resolved client authentication for one invocation.
// Token is a bot key (Bearer, no login); Username/Password is a human account
// login. Neither set means "no credential": a loopback local-only server maps
// the request to its built-in local admin, and auth=required answers 401.
type cliCredential struct {
	Username string
	Password string
	Token    string
}

func (c cliCredential) usesBotKey() bool  { return c.Token != "" }
func (c cliCredential) usesAccount() bool { return c.Username != "" }

// The auth flags live at the client-command level (run() pulls them out before
// the subcommand parses its own flags), so every `keel bot|team-bot|model|skills`
// subcommand accepts them without each FlagSet re-declaring them.
var (
	cliAuthUsername     string
	cliAuthPasswordFile string
)

// extractAuthFlags removes --username/--password-file from a client command's
// arguments and stores them for resolveCLIAuth. A --password flag is refused:
// it would be visible in `ps` and in shell history.
func extractAuthFlags(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		name, value := arg, ""
		inlineValue := false
		if before, after, found := strings.Cut(arg, "="); found {
			name, value, inlineValue = before, after, true
		}
		switch name {
		case "--username", "-username":
			if !inlineValue {
				if index+1 >= len(args) {
					return nil, errors.New("--username needs a value")
				}
				index++
				value = args[index]
			}
			cliAuthUsername = strings.TrimSpace(value)
		case "--password-file", "-password-file":
			if !inlineValue {
				if index+1 >= len(args) {
					return nil, errors.New("--password-file needs a path")
				}
				index++
				value = args[index]
			}
			cliAuthPasswordFile = strings.TrimSpace(value)
		case "--password", "-password":
			return nil, errors.New("--password is not accepted: pass --password-file <path> or set KEEL_ACCOUNT_PASSWORD (a password flag is visible in `ps`)")
		default:
			out = append(out, arg)
		}
	}
	return out, nil
}

// resolveCLIAuth reads the one credential this invocation uses. A retired
// variable is an explicit failure, never a silent fallback: a deployment that
// still exports KEEL_SERVE_PASSWORD must be told that it no longer logs anyone
// in.
func resolveCLIAuth() (cliCredential, error) {
	if strings.TrimSpace(os.Getenv("KEEL_SERVE_PASSWORD")) != "" {
		return cliCredential{}, errors.New("KEEL_SERVE_PASSWORD is retired and no longer read: unset it and use KEEL_ACCOUNT_USERNAME + KEEL_ACCOUNT_PASSWORD, --password-file, or KEEL_AGENT_TOKEN")
	}
	token := strings.TrimSpace(os.Getenv("KEEL_AGENT_TOKEN"))
	username := strings.TrimSpace(cliAuthUsername)
	if username == "" {
		username = strings.TrimSpace(os.Getenv("KEEL_ACCOUNT_USERNAME"))
	}
	password := ""
	if cliAuthPasswordFile != "" {
		raw, err := os.ReadFile(cliAuthPasswordFile)
		if err != nil {
			return cliCredential{}, fmt.Errorf("read --password-file: %w", err)
		}
		// A file written by an editor ends in a newline; the password itself is
		// everything before it (inner spaces are significant).
		password = strings.TrimRight(string(raw), "\r\n")
		if password == "" {
			return cliCredential{}, fmt.Errorf("--password-file %s is empty", cliAuthPasswordFile)
		}
	} else {
		password = os.Getenv("KEEL_ACCOUNT_PASSWORD")
	}
	switch {
	case token != "" && (username != "" || password != ""):
		return cliCredential{}, errors.New("both KEEL_AGENT_TOKEN and account credentials are set: choose one login method")
	case token != "":
		return cliCredential{Token: token}, nil
	case username == "" && password == "":
		return cliCredential{}, nil
	case username == "":
		return cliCredential{}, errors.New("a password is configured but KEEL_ACCOUNT_USERNAME (or --username) is missing")
	case password == "":
		return cliCredential{}, errors.New("KEEL_ACCOUNT_USERNAME is set but the password is missing: set KEEL_ACCOUNT_PASSWORD or pass --password-file")
	default:
		return cliCredential{Username: username, Password: password}, nil
	}
}

// cliClient is one authenticated session against `keel serve`.
type cliClient struct {
	base  string
	http  *http.Client
	debug bool
	// botKey is the Bearer credential of a machine caller; it never logs in.
	botKey string
	// csrf is the X-Keel-CSRF value for writes (the session's double-submit
	// token). Empty means no human session has been established.
	csrf string
}

// apiError is a refusal from the server, carrying the machine code and the
// field paths so the CLI can print a fixable message. Server names the address
// the request went to: a 404 from the wrong port must be visible in the error,
// not inferred from a flag the user thought they had exported.
type apiError struct {
	Status     int
	Code       string
	Message    string
	Server     string
	Fields     []botteam.FieldError
	Issues     []modelcfg.Issue
	References []map[string]any
	// Reason/RequiredKey are the permission engine's 403 detail: what the
	// caller lacks, when the refusal is a missing grant or relation.
	Reason      string
	RequiredKey string
	// Total is the server's real reference count; References may be bounded by
	// the server (the session list is capped). Truncated marks that gap so the
	// CLI can say "and N more" instead of pretending the list is complete.
	Total     int
	Truncated bool
	Revision  int64
}

func (e *apiError) Error() string {
	where := ""
	if e.Server != "" {
		where = " at " + e.Server
	}
	switch {
	case e.Code != "":
		return fmt.Sprintf("server refused (%d %s)%s: %s", e.Status, e.Code, where, e.Message)
	case e.Message != "":
		return fmt.Sprintf("server refused (%d)%s: %s", e.Status, where, e.Message)
	default:
		return fmt.Sprintf("server refused (%d)%s", e.Status, where)
	}
}

// newCLIClient resolves the server address and authenticates with the
// credential the environment (or --username/--password-file) names.
func newCLIClient(server string) (*cliClient, error) {
	base, err := normalizeServerURL(server)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	credential, err := resolveCLIAuth()
	if err != nil {
		return nil, err
	}
	client := &cliClient{base: base, http: &http.Client{Jar: jar}}
	if err := client.login(credential); err != nil {
		return nil, err
	}
	return client, nil
}

// normalizeServerURL accepts "127.0.0.1:13750", "http://127.0.0.1:13750" and a
// trailing slash, and returns a base without a trailing slash.
func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("no server address: set KEEL_SERVE_ADDR or pass --server")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("server address %q: %w", raw, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("server address %q has no host", raw)
	}
	return strings.TrimRight(parsed.Scheme+"://"+parsed.Host+parsed.Path, "/"), nil
}

// login authenticates the resolved credential. A bot key needs no login (it is
// presented on every request); an account first reads the anonymous CSRF token
// from GET /v1/auth/session (the server requires X-Keel-CSRF even for the login
// POST), then exchanges username+password for the session cookie and the
// session's own CSRF token. No credential configured is a no-op: a loopback
// local-only server answers without one, and a server with authentication
// answers 401 on the real request (which names the missing setting), so no
// error is invented here.
func (c *cliClient) login(credential cliCredential) error {
	if credential.usesBotKey() {
		c.botKey = credential.Token
		return nil
	}
	if !credential.usesAccount() {
		return nil
	}
	if err := c.prepareCSRF(); err != nil {
		return err
	}
	var answer struct {
		CSRF string `json:"csrfToken"`
	}
	body := map[string]string{"username": credential.Username, "password": credential.Password}
	if err := c.do(http.MethodPost, "/v1/auth/login", body, &answer); err != nil {
		return fmt.Errorf("login to %s failed: %w", c.base, err)
	}
	if answer.CSRF != "" {
		c.csrf = answer.CSRF
	}
	return nil
}

// prepareCSRF reads the anonymous session document: it sets the CSRF cookie and
// returns the matching token, and it is also where a deployment with no super
// admin says so (bootstrapRequired).
func (c *cliClient) prepareCSRF() error {
	var session struct {
		CSRF              string `json:"csrfToken"`
		Authenticated     bool   `json:"authenticated"`
		BootstrapRequired bool   `json:"bootstrapRequired"`
	}
	if err := c.do(http.MethodGet, "/v1/auth/session", nil, &session); err != nil {
		return fmt.Errorf("read %s/v1/auth/session: %w", c.base, err)
	}
	if session.CSRF != "" {
		c.csrf = session.CSRF
	}
	if session.BootstrapRequired {
		return fmt.Errorf("%s has no super admin: run `keel admin bootstrap` and finish setup before logging in", c.base)
	}
	return nil
}

// do performs one request and decodes a JSON answer into out (which may be nil).
func (c *cliClient) do(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	if c.botKey != "" {
		// A bot key is the whole credential: no session, no CSRF token.
		request.Header.Set("Authorization", "Bearer "+c.botKey)
	} else if c.csrf != "" && method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		request.Header.Set("X-Keel-CSRF", c.csrf)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("connect %s: %w", c.base, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		refusal := readAPIError(response)
		if typed, ok := refusal.(*apiError); ok {
			typed.Server = c.base
			// A 401 on a read with no credential configured is the server asking
			// for the login this shell never supplied. Name the variables instead
			// of making the reader infer them from a one-word body.
			if typed.Status == http.StatusUnauthorized && c.botKey == "" && c.csrf == "" {
				typed.Message += " (set KEEL_ACCOUNT_USERNAME + KEEL_ACCOUNT_PASSWORD, or KEEL_AGENT_TOKEN for a bot key)"
			}
		}
		return refusal
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s %s answer: %w", method, path, err)
	}
	return nil
}

// readAPIError turns the server's error envelope into an apiError.
func readAPIError(response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	out := &apiError{Status: response.StatusCode}
	var envelope struct {
		Error       string               `json:"error"`
		Code        string               `json:"code"`
		Reason      string               `json:"reason"`
		RequiredKey string               `json:"requiredKey"`
		Fields      []botteam.FieldError `json:"fields"`
		Issues      []modelcfg.Issue     `json:"issues"`
		References  []map[string]any     `json:"references"`
		Total       int                  `json:"total"`
		Truncated   bool                 `json:"truncated"`
		Revision    int64                `json:"revision"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		out.Message = strings.TrimSpace(envelope.Error)
		out.Code = envelope.Code
		out.Reason = envelope.Reason
		out.RequiredKey = envelope.RequiredKey
		out.Fields = envelope.Fields
		out.Issues = envelope.Issues
		out.References = envelope.References
		out.Total = envelope.Total
		out.Truncated = envelope.Truncated
		out.Revision = envelope.Revision
	}
	if out.Message == "" {
		out.Message = strings.TrimSpace(string(raw))
	}
	if out.Message == "" {
		out.Message = http.StatusText(response.StatusCode)
	}
	return out
}

// reportError renders a refusal and returns the process exit code: usage errors
// were already handled (2), every server/runtime failure is 1.
func reportError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "keel: %v\n", err)
	var refusal *apiError
	if errors.As(err, &refusal) {
		// The permission engine's 403 names what is missing; a bare "forbidden"
		// is not actionable.
		if refusal.RequiredKey != "" {
			fmt.Fprintf(stderr, "  required permission: %s\n", refusal.RequiredKey)
		}
		if refusal.Reason != "" {
			fmt.Fprintf(stderr, "  reason: %s\n", refusal.Reason)
		}
		for _, field := range refusal.Fields {
			if field.Field == "" {
				fmt.Fprintf(stderr, "  - %s: %s\n", field.Code, field.Message)
				continue
			}
			fmt.Fprintf(stderr, "  - %s (%s): %s\n", field.Field, field.Code, field.Message)
		}
		// Settings validation answers field paths as modelcfg issues (no machine
		// code); render the same `field: message` shape so a rejected models
		// document is fixable from the CLI output.
		for _, issue := range refusal.Issues {
			if issue.Field == "" {
				fmt.Fprintf(stderr, "  - %s\n", issue.Message)
				continue
			}
			fmt.Fprintf(stderr, "  - %s: %s\n", issue.Field, issue.Message)
		}
		if refusal.Revision > 0 {
			fmt.Fprintf(stderr, "  current revision: %d\n", refusal.Revision)
		}
		for _, reference := range refusal.References {
			id, _ := reference["id"].(string)
			name, _ := reference["name"].(string)
			kind, _ := reference["kind"].(string)
			if strings.TrimSpace(kind) == "" {
				kind = "reference"
			}
			field, _ := reference["field"].(string)
			if strings.TrimSpace(field) != "" {
				fmt.Fprintf(stderr, "  referencing %s: %s %s (field %s)\n", kind, id, name, field)
				continue
			}
			fmt.Fprintf(stderr, "  referencing %s: %s %s\n", kind, id, name)
		}
		if refusal.Truncated || refusal.Total > len(refusal.References) {
			more := refusal.Total - len(refusal.References)
			if more > 0 {
				fmt.Fprintf(stderr, "  ... and %d more reference(s)\n", more)
			}
		}
	}
	return 1
}

// --- flag plumbing -------------------------------------------------------

// commonFlags registers the flags every subcommand accepts.
func commonFlags(fs *flag.FlagSet) (server *string, asJSON *bool) {
	server = fs.String("server", envOr("KEEL_SERVE_ADDR", defaultServeAddr), "keel serve address")
	asJSON = fs.Bool("json", false, "print raw JSON")
	return server, asJSON
}

// parseInterleaved parses flags and positionals in any order, so
// `keel team-bot roster <team> --bot x --role member` reads like the help text.
// The stdlib stops at the first positional; this resumes after each one.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	positional := make([]string, 0, len(args))
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// visitSet returns the flag names the user actually passed, so an update can
// distinguish "set to empty" from "not mentioned".
func visitSet(fs *flag.FlagSet) map[string]bool {
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// csv splits a comma list, dropping blanks.
func csv(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// newFlagSet builds a quiet flag set; usage goes to stderr only.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// --- printing ------------------------------------------------------------

func printJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// stringList is a repeatable string flag (`--header a --header b`) and a
// comma-list (`--header a,b`), used by the model registry commands.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			*l = append(*l, trimmed)
		}
	}
	return nil
}
