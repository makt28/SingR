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

// Regression guard for the SingR default-certificate resolution in
// applyDefaultCertificatePaths. The shell side (SingR.sh / SingR-docker.sh
// node_default_cert_path, docker-entrypoint.sh default_cert_path) mirrors these
// rules; if this test changes, those must change too.

func tlsInbound(tag string, tlsOptions *option.InboundTLSOptions) option.Inbound {
	return option.Inbound{
		Type: "anytls",
		Tag:  tag,
		Options: &option.AnyTLSInboundOptions{
			InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{TLS: tlsOptions},
		},
	}
}

func applyWithPanelDir(t *testing.T, dir string, inbounds ...option.Inbound) []option.Inbound {
	t.Helper()
	previous := poetConfigPath
	poetConfigPath = filepath.Join(dir, "panel.json")
	defer func() { poetConfigPath = previous }()
	return applyDefaultCertificatePaths(option.Options{Inbounds: inbounds}).Inbounds
}

func inboundTLS(t *testing.T, inbound option.Inbound) *option.InboundTLSOptions {
	t.Helper()
	wrapper, withTLS := inbound.Options.(option.InboundTLSOptionsWrapper)
	if !withTLS {
		t.Fatal("inbound has no TLS options")
	}
	return wrapper.TakeInboundTLSOptions()
}

func writeCertFile(t *testing.T, dir string, name string) string {
	t.Helper()
	certDirectory := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(certDirectory, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestApplyDefaultCertificatePaths(t *testing.T) {
	t.Run("PrefersPEM", func(t *testing.T) {
		dir := t.TempDir()
		writeCertFile(t, dir, "default.pem")
		writeCertFile(t, dir, "default.crt")
		inbounds := applyWithPanelDir(t, dir, tlsInbound("anytls-in", &option.InboundTLSOptions{Enabled: true}))
		tlsOptions := inboundTLS(t, inbounds[0])
		if want := filepath.Join(dir, "certs", "default.pem"); tlsOptions.CertificatePath != want {
			t.Fatalf("certificate_path = %q, want %q", tlsOptions.CertificatePath, want)
		}
		if want := filepath.Join(dir, "certs", "default.key"); tlsOptions.KeyPath != want {
			t.Fatalf("key_path = %q, want %q", tlsOptions.KeyPath, want)
		}
	})

	t.Run("FallsBackToCRT", func(t *testing.T) {
		dir := t.TempDir()
		writeCertFile(t, dir, "default.crt")
		inbounds := applyWithPanelDir(t, dir, tlsInbound("anytls-in", &option.InboundTLSOptions{Enabled: true}))
		if want := filepath.Join(dir, "certs", "default.crt"); inboundTLS(t, inbounds[0]).CertificatePath != want {
			t.Fatalf("certificate_path = %q, want %q", inboundTLS(t, inbounds[0]).CertificatePath, want)
		}
	})

	// Nothing on disk: still write the primary candidate, so the startup error
	// names a concrete file instead of the generic "missing certificate".
	t.Run("NamesPrimaryCandidateWhenAbsent", func(t *testing.T) {
		dir := t.TempDir()
		inbounds := applyWithPanelDir(t, dir, tlsInbound("anytls-in", &option.InboundTLSOptions{Enabled: true}))
		if want := filepath.Join(dir, "certs", "default.pem"); inboundTLS(t, inbounds[0]).CertificatePath != want {
			t.Fatalf("certificate_path = %q, want %q", inboundTLS(t, inbounds[0]).CertificatePath, want)
		}
	})

	t.Run("LeavesConfiguredAlone", func(t *testing.T) {
		dir := t.TempDir()
		writeCertFile(t, dir, "default.pem")
		for name, tlsOptions := range map[string]*option.InboundTLSOptions{
			"certificate_path": {Enabled: true, CertificatePath: "/somewhere/cert.pem"},
			"key_path":         {Enabled: true, KeyPath: "/somewhere/key.pem"},
			"certificate":      {Enabled: true, Certificate: []string{"-----BEGIN CERTIFICATE-----"}},
			"acme":             {Enabled: true, ACME: &option.InboundACMEOptions{}},
			"provider":         {Enabled: true, CertificateProvider: &option.CertificateProviderOptions{}},
			"reality":          {Enabled: true, Reality: &option.InboundRealityOptions{Enabled: true}},
			"insecure":         {Enabled: true, Insecure: true},
			"disabled":         {},
		} {
			certificatePath := tlsOptions.CertificatePath
			keyPath := tlsOptions.KeyPath
			inbounds := applyWithPanelDir(t, dir, tlsInbound("anytls-in", tlsOptions))
			got := inboundTLS(t, inbounds[0])
			if got.CertificatePath != certificatePath || got.KeyPath != keyPath {
				t.Fatalf("%s: rewritten to %q/%q, want %q/%q", name, got.CertificatePath, got.KeyPath, certificatePath, keyPath)
			}
		}
	})

	// Gated on -p, exactly like filterInboundsByPanel: without a panel config
	// this is vanilla sing-box and must not grow implicit paths.
	t.Run("NoPanelConfigIsNoOp", func(t *testing.T) {
		previous := poetConfigPath
		poetConfigPath = ""
		defer func() { poetConfigPath = previous }()
		inbound := tlsInbound("anytls-in", &option.InboundTLSOptions{Enabled: true})
		inbounds := applyDefaultCertificatePaths(option.Options{Inbounds: []option.Inbound{inbound}}).Inbounds
		if got := inboundTLS(t, inbounds[0]); got.CertificatePath != "" || got.KeyPath != "" {
			t.Fatalf("rewritten without -p: %q / %q", got.CertificatePath, got.KeyPath)
		}
	})
}
