package access

import "testing"

func TestParseScope(t *testing.T) {
	for _, want := range []Scope{ScopeOwn, ScopeTeam, ScopeTenant, ScopeAll} {
		got, err := ParseScope(string(want))
		if err != nil {
			t.Fatalf("ParseScope(%q) error = %v", want, err)
		}
		if got != want {
			t.Fatalf("ParseScope(%q) = %q, want %q", want, got, want)
		}
	}
	for _, raw := range []string{"", "OWN", "world", "own ", " all", "teams"} {
		if _, err := ParseScope(raw); err == nil {
			t.Fatalf("ParseScope(%q) succeeded, want an error", raw)
		}
	}
}

func TestScopeOrdering(t *testing.T) {
	ordered := []Scope{ScopeOwn, ScopeTeam, ScopeTenant, ScopeAll}
	for leftIndex := range ordered {
		for rightIndex := range ordered {
			left := ordered[leftIndex]
			right := ordered[rightIndex]
			got := CompareScopes(left, right)
			want := 0
			switch {
			case leftIndex < rightIndex:
				want = -1
			case leftIndex > rightIndex:
				want = 1
			}
			if got != want {
				t.Fatalf("CompareScopes(%q, %q) = %d, want %d", left, right, got, want)
			}
			if got := left.Covers(right); got != (leftIndex >= rightIndex) {
				t.Fatalf("Covers(%q, %q) = %v, want %v", left, right, got, leftIndex >= rightIndex)
			}
		}
	}
}

func TestMaxScope(t *testing.T) {
	cases := []struct {
		name   string
		scopes []Scope
		want   Scope
	}{
		{name: "single", scopes: []Scope{ScopeOwn}, want: ScopeOwn},
		{name: "own and team", scopes: []Scope{ScopeOwn, ScopeTeam}, want: ScopeTeam},
		{name: "order does not matter", scopes: []Scope{ScopeAll, ScopeOwn, ScopeTenant}, want: ScopeAll},
		{name: "duplicates", scopes: []Scope{ScopeTeam, ScopeTeam}, want: ScopeTeam},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := MaxScope(testCase.scopes...)
			if err != nil {
				t.Fatalf("MaxScope: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("MaxScope(%v) = %q, want %q", testCase.scopes, got, testCase.want)
			}
		})
	}
	if _, err := MaxScope(); err == nil {
		t.Fatal("MaxScope() with no scopes succeeded, want an error")
	}
	if _, err := MaxScope(ScopeOwn, "world"); err == nil {
		t.Fatal("MaxScope with an invalid scope succeeded, want an error")
	}
}

func TestSortScopes(t *testing.T) {
	input := []Scope{ScopeAll, ScopeOwn, ScopeTeam, ScopeTenant}
	got := SortScopes(input)
	want := []Scope{ScopeOwn, ScopeTeam, ScopeTenant, ScopeAll}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("SortScopes(%v) = %v, want %v", input, got, want)
		}
	}
	if input[0] != ScopeAll {
		t.Fatalf("SortScopes mutated its input: %v", input)
	}
}

func TestInvalidScopeNeverCovers(t *testing.T) {
	if Scope("world").Covers(ScopeOwn) {
		t.Fatal("invalid scope covers own")
	}
	if ScopeOwn.Covers(Scope("world")) {
		t.Fatal("own covers an invalid scope")
	}
	if CompareScopes(Scope("world"), ScopeOwn) <= 0 {
		t.Fatal("invalid scope should sort after valid scopes")
	}
}
