// Ported from PI, packages/mcp/test/loopback.test.ts (MIT License). Test
// titles and expectations follow the upstream cases.

package oauth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	mcpoauth "github.com/jixoai/keel/internal/mcp/oauth"
)

func listenAt(t *testing.T, serverURL string, documents bool) *mcpoauth.LoopbackCallback {
	t.Helper()
	options := mcpoauth.LoopbackListenOptions{ServerURL: serverURL, Store: memoryStore(), Settings: mcpoauth.Settings{}}
	if documents {
		options.ClientMetadataDocuments = &mcpoauth.ClientMetadataDocuments{
			BaseURL:          "https://app.example/oauth",
			ListsRedirectURL: func(string) bool { return true },
		}
	}
	loopback, err := mcpoauth.ListenForLoopbackSignIn(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loopback.Callback.Close() })
	return loopback
}

func pendingRedirect(redirectURL string) *mcpoauth.SignInRedirect {
	return &mcpoauth.SignInRedirect{
		AuthorizationURL: "https://auth.example/authorize",
		RedirectURL:      redirectURL,
		State:            "state-1",
	}
}

func TestLoopbackSignInCompletesThroughCallbackServer(t *testing.T) {
	server := startAuthorizationServer(t)
	store := memoryStore()
	loopback, err := mcpoauth.ListenForLoopbackSignIn(mcpoauth.LoopbackListenOptions{
		ServerURL: server.serverURL(), Store: store, Settings: mcpoauth.Settings{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer loopback.Callback.Close()
	if !strings.HasPrefix(loopback.RedirectURL, "http://127.0.0.1:") || !strings.HasSuffix(loopback.RedirectURL, "/callback") {
		t.Fatalf("redirect URL = %q", loopback.RedirectURL)
	}
	options := mcpoauth.SignInOptions{
		ServerURL:   server.serverURL(),
		RedirectURL: loopback.RedirectURL,
		ClientName:  "app",
		Settings:    mcpoauth.Settings{},
		Store:       store,
		HTTPClient:  testHTTPClient,
	}
	authorization := startRedirect(t, options)
	wait := make(chan mcpoauth.Callback, 1)
	waitErr := make(chan error, 1)
	go func() {
		callback, err := mcpoauth.WaitForLoopbackSignIn(mcpoauth.LoopbackWaitOptions{
			Callback:      loopback.Callback,
			Authorization: *authorization,
			Context:       context.Background(),
		})
		if err != nil {
			waitErr <- err
			return
		}
		wait <- callback
	}()
	location := authorizeInBrowser(t, authorization.AuthorizationURL)
	response, err := testHTTPClient.Get(location)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	var callback mcpoauth.Callback
	select {
	case callback = <-wait:
	case err := <-waitErr:
		t.Fatal(err)
	}
	if err := mcpoauth.CompleteSignIn(context.Background(), options, *authorization, callback); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Load(context.Background())
	if state.Tokens == nil || state.Tokens.AccessToken != "access-1" {
		t.Errorf("tokens = %#v", state.Tokens)
	}
	if len(server.registrations) != 1 {
		t.Fatalf("registrations = %#v", server.registrations)
	}
	redirects, _ := server.registrations[0]["redirect_uris"].([]any)
	if len(redirects) != 1 || redirects[0] != loopback.RedirectURL || server.registrations[0]["application_type"] != "native" {
		t.Errorf("registration = %#v", server.registrations[0])
	}
}

func TestLoopbackSignInRejectsResponseOnAnotherPath(t *testing.T) {
	const serverURL = "https://mcp.example/mcp"
	loopback := listenAt(t, serverURL, true)
	id := mcpoauth.CallbackID(serverURL)
	waitErr := make(chan error, 1)
	go func() {
		_, err := mcpoauth.WaitForLoopbackSignIn(mcpoauth.LoopbackWaitOptions{
			Callback:      loopback.Callback,
			Authorization: *pendingRedirect(loopback.RedirectURL + "/" + id),
			Context:       context.Background(),
		})
		waitErr <- err
	}()
	// A mixed-up authorization server redirects to the shared callback path.
	response, err := testHTTPClient.Get(loopback.RedirectURL + "?code=c&state=state-1")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	err = <-waitErr
	if err == nil || !strings.Contains(err.Error(), "arrived on another redirect URI") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoopbackSignInTakesPastedRedirect(t *testing.T) {
	loopback := listenAt(t, "https://mcp.example/mcp", false)
	redirectURL := loopback.RedirectURL
	callback, err := mcpoauth.WaitForLoopbackSignIn(mcpoauth.LoopbackWaitOptions{
		Callback:      loopback.Callback,
		Authorization: *pendingRedirect(redirectURL),
		PromptForRedirectURL: func(context.Context) (string, error) {
			return redirectURL + "?code=pasted&state=state-1", nil
		},
		Context: context.Background(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if callback.Code != "pasted" || callback.State != "state-1" {
		t.Errorf("callback = %#v", callback)
	}
}

func TestLoopbackSignInCancelsWait(t *testing.T) {
	loopback := listenAt(t, "https://mcp.example/mcp", false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := mcpoauth.WaitForLoopbackSignIn(mcpoauth.LoopbackWaitOptions{
			Callback:      loopback.Callback,
			Authorization: *pendingRedirect(loopback.RedirectURL),
			Context:       ctx,
		})
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, mcpoauth.ErrSignInCancelled) {
		t.Fatalf("error = %v", err)
	}

	empty := pendingRedirect(loopback.RedirectURL)
	empty.State = "state-2"
	_, err := mcpoauth.WaitForLoopbackSignIn(mcpoauth.LoopbackWaitOptions{
		Callback:      loopback.Callback,
		Authorization: *empty,
		PromptForRedirectURL: func(context.Context) (string, error) {
			return "", nil
		},
		Context: context.Background(),
	})
	if !errors.Is(err, mcpoauth.ErrSignInCancelled) {
		t.Fatalf("empty paste error = %v", err)
	}
}

func TestPastedRedirectURLs(t *testing.T) {
	authorization := pendingRedirect("http://127.0.0.1:1234/callback/server-id")
	redirectURL := authorization.RedirectURL

	callback, err := mcpoauth.ResponseFromRedirectURL(" "+redirectURL+"?code=c&state=state-1&iss=x ", *authorization)
	if err != nil {
		t.Fatal(err)
	}
	if callback.Code != "c" || callback.State != "state-1" || callback.ISS != "x" {
		t.Errorf("callback = %#v", callback)
	}

	cases := []struct {
		input string
		want  string
	}{
		{redirectURL + "?code=c&state=other", "belongs to a different sign-in"},
		{"http://127.0.0.1:1234/callback?code=c&state=state-1", "does not match this sign-in's redirect URI"},
		{redirectURL + "?error=access_denied", "access_denied"},
		{"code=c", "Expected the full redirect URL"},
	}
	for _, testCase := range cases {
		_, err := mcpoauth.ResponseFromRedirectURL(testCase.input, *authorization)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%q error = %v, want %q", testCase.input, err, testCase.want)
		}
	}
}
