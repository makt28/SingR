# Hysteria2 SSPanel example

SSPanel has no native hysteria2 node type, so SingR reuses the same alias
trick as AnyTLS: a vmess/ws node whose `path` is `hy2` (or `/hy2`) is driven
as a hysteria2 inbound.

For an old SSPanel V2ray node, set the node address like:

```text
sa.example.com;14555;0;ws;;path=/hy2|host=xxxx.com
```

When `path` is `hy2` or `/hy2`, SingR treats the panel node as a hysteria2
inbound while preserving the original panel type as `V2ray`. The panel
delivers only:

- `port` (offset port) → the real UDP listen port
- `host` → TLS SNI (`server_name`)
- per-user `uuid` → hysteria2 auth password (falls back to `passwd`)
- `node_speedlimit` / `node_iplimit` → per-user limits

Everything hysteria2-specific (obfs, up/down bandwidth, masquerade, port
hopping) is **not deliverable by the panel** and must be set locally in
`release/poet/server_hysteria2.json`.

## Server config

The real hysteria2 inbound must be declared in the main sing-box JSON. Set
the same `host` value as the SSPanel node into `server_name` and provide a
certificate that covers it:

```json
"tls": {
  "enabled": true,
  "server_name": "xxxx.com",
  "certificate_path": "/etc/singr/certs/hysteria2.crt",
  "key_path": "/etc/singr/certs/hysteria2.key"
}
```

`listen_port` and `server_name` are hot-reloaded from the panel at runtime
(`port` and `host`); the certificate/key, obfs, bandwidth and masquerade are
**not** hot-reloaded — changing them requires a restart.

## TLS is mandatory

Unlike AnyTLS, hysteria2 requires TLS. If you use a self-signed certificate,
clients must trust it or enable insecure verification.

## obfs (optional, anti-DPI)

`obfs` (salamander) scrambles UDP packets so the traffic does not look like
plain QUIC. It is **off by default**. The obfs password is a single
node-wide shared secret (NOT per-user), so the panel cannot deliver it.

If you need it, set the same password on both ends. A convenient trick is to
reuse the SNI as the obfs password (the panel already delivers `host`):

```json
"obfs": {
  "type": "salamander",
  "password": "xxxx.com"
}
```

and have the subscription template emit
`obfs=salamander&obfs-password=xxxx.com`. Note this is weaker obfuscation
than a random secret. Leave obfs off unless your network actively
fingerprints/blocks QUIC.

## Bandwidth

`up_mbps` / `down_mbps` of `0` means unlimited / let the client self-report
its bandwidth (BBR or Brutal). Per-user speed limiting still comes from the
panel `node_speedlimit` and is enforced independently of hysteria2's own
congestion control.

## Port hopping (optional, deployment-only)

The hysteria2 process binds a single UDP port. Port hopping is done at the
OS firewall layer by redirecting a port range to that real port, e.g.:

```sh
iptables  -t nat -A PREROUTING -p udp --dport 40000:60000 -j REDIRECT --to-ports <real_port>
ip6tables -t nat -A PREROUTING -p udp --dport 40000:60000 -j REDIRECT --to-ports <real_port>
```

Then have the subscription target the range, e.g.
`hysteria2://<uuid>@host:40000-60000/?sni=...`. The range must not overlap
the real listen port or other services. If a relay in front (e.g. nyanpass)
already does port hopping, do **not** also add this NAT — let exactly one
layer own the hopping.
