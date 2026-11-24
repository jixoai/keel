package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTarRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	err := extractTar(bytes.NewReader(buf.Bytes()), dst)
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("extractTar error = %v; want an unsafe-path refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "..", "escape")); statErr == nil {
		t.Fatal("extractTar wrote outside the destination")
	}
}

func TestInfoPlistDeclaresSchemeAndLauncher(t *testing.T) {
	plist := infoPlist()
	for _, want := range []string{"<string>Keel</string>", "<string>keel</string>", "CFBundleURLSchemes"} {
		if !strings.Contains(plist, want) {
			t.Fatalf("Info.plist is missing %q", want)
		}
	}
}

func TestLocalTenantPersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KEEL_HOME", home)
	first, err := localTenant()
	if err != nil {
		t.Fatal(err)
	}
	second, err := localTenant()
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 36 {
		t.Fatalf("tenant = %q then %q; want one stable UUID", first, second)
	}
	if info, err := os.Stat(filepath.Join(home, "tenant")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("tenant file is accessible to group/other: %v", info.Mode())
	}
}
