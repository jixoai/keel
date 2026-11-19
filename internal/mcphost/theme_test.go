package mcphost

import (
	"encoding/json"
	"testing"
)

const themeFixture = `{
	"name": "Keel Test",
	"cssVars": {
		"theme": {
			"radius": "12px",
			"radius-card": "16px",
			"radius-control": "6px",
			"radius-pill": "999px",
			"font-sans": "system-ui, sans-serif",
			"font-mono": "ui-monospace"
		},
		"light": {
			"card": "#ffffff",
			"panel": "#f5f5f5",
			"muted": "rgba(0,0,0,0.04)",
			"foreground": "#111111",
			"muted-foreground": "#666666",
			"border": "rgba(0,0,0,0.1)",
			"header-border": "rgba(0,0,0,0.08)",
			"destructive": "#dc2626",
			"accent": "#f0f9ff",
			"ring": "rgba(0,0,0,0.2)",
			"glass-edge": "rgba(0,0,0,0.1)"
		},
		"dark": {
			"card": "#111111",
			"panel": "#1a1a1a",
			"foreground": "#fafafa",
			"muted-foreground": "#999999",
			"border": "rgba(255,255,255,0.1)",
			"destructive": "#f87171",
			"success": "#22c55e",
			"warning": "#f59e0b",
			"accent": "#0ea5e9",
			"ring": "rgba(255,255,255,0.2)"
		}
	}
}`

// TestThemeVariablesMapping pins the design §3.4 mapping table for both modes.
func TestThemeVariablesMapping(t *testing.T) {
	light := ThemeVariables(json.RawMessage(themeFixture), false)
	if light == nil {
		t.Fatal("light palette is nil")
	}
	wantLight := map[string]string{
		"--color-background-primary":   "#ffffff",
		"--color-background-secondary": "#f5f5f5",
		"--color-background-tertiary":  "rgba(0,0,0,0.04)",
		"--color-text-primary":         "#111111",
		"--color-text-secondary":       "#666666",
		"--color-border-primary":       "rgba(0,0,0,0.1)",
		"--color-border-secondary":     "rgba(0,0,0,0.08)",
		"--color-background-danger":    "#dc2626",
		// No success token in light: accent is the documented fallback.
		"--color-background-success": "#f0f9ff",
		"--color-background-warning": "#f0f9ff",
		"--color-ring-primary":       "rgba(0,0,0,0.2)",
		"--font-sans":                "system-ui, sans-serif",
		"--font-mono":                "ui-monospace",
		"--border-radius-sm":         "6px",
		"--border-radius-md":         "12px",
		"--border-radius-lg":         "16px",
		"--border-radius-full":       "999px",
		"--shadow-hairline":          "rgba(0,0,0,0.1)",
	}
	for variable, want := range wantLight {
		if got := light[variable]; got != want {
			t.Errorf("light %s = %q, want %q", variable, got, want)
		}
	}

	dark := ThemeVariables(json.RawMessage(themeFixture), true)
	wantDark := map[string]string{
		"--color-background-primary": "#111111",
		"--color-text-primary":       "#fafafa",
		"--color-background-success": "#22c55e",
		"--color-background-warning": "#f59e0b",
	}
	for variable, want := range wantDark {
		if got := dark[variable]; got != want {
			t.Errorf("dark %s = %q, want %q", variable, got, want)
		}
	}
	// A dark palette without muted/header-border: the variable is omitted, never
	// filled with an invented value.
	if _, present := dark["--color-background-tertiary"]; present {
		t.Fatal("a missing token produced a value")
	}
	if _, present := dark["--color-border-secondary"]; present {
		t.Fatal("a missing token produced a value")
	}
	if _, present := dark["--shadow-hairline"]; present {
		t.Fatal("a missing glass-edge token produced a value")
	}
	if dark["--border-radius-md"] != "12px" {
		t.Fatalf("the shared theme block was not consulted: %q", dark["--border-radius-md"])
	}
}

// TestThemeVariablesUnusableDocument pins the nil contract: an unusable
// document maps to nothing.
func TestThemeVariablesUnusableDocument(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`"nope"`), json.RawMessage(`[]`), json.RawMessage(`{}`), json.RawMessage(`{"cssVars": 3}`)} {
		if got := ThemeVariables(raw, false); got != nil {
			t.Errorf("%s: got %v, want nil", raw, got)
		}
		if got := ThemePalettes(raw); got != nil {
			t.Errorf("%s: palettes = %v, want nil", raw, got)
		}
	}
}

// TestThemePalettes carries both modes, the shape the app catalogue serves.
func TestThemePalettes(t *testing.T) {
	palettes := ThemePalettes(json.RawMessage(themeFixture))
	if palettes == nil || palettes["light"] == nil || palettes["dark"] == nil {
		t.Fatalf("palettes = %+v", palettes)
	}
	if palettes["light"]["--color-text-primary"] != "#111111" || palettes["dark"]["--color-text-primary"] != "#fafafa" {
		t.Fatalf("palettes crossed: %+v", palettes)
	}
}
