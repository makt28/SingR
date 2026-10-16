package option

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

	// RegistryURL is sspapi's tailnet.php (https). Every minute the node
	// reports its DERP port, certificate pin and its own public IPv4 / IPv6
	// there and gets back the mesh PSK and every other relay. See
	// service/derp/singr_mesh.go.
	RegistryURL string `json:"registry_url,omitempty"`

	// RegistryTokenPath holds the node token (derp.node_token in sspapi's
	// data/tailnet.json).
	RegistryTokenPath string `json:"registry_token_path,omitempty"`

	// AdvertiseIPv4 / AdvertiseIPv6: empty = detect (the source address the
	// kernel would use, checked every 5 s; behind NAT, what ip.sb says once at
	// startup); an address = always report that one; "none" = never report
	// this family.
	AdvertiseIPv4 string `json:"advertise_ipv4,omitempty"`
	AdvertiseIPv6 string `json:"advertise_ipv6,omitempty"`

	// ProbeListenPort serves a plain-HTTP 204 on 127.0.0.1 so clients can
	// probe, through each proxy node, whether that node has a relay (the DERP
	// port is TLS with a self-signed certificate, which a URL test cannot
	// verify). Zero disables it.
	ProbeListenPort uint16 `json:"probe_listen_port,omitempty"`
}
