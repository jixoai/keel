package boardintegration

import "testing"

// TestParseBoardFlagsMatrix is the tenant-switch truth table: both default
// false, sync cannot enable the board, and a malformed document fails closed.
func TestParseBoardFlagsMatrix(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want BoardFlags
		err  bool
	}{
		{name: "empty document", doc: `{}`, want: BoardFlags{}},
		{name: "board on", doc: `{"board":{"enabled":true}}`, want: BoardFlags{Enabled: true}},
		{name: "sync without board", doc: `{"board":{"sync":{"enabled":true}}}`, want: BoardFlags{}},
		{name: "both on", doc: `{"board":{"enabled":true,"sync":{"enabled":true}}}`, want: BoardFlags{Enabled: true, Sync: true}},
		{name: "board off sync on", doc: `{"board":{"enabled":false,"sync":{"enabled":true}}}`, want: BoardFlags{}},
		{name: "malformed", doc: `{"board":`, err: true},
		{name: "unknown fields are ignored by this reader", doc: `{"agent":{},"board":{"enabled":true}}`, want: BoardFlags{Enabled: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBoardFlags([]byte(tc.doc))
			if tc.err {
				if err == nil {
					t.Fatalf("ParseBoardFlags(%s) = %+v, want error", tc.doc, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBoardFlags(%s): %v", tc.doc, err)
			}
			if got != tc.want {
				t.Fatalf("ParseBoardFlags(%s) = %+v, want %+v", tc.doc, got, tc.want)
			}
		})
	}
}

func TestBoardSyncPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/board/sync":                      true,
		"/v1/board/sync/entries":              true,
		"/v1/board/sync/entries/slack/status": true,
		"/v1/board/sources/slack/read":        true,
		"/v1/board/messages":                  false,
		"/v1/board/syncx":                     false,
	} {
		if got := boardSyncPath(path); got != want {
			t.Fatalf("boardSyncPath(%q) = %v, want %v", path, got, want)
		}
	}
}
