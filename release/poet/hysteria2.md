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

## obfs (anti-DPI, on by default via SNI)

`obfs` (salamander) scrambles UDP packets so the traffic does not look like
plain QUIC. The obfs password is a single node-wide shared secret (NOT
per-user). The default template ships the obfs block with an **empty
password**:

```json
"obfs": { "type": "salamander", "password": "" }
```

Rule (SingR-specific):

- **empty `password` → falls back to the TLS SNI** (`server_name`, i.e. the
  panel's `host=`). This lets SSPanel "deliver" the shared obfs secret for
  free via the SNI it already sends.
- **non-empty `password` → that value is used verbatim.**
- **remove the whole `obfs` block → obfs disabled.** (An empty password is
  "on, using SNI", not "off".)

Because obfs is rebuilt together with the QUIC service, the SNI-derived
password follows an SNI hot-reload; an explicit password stays fixed.

> Once obfs is on, **every client must use the same obfs** or it cannot
> connect at all (an obfs mismatch is a total failure, not a downgrade).
> The subscription template must emit
> `obfs=salamander&obfs-password=<sni-or-your-value>`. SNI-as-obfs is weaker
> than a random secret; for stronger obfuscation set an explicit random
> password on both ends (but then the panel can't help deliver it).

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

### Managed via `SingR porthop`

Rather than running the `iptables`/`ip6tables` commands by hand, the
management script ships a port-hopping manager: `SingR porthop` (or menu
item 13) lets you list / add / delete rules by entering start port, end
port and target (real) port. It writes both the v4 and v6 REDIRECT rules
(tagged with a `singr-porthop` iptables comment so they can be listed and
removed precisely without touching your other firewall rules) and persists
them in `/etc/singr/porthop.rules`. Persistence is SingR-owned: a generated
`singr-porthop.service` systemd oneshot replays the rules on boot — it does
**not** depend on `iptables-persistent` / `iptables-services`, so it won't
save or clobber your unrelated firewall rules. `install.sh` installs the
`iptables` package (v4+v6) for this.
