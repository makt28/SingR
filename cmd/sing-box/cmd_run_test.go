package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sagernet/sing-box/option"
)

// TestFilterInboundsByPanel covers the SingR-specific superset behavior:
// a single server.json may declare both anytls-in and hysteria2-in, and
// only the inbounds referenced by panel.json's InTag are kept.
func TestFilterInboundsByPanel(t *testing.T) {
	dir := t.TempDir()
	panelPath := filepath.Join(dir, "panel.json")
	if err := os.WriteFile(panelPath, []byte(
		`{"name":"t","nodes":[{"intag":"anytls-in","outtag":"anytls-out","apiconfig":{"nodeid":1}}]}`,
	), 0o644); err != nil {
		t.Fatal(err)
	}

	base := option.Options{}
	base.Inbounds = []option.Inbound{
		{Type: "anytls", Tag: "anytls-in"},
		{Type: "hysteria2", Tag: "hysteria2-in"},
	}

	saved := poetConfigPath
	t.Cleanup(func() { poetConfigPath = saved })

	// No -p → vanilla behavior, all inbounds kept.
	poetConfigPath = ""
	if got := filterInboundsByPanel(base); len(got.Inbounds) != 2 {
		t.Fatalf("no -p: want 2 inbounds, got %d", len(got.Inbounds))
	}

	// -p referencing only anytls-in → hysteria2-in dropped.
	poetConfigPath = panelPath
	got := filterInboundsByPanel(base)
	if len(got.Inbounds) != 1 || got.Inbounds[0].Tag != "anytls-in" {
		t.Fatalf("with -p: want [anytls-in], got %+v", got.Inbounds)
	}

	// Unreadable panel path → best-effort keep all.
	poetConfigPath = filepath.Join(dir, "does-not-exist.json")
	if got := filterInboundsByPanel(base); len(got.Inbounds) != 2 {
		t.Fatalf("missing panel: want 2 inbounds (best-effort), got %d", len(got.Inbounds))
	}
}
