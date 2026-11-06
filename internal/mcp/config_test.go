// Tests ported from @earendil-works/pi-mcp (55a5c6914570e2ae31ca36297654483bbbf46883),
// packages/mcp/test/config.test.ts (MIT License). Each subtest keeps the title
// of the pi test it ports.

package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func validateRaw(t *testing.T, name, raw string, options ...McpServerConfigOptions) McpServerConfigResult {
	t.Helper()
	var opts McpServerConfigOptions
	if len(options) > 0 {
		opts = options[0]
	}
	return ValidateMcpServerConfig(name, json.RawMessage(raw), opts)
}

func errorOf(result McpServerConfigResult) string {
	if result.Kind == "invalid" {
		return result.Error
	}
	return ""
}

func TestValidateMcpServerConfig(t *testing.T) {
	t.Run("accepts stdio and HTTP entries and keeps them as given", func(t *testing.T) {
		stdio := `{"command":"npx","args":["-y","server"],"env":{"A":"1"},"cwd":"sub","enabled":false}`
		result := validateRaw(t, "files", stdio)
		if result.Kind != "stdio" {
			t.Fatalf("kind = %q, want stdio (%s)", result.Kind, result.Error)
		}
		if result.Config["cwd"] != "sub" {
			t.Errorf("config = %#v, want the entry as given", result.Config)
		}
		if got := validateRaw(t, "files", `{"type":"stdio","command":"x"}`).Kind; got != "stdio" {
			t.Errorf("type=stdio kind = %q", got)
		}
		httpEntry := `{"url":"https://example.com/mcp","headers":{"Authorization":"Bearer token"},"oauth":{"clientId":"id","callbackPort":8080,"scope":"read"},"timeout":30,"description":"Docs"}`
		if got := validateRaw(t, "docs", httpEntry); got.Kind != "http" || got.Config["description"] != "Docs" {
			t.Errorf("http entry = %#v", got)
		}
		for _, kind := range []string{"http", "streamable-http"} {
			raw := `{"type":"` + kind + `","url":"http://localhost:3000/mcp"}`
			if got := validateRaw(t, "docs", raw).Kind; got != "http" {
				t.Errorf("type=%s kind = %q", kind, got)
			}
		}
	})

	t.Run("picks the transport from type, url and command", func(t *testing.T) {
		if got := validateRaw(t, "s", `{"url":"https://a.example","command":"x"}`).Kind; got != "http" {
			t.Errorf("both given kind = %q, want http", got)
		}
		if got := validateRaw(t, "s", `{"type":"stdio","url":"https://a.example","command":"x"}`).Kind; got != "stdio" {
			t.Errorf("type=stdio both given kind = %q, want stdio", got)
		}
		if got := errorOf(validateRaw(t, "s", `{"type":"http","command":"x"}`)); got != `server "s" needs either "command" (stdio) or "url" (streamable HTTP)` {
			t.Errorf("type=http command error = %q", got)
		}
		if got := errorOf(validateRaw(t, "s", `{"type":"sse","url":"https://a.example/sse"}`)); got != `server "s": legacy SSE transport is not supported; use the streamable HTTP URL` {
			t.Errorf("legacy sse error = %q", got)
		}
	})

	t.Run("reports invalid names and fields", func(t *testing.T) {
		cases := []struct {
			name string
			raw  string
			want string
		}{
			{"bad name", `{"command":"x"}`, `invalid server name "bad name" (use letters, digits, "_" and "-")`},
			{"s", `"npx"`, `server "s" must be an object`},
			{"s", `[]`, `server "s" must be an object`},
			{"s", `{"command":"x","enabled":"yes"}`, `server "s": enabled must be a boolean`},
			{"s", `{"command":"x","description":1}`, `server "s": description must be a string`},
			{"s", `{"command":"x","timeout":0}`, `server "s": timeout must be a positive number of seconds`},
			{"s", `{"command":"x","args":"a b"}`, `server "s": args must be an array of strings`},
			{"s", `{"command":"x","env":{"A":1}}`, `server "s": env must map names to strings`},
			{"s", `{"command":"x","cwd":1}`, `server "s": cwd must be a string`},
			{"s", `{"url":"file:///x"}`, `server "s": url must be an http or https URL`},
			{"s", `{"url":"not a url"}`, `server "s": url must be an http or https URL`},
			{"s", `{"url":"https://a.example","headers":{"A":1}}`, `server "s": headers must map names to strings`},
			{"s", `{}`, `server "s" needs either "command" (stdio) or "url" (streamable HTTP)`},
		}
		for _, test := range cases {
			if got := errorOf(validateRaw(t, test.name, test.raw)); got != test.want {
				t.Errorf("validate(%s, %s) = %q, want %q", test.name, test.raw, got, test.want)
			}
		}
	})

	t.Run("checks oauth settings", func(t *testing.T) {
		oauth := func(settings string) string {
			return errorOf(validateRaw(t, "s", `{"url":"https://a.example","oauth":`+settings+`}`))
		}
		if got := oauth(`"x"`); got != `server "s": oauth must be an object` {
			t.Errorf("string oauth = %q", got)
		}
		if got := oauth(`{"clientId":1}`); got != `server "s": oauth.clientId must be a string` {
			t.Errorf("clientId = %q", got)
		}
		if got := oauth(`{"clientSecret":1}`); got != `server "s": oauth.clientSecret must be a string` {
			t.Errorf("clientSecret = %q", got)
		}
		if got := oauth(`{"callbackPort":70000}`); got != `server "s": oauth.callbackPort must be a port number` {
			t.Errorf("callbackPort = %q", got)
		}
		for _, callback := range []string{`"https://localhost/callback"`, `"http://example.com/callback"`, `"http://localhost/callback?x=1"`} {
			if got := oauth(`{"callbackUrl":` + callback + `}`); !strings.Contains(got, "oauth.callbackUrl must be an http URI") {
				t.Errorf("callbackUrl %s = %q", callback, got)
			}
		}
		if got := oauth(`{"callbackUrl":"http://[::1]:8080/cb","callbackPort":8080}`); got != "" {
			t.Errorf("loopback callback = %q", got)
		}
		if got := oauth(`{"callbackUrl":"http://localhost:8080/cb","callbackPort":9090}`); got != `server "s": oauth.callbackUrl and oauth.callbackPort name different ports` {
			t.Errorf("port mismatch = %q", got)
		}
		if got := oauth(`{"scope":1}`); got != `server "s": oauth.scope must be a string` {
			t.Errorf("scope = %q", got)
		}
		if got := oauth(`{"clientName":" "}`); got != `server "s": oauth.clientName must be a non-empty string` {
			t.Errorf("clientName = %q", got)
		}
		if got := oauth(`{"clientRegistration":"x"}`); got != `server "s": oauth.clientRegistration must be "auto", "dcr", or "cimd"` {
			t.Errorf("registration = %q", got)
		}
		if got := oauth(`{"clientRegistration":"dcr","clientName":"app"}`); got != "" {
			t.Errorf("dcr = %q", got)
		}
		if got := oauth(`{"authServerMetadataUrl":"http://auth.example/.well-known/x"}`); !strings.Contains(got, "oauth.authServerMetadataUrl must be an https URL") {
			t.Errorf("metadata http = %q", got)
		}
		if got := oauth(`{"authServerMetadataUrl":"http://127.0.0.1/.well-known/x"}`); got != "" {
			t.Errorf("metadata loopback = %q", got)
		}
	})

	t.Run("allows cimd only with the application's check of the callback", func(t *testing.T) {
		entry := func(oauth string) string {
			return `{"url":"https://a.example","oauth":` + oauth + `}`
		}
		if got := errorOf(validateRaw(t, "s", entry(`{"clientRegistration":"cimd"}`))); got != `server "s": oauth.clientRegistration "cimd" is not supported: there are no Client ID Metadata Documents` {
			t.Errorf("cimd without check = %q", got)
		}
		checked := []string{}
		options := McpServerConfigOptions{CheckMetadataDocumentCallback: func(callbackURL string) string {
			checked = append(checked, callbackURL)
			if strings.HasSuffix(callbackURL, "/other") {
				return "the documents do not list it"
			}
			return ""
		}}
		if got := validateRaw(t, "s", entry(`{"clientRegistration":"cimd"}`), options).Kind; got != "http" {
			t.Errorf("cimd kind = %q", got)
		}
		if got := errorOf(validateRaw(t, "s", entry(`{"clientRegistration":"cimd","callbackUrl":"http://localhost/other"}`), options)); got != `server "s": the documents do not list it` {
			t.Errorf("cimd rejected callback = %q", got)
		}
		if len(checked) != 2 || checked[0] != "" || checked[1] != "http://localhost/other" {
			t.Errorf("checked callbacks = %#v", checked)
		}
		if got := errorOf(validateRaw(t, "s", entry(`{"clientRegistration":"cimd","clientId":"id"}`), options)); got != `server "s": oauth.clientRegistration "cimd" cannot be combined with oauth.clientId or oauth.clientName` {
			t.Errorf("cimd with clientId = %q", got)
		}
	})
}

// TestValidateMcpServerConfigKeepsUnknownFields is pi's "keeps unknown fields
// for the caller, after the shared fields are valid".
func TestValidateMcpServerConfigKeepsUnknownFields(t *testing.T) {
	appConfig := func(raw string) string {
		result := validateRaw(t, "s", raw)
		if result.Kind == "invalid" {
			return result.Error
		}
		if shared, ok := result.Config["shared"]; ok {
			if _, ok := shared.(bool); !ok {
				return `server "s": shared must be a boolean`
			}
		}
		if result.Kind == "http" {
			if _, ok := result.Config["runOn"]; ok {
				return `server "s": runOn is for stdio`
			}
		}
		return result.Kind + " shared=" + stringify(result.Config["shared"])
	}
	if got := appConfig(`{"command":"x","shared":true,"runOn":"host"}`); got != "stdio shared=true" {
		t.Errorf("stdio = %q", got)
	}
	if got := appConfig(`{"url":"https://a.example","runOn":"host"}`); got != `server "s": runOn is for stdio` {
		t.Errorf("http = %q", got)
	}
	if got := appConfig(`{"command":"x","shared":1}`); got != `server "s": shared must be a boolean` {
		t.Errorf("shared = %q", got)
	}
	if got := appConfig(`{"command":"x","shared":1,"enabled":"no"}`); got != `server "s": enabled must be a boolean` {
		t.Errorf("shared fields first = %q", got)
	}
}

func stringify(value any) string {
	switch typed := value.(type) {
	case nil:
		return "<nil>"
	case bool:
		if typed {
			return "true"
		}
		return "false"
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func TestIsMcpServerOverride(t *testing.T) {
	cases := map[string]bool{
		`{"enabled":false}`:                true,
		`{}`:                               true,
		`{"command":"x"}`:                  false,
		`{"url":"https://a.example"}`:      false,
		`{"type":"stdio","enabled":false}`: false,
	}
	for raw, want := range cases {
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatal(err)
		}
		if got := IsMcpServerOverride(entry); got != want {
			t.Errorf("IsMcpServerOverride(%s) = %v, want %v", raw, got, want)
		}
	}
}
