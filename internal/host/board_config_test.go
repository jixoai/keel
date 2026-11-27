package host

import "testing"

// TestBoardTenantConfig covers the tenant switch document the board mount
// reads (keel-board-v1 design §20): the admin parser accepts it, defaults stay
// false, and validation does not reject a sync-only document (it is inert until
// board.enabled is also on).
func TestBoardTenantConfig(t *testing.T) {
	for _, raw := range []string{
		`{"board":{"enabled":true,"sync":{"enabled":true}}}`,
		`{"board":{"enabled":true}}`,
		`{"board":{"sync":{"enabled":true}}}`,
		`{"board":{}}`,
	} {
		config, err := ParseTenantConfig([]byte(raw))
		if err != nil {
			t.Fatalf("ParseTenantConfig(%s): %v", raw, err)
		}
		if err := config.Validate(); err != nil {
			t.Fatalf("Validate(%s): %v", raw, err)
		}
	}
	empty, err := ParseTenantConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Board.Enabled || empty.Board.Sync.Enabled {
		t.Fatalf("default board flags = %+v, want both false", empty.Board)
	}
	merged := Merge(TenantConfig{}, TenantConfig{Board: BoardConfig{Enabled: true, Sync: BoardSyncConfig{Enabled: true}}})
	if !merged.Board.Enabled || !merged.Board.Sync.Enabled {
		t.Fatalf("merge dropped board flags: %+v", merged.Board)
	}
}
