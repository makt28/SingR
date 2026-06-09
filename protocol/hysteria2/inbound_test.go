package hysteria2

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing/common/json/badoption"
)

func TestConfigureFromPanelNodeConcurrentReloadAndUsers(t *testing.T) {
	certPEM, keyPEM := testSelfSignedCertificate(t)
	portA := testFreeUDPPort(t)
	portB := testFreeUDPPort(t)
	listenAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))

	inbound, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "hysteria2-in", option.Hysteria2InboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     &listenAddr,
			ListenPort: uint16(portA),
		},
		InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
			TLS: &option.InboundTLSOptions{
				Enabled:     true,
				ServerName:  "initial.example.com",
				Certificate: badoption.Listable[string]{certPEM},
				Key:         badoption.Listable[string]{keyPEM},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := inbound.(*Inbound)
	if err := h.Start(adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	start := make(chan struct{})
	errs := make(chan error, 64)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		ports := []uint32{uint32(portB), uint32(portA)}
		hosts := []string{"reload-a.example.com", "reload-b.example.com"}
		for i := 0; i < 40; i++ {
			if err := h.ConfigureFromPanelNode(&api.NodeInfo{
				NodeType: C.TypeHysteria2,
				Port:     ports[i%len(ports)],
				Host:     hosts[i%len(hosts)],
			}); err != nil {
				errs <- err
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 80; i++ {
			users := []api.UserInfo{
				{UID: 1001, UUID: "uuid-1001", Passwd: "passwd-1001"},
				{UID: 1002, UUID: "uuid-1002", Passwd: "passwd-1002"},
				{UID: 1003, UUID: "uuid-1003", Passwd: "passwd-1003"},
			}
			switch i % 3 {
			case 0:
				if err := h.AddUsers(&users, &api.NodeInfo{NodeType: C.TypeHysteria2}); err != nil {
					errs <- err
					return
				}
			case 1:
				if err := h.RemoveUsers([]string{"u1002"}); err != nil {
					errs <- err
					return
				}
			default:
				users[0].UUID = "uuid-1001-rotated"
				if err := h.RefreshUsers(&users, &api.NodeInfo{NodeType: C.TypeHysteria2}); err != nil {
					errs <- err
					return
				}
			}
		}
	}()

	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func testFreeUDPPort(t *testing.T) int {
	t.Helper()
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	return packetConn.LocalAddr().(*net.UDPAddr).Port
}

func testSelfSignedCertificate(t *testing.T) (string, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "test.example.com",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
		DNSNames:  []string{"initial.example.com", "reload-a.example.com", "reload-b.example.com"},
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(privateKey)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})
	return string(certPEM), string(keyPEM)
}
