package pluginpkg

import "testing"

func TestParseSemVerAndCompare(t *testing.T) {
	cases := []struct {
		raw    string
		ok     bool
		equals string
	}{
		{"1.2.3", true, "1.2.3"},
		{"0.0.0", true, "0.0.0"},
		{"1.2.3-rc.1", true, "1.2.3-rc.1"},
		{"1.2.3+build.7", true, "1.2.3+build.7"},
		{"1.2", false, ""},
		{"1.2.3.4", false, ""},
		{"01.2.3", false, ""},
		{"1.2.x", false, ""},
		{"", false, ""},
		{" 1.2.3", false, ""},
	}
	for _, tc := range cases {
		version, err := ParseSemVer(tc.raw)
		if tc.ok != (err == nil) {
			t.Fatalf("ParseSemVer(%q) err = %v", tc.raw, err)
		}
		if tc.ok && version.String() != tc.equals {
			t.Fatalf("ParseSemVer(%q).String() = %q", tc.raw, version.String())
		}
	}
	order := []string{"0.1.0", "0.1.1", "0.2.0-rc.1", "0.2.0", "1.0.0-alpha", "1.0.0"}
	for i := 0; i+1 < len(order); i++ {
		left, _ := ParseSemVer(order[i])
		right, _ := ParseSemVer(order[i+1])
		if left.Compare(right) >= 0 {
			t.Fatalf("%s should sort before %s", order[i], order[i+1])
		}
	}
	pre, _ := ParseSemVer("1.0.0-rc.2")
	older, _ := ParseSemVer("1.0.0-rc.1")
	if older.Compare(pre) >= 0 {
		t.Fatal("rc.1 must sort before rc.2")
	}
}

func TestVersionRangeMatching(t *testing.T) {
	cases := []struct {
		expr     string
		version  string
		contains bool
	}{
		{"", "1.2.3", true},
		{"*", "1.2.3", true},
		{">=0.2.0 <0.3.0", "0.2.5", true},
		{">=0.2.0 <0.3.0", "0.3.0", false},
		{">=0.2.0, <0.3.0", "0.1.9", false},
		{">=0.1.0", "1.0.0", true},
		{"=0.1.0", "0.1.0", true},
		{"=0.1.0", "0.1.1", false},
		{"^0.1.0", "0.1.9", true},
		{"^0.1.0", "0.2.0", false},
		{"^1.2.0", "1.9.0", true},
		{"^1.2.0", "2.0.0", false},
		{"~0.1.2", "0.1.9", true},
		{"~0.1.2", "0.2.0", false},
		{">=0.2.0", "0.3.0-rc.1", false},
		{">=0.3.0-0", "0.3.0-rc.1", true},
	}
	for _, tc := range cases {
		rng, err := ParseVersionRange(tc.expr)
		if err != nil {
			t.Fatalf("ParseVersionRange(%q): %v", tc.expr, err)
		}
		version, err := ParseSemVer(tc.version)
		if err != nil {
			t.Fatal(err)
		}
		if got := rng.Contains(version); got != tc.contains {
			t.Fatalf("range %q contains %s = %v, want %v", tc.expr, tc.version, got, tc.contains)
		}
	}
	for _, bad := range []string{"nope", ">=0.1", ">=x.y.z", "<<1.0.0"} {
		if _, err := ParseVersionRange(bad); err == nil {
			t.Fatalf("range %q must be rejected", bad)
		}
	}
}
