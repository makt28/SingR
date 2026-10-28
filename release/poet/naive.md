# NaiveProxy SSPanel example

SSPanel has no native naive node type, so SingR reuses the same alias trick
as AnyTLS and Hysteria2: a vmess/ws node whose `path` is `naive` (or
`/naive`) is driven as a naive inbound.

For an old SSPanel V2ray node, set the node address like:

```text
sa.example.com;14556;0;ws;;path=/naive|host=xxxx.com
```

When `path` is `naive` or `/naive`, SingR treats the panel node as a naive
inbound while preserving the original panel type as `V2ray`. The panel
delivers only:

- `port` (offset port) → the real listen port, on **both TCP and UDP**
- `host` → TLS SNI (`server_name`)
- per-user `uuid` → naive Basic-auth password (falls back to `passwd`)
- `node_speedlimit` / `node_iplimit` → per-user limits

## HTTP/2 and HTTP/3 on one port

One naive inbound serves both carriers on the same port number:

- **TCP → HTTP/2** over TLS (`https://` in NaiveProxy terms)
- **UDP → HTTP/3** over QUIC (`quic://`)

The default template leaves `network` unset, which means both. Set
`"network": "tcp"` (or `"udp"`) in `server.json` to serve only one. Because
the port is bound on both protocols, a naive node **cannot share its port
number with an AnyTLS node (TCP) or a Hysteria2 node (UDP)** on the same
machine, and the firewall must allow both TCP and UDP on it.

The client picks the carrier from its configuration; there is no
negotiation or fallback between them, and the server sends no `Alt-Svc`.
To let users fall back to HTTP/2 when UDP is blocked, emit **two
subscription entries** per node (one `https://`, one `quic://`) and let the
client's selector or URL test choose.

If HTTP/3 cannot bind (for example the UDP port is taken), the inbound fails
to start instead of silently serving HTTP/2 only (upstream sing-box only
warns). The same holds for a panel port change: both carriers move together
or neither does.

## Authentication

Clients send `Proxy-Authorization: Basic <username>:<password>`. SingR
authenticates by **password only** — the panel user's `uuid` (or `passwd`
when there is no UUID) — and **ignores the username**, the same model as the
AnyTLS and Hysteria2 inbounds. The identity used for accounting always comes
from SingR's user table, never from the username the client sends. So a
subscription can put anything in the username:

```text
naive+https://anything:<uuid>@xxxx.com:14556
naive+quic://anything:<uuid>@xxxx.com:14556
```

For a sing-box client, use the `naive` outbound (built with
`with_naive_outbound`); `"quic": true` selects HTTP/3. UDP is carried over
the tunnel only through sing-box's `udp_over_tcp` — the NaiveProxy client
itself cannot proxy UDP at all.

## Server config

The real naive inbound must be declared in the main sing-box JSON (the
default `server.json` superset already has `naive-in`). Set the same `host`
value as the SSPanel node into `server_name` and provide a certificate that
covers it:

```json
"tls": {
  "enabled": true,
  "server_name": "xxxx.com",
  "certificate_path": "",
  "key_path": ""
}
```

Leaving `certificate_path` and `key_path` empty is not "no certificate" — it
means "use the default location". At startup SingR fills empty values in with
`<directory of the panel config>/certs/default.pem` (falling back to
`default.crt` when the `.pem` is absent) plus `default.key`, which is
`/etc/singr/certs/` for a bare-metal install and `/etc/singr-docker/certs/`
for the docker one. The substitution is logged:

```text
inbound/naive[naive-in]: no TLS certificate configured, using default /etc/singr/certs/default.pem + /etc/singr/certs/default.key
```

### The certificate must be real

The original NaiveProxy client is built on Chromium and verifies the
certificate strictly, with no option to skip verification. The certificate
must be valid and cover the SNI; a self-signed certificate will not work with
it.

## Hot reload

`listen_port` and `server_name` are hot-reloaded from the panel at runtime:

- **port change:** HTTP/2 and HTTP/3 come up on the new port first, then the
  old port is closed. Established HTTP/2 tunnels keep running; HTTP/3
  sessions on the old port are dropped (they live on its UDP socket, as with
  a Hysteria2 port change). If either new socket fails to bind, nothing
  changes and the reload is retried on the next panel tick.
- **SNI change:** only the TLS config is swapped; no socket is touched, so
  unlike Hysteria2 there is no restart window.

The certificate/key and `network` are not hot-reloaded by the panel path;
certificate **file contents** are reloaded live, as for the other protocols.

## Probing

Like the AnyTLS inbound, naive has no fallback site: a request that is not a
valid naive `CONNECT` gets `400`, a wrong password gets the connection reset.
Such failures are logged at debug level only.
