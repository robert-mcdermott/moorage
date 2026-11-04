# Behind a TLS proxy

If you already run nginx or a load balancer for its certificates, its access logs
or because policy asks for one, Moorage can serve plain HTTP behind it:

```bash
sudo moor server init --domain apps.example.com --behind-proxy
```

```toml
[listen]
http = "127.0.0.1:8080"              # or a private address the proxy can reach
ssh  = ":2222"                       # git over SSH goes straight to Moorage

[tls]
mode = "off"                         # the proxy terminates TLS

[proxy]
trusted         = ["127.0.0.1/32"]   # believe forwarded headers only from these
external_scheme = "https"            # the scheme browsers see
```

Headers such as `X-Forwarded-For` are believed only from the addresses under
`trusted`. From anywhere else they are ignored, so nobody can pretend to be someone
else by sending them. `X-Moorage-*` headers from clients are always removed.

## nginx

`moor server proxy-config` writes the configuration, for this server's hostnames:

```bash
moor server proxy-config nginx --cert /etc/nginx/tls/wildcard.crt --key /etc/nginx/tls/wildcard.key | sudo tee /etc/nginx/conf.d/moorage.conf
```

```bash
sudo nginx -t && sudo nginx -s reload
```

The certificate must cover the console's hostname and `*.<domain>`. CI runs a server
behind a real nginx with this configuration, and deploys and pushes through it.

## Any other proxy

Whatever stands in front of Moorage must:

- send the console's hostname and `*.<domain>` to Moorage, **keeping the `Host`
  header**: Moorage routes by it;
- set `X-Forwarded-For` and `X-Forwarded-Proto`;
- pass WebSocket upgrades on, with HTTP/1.1 and the `Upgrade` and `Connection`
  headers;
- not buffer responses, or logs and deploys will not show as they happen. Moorage
  marks these responses with `X-Accel-Buffering: no`;
- allow long requests: a `git push` over HTTPS stays open for its whole deployment,
  and a followed log can be silent for long. Limits of a minute break them;
- allow large requests: an upload or a push may be up to 1 GiB, and Moorage limits
  them itself.

`/.moorage/health` answers on every hostname, for a load balancer's health checks.

## Check the whole path

On the server's machine, with the address people use:

```bash
moor server doctor --public-url https://moorage.apps.example.com --timeouts
```

```text
✔ proxy (headers)     from 127.0.0.1, a trusted proxy, with the hostname and X-Forwarded headers
✔ proxy (apps)        *.apps.example.com reaches the server too
✔ proxy (websockets)  pass through in both directions
✘ proxy (streaming)   responses are buffered: lines written over most of a second arrived all at once, so logs and deploys would not show as they happen
                      → proxy_buffering off in nginx
✔ proxy (uploads)     a request of 8 MiB gets through
✔ proxy (timeouts)    a request may stay silent for 70 seconds
```

`--timeouts` adds over a minute to the check, to catch a limit of a minute.
`--ca-cert` names the issuer of the proxy's certificate, if the system does not
trust it.
