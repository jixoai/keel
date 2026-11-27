package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBoardRoutesMountBesideProductSubtree protects the outer mux shape: the
// board patterns are concrete and win over the product "/" subtree, while a
// normal product route still reaches the product handler.
func TestBoardRoutesMountBesideProductSubtree(t *testing.T) {
	product := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "product")
	})
	board := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "board:"+r.URL.Path)
	})
	mux := http.NewServeMux()
	mux.Handle("/", product)
	mux.Handle("/v1/board", board)
	mux.Handle("/v1/board/", board)

	cases := map[string]string{
		"/v1/board":             "board:/v1/board",
		"/v1/board/channels":    "board:/v1/board/channels",
		"/v1/board/events":      "board:/v1/board/events",
		"/v1/sessions":          "product",
		"/v1/blockers":          "product",
		"/v1/boardroom":         "product",
		"/v1/board/../sessions": "product",
	}
	for path, want := range cases {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		if want == "product" && recorder.Code >= 300 && recorder.Code < 400 {
			// ServeMux cleans ".." with a redirect; follow the target here so the
			// assertion is about where the final path lands.
			target := recorder.Header().Get("Location")
			request = httptest.NewRequest(http.MethodGet, target, nil)
			recorder = httptest.NewRecorder()
			mux.ServeHTTP(recorder, request)
		}
		if got := recorder.Body.String(); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}
