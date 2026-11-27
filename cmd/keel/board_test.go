package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestBoardDeploymentEnabled(t *testing.T) {
	cases := []struct {
		value string
		want  bool
		fail  bool
	}{
		{"", false, false},
		{"1", true, false},
		{"true", true, false},
		{"on", true, false},
		{"0", false, false},
		{"false", false, false},
		{"off", false, false},
		{"maybe", false, true},
	}
	for _, test := range cases {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv("KEEL_BOARD_ENABLED", test.value)
			got, err := boardDeploymentEnabled()
			if test.fail {
				if err == nil {
					t.Fatalf("KEEL_BOARD_ENABLED=%q was accepted", test.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("KEEL_BOARD_ENABLED=%q: %v", test.value, err)
			}
			if got != test.want {
				t.Fatalf("KEEL_BOARD_ENABLED=%q = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestBoardDSNPrefersTheDedicatedDSN(t *testing.T) {
	t.Setenv("KEEL_BOARD_DSN", "")
	if got := boardDSN("keel-dsn"); got != "keel-dsn" {
		t.Fatalf("boardDSN without a dedicated DSN = %q", got)
	}
	t.Setenv("KEEL_BOARD_DSN", "board-dsn")
	if got := boardDSN("keel-dsn"); got != "board-dsn" {
		t.Fatalf("boardDSN with a dedicated DSN = %q", got)
	}
}

// TestBoardDefaultOffWiring keeps the default-off path honest: the mux gets a
// handler that answers 404 board_disabled without opening a board connection.
func TestBoardDefaultOffWiring(t *testing.T) {
	t.Setenv("KEEL_BOARD_ENABLED", "")
	wiring, err := newBoardWiring(context.Background(), "", uuid.Nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("newBoardWiring: %v", err)
	}
	if wiring.Surface != nil {
		t.Fatal("a disabled deployment opened a board surface")
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.NotFoundHandler())
	mux.Handle("/v1/board", wiring.API)
	mux.Handle("/v1/board/", wiring.API)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/board/channels", nil))
	if recorder.Code != http.StatusNotFound || !contains(recorder.Body.String(), "board_disabled") {
		t.Fatalf("disabled board = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
