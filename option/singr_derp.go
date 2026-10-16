package option

import "github.com/sagernet/sing/common/json/badoption"

// SingRDERPOptions are SingR-only DERP service options (not in upstream),
// embedded in DERPServiceOptions so they sit flat next to the upstream
// fields. Kept in their own file so a core resync only conflicts on the one
// embedding line in option/tailscale.go.
type SingRDERPOptions struct {
	// VerifyClientLoopback admits ordinary DERP clients only when they come
	// from the loopback address. Mesh peers are unaffected (they authenticate
	// with the mesh PSK). Clients reach the server through the proxy tunnel: a
	// route rule rewrites the virtual DERP domain to 127.0.0.1, so only users
	// with a valid subscription can use it and the bytes are billed as normal
	// proxy traffic. Direct connections from the internet are refused.
	//
	// Mutually exclusive with verify_client_url / verify_client_endpoint:
	// upstream admits a client when any one verifier allows it, which would
	// silently turn the loopback restriction into a no-op.
	VerifyClientLoopback bool `json:"verify_client_loopback,omitempty"`

	// ControlURL is the Linbei headscale fork (https). The node registers its
	// DERP port and certificate pin there and gets back the mesh PSK and every
	// other relay. See service/derp/singr_mesh.go.
	ControlURL string `json:"control_url,omitempty"`

	// ControlTokenPath holds the node token (not a headscale API key).
	ControlTokenPath string `json:"control_token_path,omitempty"`

	// ControlAdvertiseHost overrides the address other relays dial; empty
	// means the control server uses the address it sees the request from.
	ControlAdvertiseHost string `json:"control_advertise_host,omitempty"`

	// ControlInterval is the register/refresh interval, 5 minutes by default.
	ControlInterval badoption.Duration `json:"control_interval,omitempty"`

	// ProbeListenPort serves a plain-HTTP 204 on 127.0.0.1 so clients can
	// probe, through each proxy node, whether that node has a relay (the DERP
	// port is TLS with a self-signed certificate, which a URL test cannot
	// verify). Zero disables it.
	ProbeListenPort uint16 `json:"probe_listen_port,omitempty"`
}
