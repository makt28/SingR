package naive

import (
	stdtls "crypto/tls"
	"io"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
)

// Test hooks for the external naive_test package, which exists because only
// an external test can import protocol/naive/quic (it imports this package)
// to register the HTTP/3 listener.

var BuildNaiveUsers = buildNaiveUsers

func (n *Inbound) SetRouterForTest(router adapter.ConnectionRouterEx) {
	n.router = router
}

func (n *Inbound) OptionsForTest() option.NaiveInboundOptions {
	n.reloadMu.Lock()
	defer n.reloadMu.Unlock()
	return n.options
}

// ComponentsForTest returns the live listener, HTTP/3 closer and TLS config
// as opaque values for identity comparisons.
func (n *Inbound) ComponentsForTest() (listener any, h3Server io.Closer, tlsConfig any) {
	n.reloadMu.Lock()
	defer n.reloadMu.Unlock()
	return n.listener, n.h3Server, n.currentTLS()
}

// QUICConfigForTest resolves what the HTTP/3 listener hands to a handshake.
func (n *Inbound) QUICConfigForTest() (*stdtls.Config, error) {
	return n.tlsSwitch.getConfigForClient(&stdtls.ClientHelloInfo{})
}

// CurrentSTDConfigForTest is the current TLS config's resolved standard config.
func (n *Inbound) CurrentSTDConfigForTest() (*stdtls.Config, error) {
	config, err := n.currentTLS().STDConfig()
	if err != nil {
		return nil, err
	}
	return config.GetConfigForClient(&stdtls.ClientHelloInfo{})
}

// SwapTLSForTest replaces the live TLS config the way an SNI reload does,
// but with arbitrary options (e.g. a different certificate) so a test can
// tell from the client side which config a handshake used.
func (n *Inbound) SwapTLSForTest(tlsOptions option.InboundTLSOptions) error {
	n.reloadMu.Lock()
	defer n.reloadMu.Unlock()
	options := n.options
	options.TLS = &tlsOptions
	tlsConfig, err := newNaiveTLS(n.ctx, n.logger, options, n.network)
	if err != nil {
		return err
	}
	if err = tlsConfig.Start(); err != nil {
		return err
	}
	n.tlsMu.Lock()
	oldTLS := n.tlsConfig
	n.tlsConfig = tlsConfig
	n.tlsMu.Unlock()
	return common.Close(oldTLS)
}
