package main

import "testing"

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
