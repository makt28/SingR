package option

import "github.com/sagernet/sing/common/json/badoption"

// SingRDERPOptions are SingR-only DERP service options (not in upstream),
// embedded in DERPServiceOptions so they sit flat next to the upstream
// fields. Kept in their own file so a core resync only conflicts on the one
// embedding line in option/tailscale.go.
type SingRDERPOptions struct {
	// VerifyClientLoopback admits ordinary DERP clients only when they come
	// from the loopback address. Mesh peers are unaffected (they authenticate
	// with mesh_psk). Clients reach the server through the proxy tunnel: a
	// route rule rewrites the virtual DERP domain to 127.0.0.1, so only users
	// with a valid subscription can use it and the bytes are billed as normal
	// proxy traffic. Direct connections from the internet are refused.
	//
	// Mutually exclusive with verify_client_url / verify_client_endpoint:
	// upstream admits a client when any one verifier allows it, which would
	// silently turn the loopback restriction into a no-op.
	VerifyClientLoopback bool `json:"verify_client_loopback,omitempty"`

	// MeshWithURL is a remote mesh peer list (https only), polled while
	// running; peers are added and removed without restarting the process.
	// Format: see parseMeshPeerList in service/derp/singr_mesh.go.
	MeshWithURL string `json:"mesh_with_url,omitempty"`

	// MeshWithURLInterval is the poll interval, 5 minutes by default.
	MeshWithURLInterval badoption.Duration `json:"mesh_with_url_interval,omitempty"`
}
