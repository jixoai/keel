// Tests ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi), packages/mcp/test/naming.test.ts
// (MIT License). Each subtest keeps the title of the pi test it ports.

package mcp

import (
	"regexp"
	"strings"
	"testing"
)

func TestCreateToolName(t *testing.T) {
	t.Run("joins server and tool and replaces characters outside [A-Za-z0-9_]", func(t *testing.T) {
		tests := []struct {
			server, tool, want string
		}{
			{server: "docs", tool: "search", want: "mcp__docs__search"},
			{server: "my-server", tool: "get.item/v2", want: "mcp__my_server__get_item_v2"},
			// Supplementary: every character outside the provider set is
			// replaced, including spaces and non-ASCII runes.
			{server: "my server", tool: "a b", want: "mcp__my_server__a_b"},
			{server: "sérver", tool: "t", want: "mcp__s_rver__t"},
			// pi's regex works on UTF-16 code units, so an astral rune
			// becomes two underscores.
			{server: "s", tool: "😀", want: "mcp__s____"},
			{server: "", tool: "", want: "mcp____"},
		}
		for _, test := range tests {
			if got := CreateToolName(test.server, test.tool, nil); got != test.want {
				t.Errorf("CreateToolName(%q, %q) = %q, want %q", test.server, test.tool, got, test.want)
			}
		}
	})

	t.Run("shortens names over 64 characters with a hash of server and tool", func(t *testing.T) {
		longTool := strings.Repeat("x", 100)
		long := CreateToolName("server", longTool, nil)
		if len(long) != MaxToolNameLength {
			t.Fatalf("len(CreateToolName(server, x*100)) = %d, want %d (%q)", len(long), MaxToolNameLength, long)
		}
		if !regexp.MustCompile(`^mcp__server__x+_[0-9a-f]{8}$`).MatchString(long) {
			t.Errorf("long name = %q, want mcp__server__x+_<8 hex>", long)
		}
		// Names that share their first 55 characters still differ.
		if other := CreateToolName("server", longTool+"y", nil); other == long {
			t.Errorf("CreateToolName(server, x*100+y) = %q, want a different name", other)
		}
		// The hash covers the unsanitized names, and is stable.
		if again := CreateToolName("server", longTool, nil); again != long {
			t.Errorf("CreateToolName(server, x*100) = %q on second call, want %q", again, long)
		}
		if CreateToolName("ser-ver", longTool, nil) == CreateToolName("ser_ver", longTool, nil) {
			t.Error("names hashed from ser-ver and ser_ver are equal, want different")
		}
		// Supplementary: a name of exactly the limit is kept as it is.
		exact := CreateToolName("s", strings.Repeat("x", MaxToolNameLength-len("mcp__s__")), nil)
		if len(exact) != MaxToolNameLength {
			t.Fatalf("len(exact-limit name) = %d, want %d", len(exact), MaxToolNameLength)
		}
		if regexp.MustCompile(`_[0-9a-f]{8}$`).MatchString(exact) {
			t.Errorf("exact-limit name = %q, want no hash suffix", exact)
		}
	})

	t.Run("adds the hash suffix when the sanitized name is taken", func(t *testing.T) {
		taken := CreateToolName("s", "a_b", nil)
		if taken != "mcp__s__a_b" {
			t.Fatalf("CreateToolName(s, a_b) = %q, want mcp__s__a_b", taken)
		}
		isTaken := func(name string) bool { return name == taken }
		second := CreateToolName("s", "a-b", isTaken)
		if !regexp.MustCompile(`^mcp__s__a_b_[0-9a-f]{8}$`).MatchString(second) {
			t.Errorf("CreateToolName(s, a-b) with a_b taken = %q, want mcp__s__a_b_<8 hex>", second)
		}
		if third := CreateToolName("s", "a_b", isTaken); third == second {
			t.Errorf("CreateToolName(s, a_b) with a_b taken = %q, want a different name than %q", third, second)
		}
	})
}

func TestNamespace(t *testing.T) {
	t.Run("is mcp__<server> with - replaced by _, the prefix of the server's tool names", func(t *testing.T) {
		if got, want := Namespace("docs"), "mcp__docs"; got != want {
			t.Errorf("Namespace(docs) = %q, want %q", got, want)
		}
		if got, want := Namespace("my-server"), "mcp__my_server"; got != want {
			t.Errorf("Namespace(my-server) = %q, want %q", got, want)
		}
		if Namespace("my-server") != Namespace("my_server") {
			t.Error("Namespace(my-server) != Namespace(my_server), want equal")
		}
		prefix := Namespace("my-server") + "__"
		if name := CreateToolName("my-server", "search", nil); !strings.HasPrefix(name, prefix) {
			t.Errorf("CreateToolName(my-server, search) = %q, want prefix %q", name, prefix)
		}
	})
}
