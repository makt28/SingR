package api

import (
	"testing"

	"github.com/sagernet/sing-box/option"
)

func TestValidatePanelNode(t *testing.T) {
	if err := ValidatePanelNode("anytls", &NodeInfo{NodeType: "anytls", Port: 443}); err != nil {
		t.Fatalf("valid node rejected: %v", err)
	}
	if err := ValidatePanelNode("anytls", &NodeInfo{NodeType: "hysteria2", Port: 443}); err == nil {
		t.Fatal("node of another type accepted")
	}
	for _, port := range []uint32{0, 65536, 1 << 20} {
		if err := ValidatePanelNode("anytls", &NodeInfo{NodeType: "anytls", Port: port}); err == nil {
			t.Fatalf("port %d accepted", port)
		}
	}
}

func TestPlanListenUpdate(t *testing.T) {
	current := &option.InboundTLSOptions{Enabled: true, ServerName: "old.example.com", CertificatePath: "/c.pem"}

	t.Run("nothing changes", func(t *testing.T) {
		update := PlanListenUpdate(&NodeInfo{Port: 443, Host: "old.example.com"}, 443, current)
		if update.Changed() || update.Port != 443 || update.SNI != "old.example.com" {
			t.Fatalf("update = %+v", update)
		}
	})

	t.Run("port only", func(t *testing.T) {
		update := PlanListenUpdate(&NodeInfo{Port: 8443, Host: "old.example.com"}, 443, current)
		if !update.PortChanged || update.SNIChanged || update.Port != 8443 {
			t.Fatalf("update = %+v", update)
		}
	})

	t.Run("host replaces the server name on a copy", func(t *testing.T) {
		update := PlanListenUpdate(&NodeInfo{Port: 443, Host: "new.example.com"}, 443, current)
		if update.PortChanged || !update.SNIChanged || update.SNI != "new.example.com" {
			t.Fatalf("update = %+v", update)
		}
		if update.TLS == current {
			t.Fatal("TLS options were not copied")
		}
		if current.ServerName != "old.example.com" {
			t.Fatal("current TLS options were modified in place")
		}
		if update.TLS.CertificatePath != "/c.pem" || !update.TLS.Enabled {
			t.Fatalf("copy lost fields: %+v", update.TLS)
		}
	})

	t.Run("empty host keeps the configured server name", func(t *testing.T) {
		update := PlanListenUpdate(&NodeInfo{Port: 443}, 443, current)
		if update.Changed() || update.TLS != current || update.SNI != "old.example.com" {
			t.Fatalf("update = %+v", update)
		}
	})

	t.Run("host without a TLS block enables TLS", func(t *testing.T) {
		update := PlanListenUpdate(&NodeInfo{Port: 443, Host: "new.example.com"}, 443, nil)
		if update.TLS == nil || !update.TLS.Enabled || update.TLS.ServerName != "new.example.com" || !update.SNIChanged {
			t.Fatalf("update = %+v", update)
		}
	})

	t.Run("no host and no TLS block", func(t *testing.T) {
		update := PlanListenUpdate(&NodeInfo{Port: 443}, 443, nil)
		if update.TLS != nil || update.SNI != "" || update.Changed() {
			t.Fatalf("update = %+v", update)
		}
	})
}
