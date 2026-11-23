package assembly_test

import (
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/assembly"
)

// TestParseConfigReadsTheCaptureSetting pins the file spelling of the one
// setting the config.toml loader carries: [filesystem.capture] mode. The
// setting is part of the content hash and independent of row order/whitespace.
func TestParseConfigReadsTheCaptureSetting(t *testing.T) {
	config := mustParseConfig(t, `
version = 1

[filesystem.capture]
mode = "checkpoint"

[[rows]]
id = "keel.shell#bash-local"
impl = "ssh"
`)
	mode, ok := config.Settings.Get(assembly.SettingFilesystemCaptureMode)
	if !ok || mode != "checkpoint" {
		t.Fatalf("capture setting = %q/%v", mode, ok)
	}
	if config.Settings.Empty() {
		t.Fatal("settings reported empty")
	}

	same := mustParseConfig(t, `
# the settings table may precede or follow rows
version = 1
[[rows]]
id="keel.shell#bash-local"
impl="ssh"
[filesystem.capture]
mode='checkpoint'
`)
	if config.Hash() != same.Hash() {
		t.Fatalf("canonical hash differs:\n%s\n%s", config.Hash(), same.Hash())
	}

	changed := mustParseConfig(t, `
version = 1
[filesystem.capture]
mode = "manual"
`)
	if changed.Hash() == config.Hash() {
		t.Fatal("a changed capture mode must change the config hash")
	}
	if got := mustParseConfig(t, "version = 1\n").Hash(); got == changed.Hash() {
		t.Fatal("the setting must participate in the hash, not be ignored")
	}
}

func TestParseConfigRejectsMalformedCaptureSettings(t *testing.T) {
	cases := []struct {
		name   string
		source string
		code   string
	}{
		{
			name:   "unknown parent table",
			source: "version = 1\n[filesystem]\nmode = \"manual\"\n",
			code:   assembly.IssueConfigTable,
		},
		{
			name:   "unknown setting key",
			source: "version = 1\n[filesystem.capture]\nwhen = \"manual\"\n",
			code:   assembly.IssueConfigKey,
		},
		{
			name:   "duplicate mode",
			source: "version = 1\n[filesystem.capture]\nmode = \"manual\"\nmode = \"checkpoint\"\n",
			code:   assembly.IssueConfigDuplicate,
		},
		{
			name:   "duplicate table",
			source: "version = 1\n[filesystem.capture]\nmode = \"manual\"\n[filesystem.capture]\nmode = \"checkpoint\"\n",
			code:   assembly.IssueConfigDuplicate,
		},
		{
			name:   "mode is not a string",
			source: "version = 1\n[filesystem.capture]\nmode = 7\n",
			code:   assembly.IssueConfigValue,
		},
		{
			name:   "empty mode",
			source: "version = 1\n[filesystem.capture]\nmode = \"\"\n",
			code:   assembly.IssueConfigValue,
		},
		{
			name:   "unknown mode",
			source: "version = 1\n[filesystem.capture]\nmode = \"every-now-and-then\"\n",
			code:   assembly.IssueConfigValue,
		},
		{
			name:   "padded mode",
			source: "version = 1\n[filesystem.capture]\nmode = \" manual \"\n",
			code:   assembly.IssueConfigValue,
		},
		{
			name:   "arrays are outside the subset",
			source: "version = 1\n[filesystem.capture]\nmode = [\"manual\"]\n",
			code:   assembly.IssueConfigValue,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			issues := configIssues(t, test.source)
			if !issues.HasCode(test.code) {
				t.Fatalf("codes = %v, want %s\n%s", issues.Codes(), test.code, issues.Error())
			}
		})
	}
}

// TestValidateRejectsHandBuiltSettings keeps the shape validator honest for a
// caller that builds Config in Go instead of parsing a file.
func TestValidateRejectsHandBuiltSettings(t *testing.T) {
	config := assembly.Config{Version: assembly.ConfigFormatVersion}
	config.Settings.Values = map[string]string{"filesystem.capture.mode": "bogus"}
	err := config.Validate()
	if err == nil || !strings.Contains(err.Error(), "per-tool-call|checkpoint|manual") {
		t.Fatalf("invalid capture setting = %v", err)
	}

	config.Settings.Values = map[string]string{"filesystem.capture": "manual"}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "unknown config setting") {
		t.Fatalf("unknown setting = %v", err)
	}

	config.Settings.Values = map[string]string{"filesystem.capture.mode": "manual"}
	if err := config.Validate(); err != nil {
		t.Fatalf("valid capture setting rejected: %v", err)
	}
}
