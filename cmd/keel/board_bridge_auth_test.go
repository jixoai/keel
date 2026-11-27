package main

import (
	"net/http"
	"testing"
)

// TestBoardBridgeRoute locks the pre-session bridge exception: only the signed
// Chat SDK event push bypasses keel auth, and only with POST.
func TestBoardBridgeRoute(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/v1/board/sync/bridge/telegram/events", true},
		{http.MethodGet, "/v1/board/sync/bridge/telegram/events", false},
		{http.MethodPost, "/v1/board/sync/bridge/telegram", false},
		{http.MethodPost, "/v1/board/sync/entries", false},
		{http.MethodPost, "/v1/board/messages", false},
	}
	for _, tc := range cases {
		if got := boardBridgeRoute(tc.method, tc.path); got != tc.want {
			t.Fatalf("boardBridgeRoute(%s, %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}
