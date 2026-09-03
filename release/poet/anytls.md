# AnyTLS SSPanel example

For an old SSPanel V2ray node, set the node address like:

```text
sa.akanyoni.com;14555;0;ws;;path=/anytls|host=xxxx.com|relay_server=other.agstores99.vip|relay_port=42132
```

When `path` is `anytls` or `/anytls`, SingR treats the panel node as an AnyTLS inbound while preserving the original panel type as `V2ray`.

The `host` value is parsed into `NodeInfo.Host`. Because the current runtime still requires the real AnyTLS inbound to be declared in the main sing-box JSON, set the same value in `release/poet/server_anytls.json`:

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
inbound/anytls[anytls-in]: no TLS certificate configured, using default /etc/singr/certs/default.pem + /etc/singr/certs/default.key
```

A path that is already set is never rewritten, so per-node certificates (and
existing installations) keep working unchanged.

You may also point `certificate_path` and `key_path` at your own certificate and private key. The certificate should cover the configured `server_name`/SSPanel `host` value. If you use a self-signed certificate, clients must explicitly trust it or enable insecure TLS verification.
