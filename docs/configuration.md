# Server configuration

`server.toml` holds what the server needs before it can open its database. `moor server init` writes it, with a comment
for each setting. Change the file and restart the server to apply it. A mistake stops the server, and says which setting, with the
line for a syntax error or a setting it does not know, so that a typing error is not quietly ignored.

Everything else is kept in the database, and changed in the console or with `moor`: see
[Settings in the database](#settings-in-the-database).

## Where it is

| | As root, or an elevated Administrator | As yourself |
|---|---|---|
| **Linux** | `/etc/moorage/server.toml` | `~/.moorage/server.toml` |
| **macOS** | `/Library/Application Support/Moorage/server.toml` | `~/.moorage/server.toml` |
| **Windows** | `%ProgramData%\Moorage\server.toml` | `%LocalAppData%\Moorage\server.toml` |

`--config` or `$MOORAGE_CONFIG` names another. `moor server status` says which one a server uses.

These variables override what the file says:

| Variable | Overrides |
|---|---|
| `MOORAGE_DATA_DIR` | `data_dir` |
| `MOORAGE_LOG_LEVEL` | `[log] level` |
| `MOORAGE_LOG_FORMAT` | `[log] format` |

## Presets

```bash
moor server init --preset local
```

```bash
sudo moor server init --domain apps.example.com
```

```bash
sudo moor server init --domain apps.example.com --behind-proxy
```

```bash
sudo moor server init --preset public --domain apps.example.com --email ops@example.com --agree-terms
```

| | `local` | `lan` | `lan --behind-proxy` | `public` |
|---|---|---|---|---|
| **For** | This computer | A server on your network | A server behind nginx or the like | A server on the internet |
| **Apps at** | `http://<app>.localhost:8080` | `https://<app>.<domain>` | `https://<app>.<domain>` | `https://<app>.<domain>` |
| `listen.http` | `127.0.0.1:8080` | `:80` | `127.0.0.1:8080` | `:80` |
| `listen.https` | | `:443` | | `:443` |
| `listen.ssh` | `127.0.0.1:2222` | `:2222` | `:2222` | `:2222` |
| `tls.mode` | `off` | `internal` | `off` | `acme` |
| `access.named`, `access.preview` | `public`, `login` | `public`, `login` | `public`, `login` | `login`, `login` |
| `proxy.trusted` | | | `127.0.0.1`, `::1` | |
| `proxy.external_scheme` | `http` | `https` | `https` | `https` |

## An example

```toml
data_dir     = "/var/lib/moorage"
domain       = "apps.example.com"
console_host = "moorage.apps.example.com"
routing      = "host"

[access]
production = "public"
named      = "public"
preview    = "login"

[isolation]
level = "user"

[listen]
http  = ":80"
https = ":443"
ssh   = ":2222"

[tls]
mode    = "internal"
ca_cert = "/etc/moorage/tls/intermediate.crt"
ca_key  = "/etc/moorage/tls/intermediate.key"

[log]
level  = "info"
format = "json"
```

## The top level

| Setting | Default | What it says |
|---|---|---|
| `data_dir` | required | Where the server keeps its database, builds, logs and apps' data. An absolute path. |
| `domain` | required | Apps are at `<app>.<domain>`. Point a wildcard DNS record, `*.<domain>`, at the server. |
| `console_host` | `moorage.<domain>` | The hostname of the console, the API and git over HTTPS. |
| `routing` | `"host"` | `host` gives every app a hostname of its own. `path` puts apps under `/<app>/` on the domain, for where there can be no wildcard record: see [path routing](domains.md#with-no-wildcard-dns-path-routing). The console then needs a hostname of its own. |

## `[listen]`

| Setting | Default | What it says |
|---|---|---|
| `http` | required | Where it answers plain HTTP. With TLS, it redirects to HTTPS. |
| `https` | | Where it answers HTTPS. Empty when Moorage does not terminate TLS. |
| `ssh` | | Where git pushes arrive over SSH. Empty turns that off. |
| `dns` | | A loopback address where it answers DNS for its domain, for `moor server resolver`. |

## `[tls]`

See [TLS and trusting the server](tls.md).

| Setting | Default | What it says |
|---|---|---|
| `mode` | `"internal"` with `listen.https`, otherwise `"off"` | `internal`: certificates from Moorage's own CA, or from an intermediate of yours. `files`: certificates you provide. `acme`: certificates from a public CA or an internal ACME server. `off`: plain HTTP, such as behind a proxy. |
| `ca_cert`, `ca_key` | | With `internal`, an intermediate CA issued by your company's, so that its certificates are already trusted. Without them, Moorage makes its own root. |

`[[tls.certificates]]`, once for each, with `mode = "files"`:

| Setting | What it says |
|---|---|
| `cert` | A certificate chain, in a PEM file |
| `key` | Its private key, in a PEM file |

`[tls.acme]`, with `mode = "acme"`: see [From a public CA, with ACME](tls.md#from-a-public-ca-with-acme).

| Setting | Default | What it says |
|---|---|---|
| `agree_terms` | required | Must be `true`: it says that you have read and accept the CA's terms. |
| `directory` | Let's Encrypt | The address of the ACME server's directory. |
| `email` | | Whom the CA tells of what it has to say about the account. |
| `ca_file` | | The root to trust an internal ACME server by. |
| `challenges` | both | How a hostname is shown to be this server's: `http-01`, on port 80, and `tls-alpn-01`, on port 443. |

`[tls.acme.dns]`, for one certificate for the whole domain:

| Setting | What it says |
|---|---|
| `provider` | `cloudflare`, `rfc2136` or `command` |
| `token_file` | With `cloudflare`, the file that holds its API token |
| `server` | With `rfc2136`, the DNS server, as `host:port` |
| `key_name`, `key_file`, `key_algorithm` | With `rfc2136`, its TSIG key: its name, the file that holds its base64 secret, and its algorithm, by default `hmac-sha256` |
| `command` | With `command`, a program run as `add NAME VALUE` and `remove NAME VALUE` |
| `wait` | With `command`, how long to give a record to reach the CA. By default `30s` |
| `resolvers` | With `cloudflare` or `rfc2136`, the DNS servers to look for the record through, as `host:port`. By default the system's |

## `[proxy]`

See [Behind a TLS proxy](proxy.md).

| Setting | Default | What it says |
|---|---|---|
| `trusted` | none | Addresses or prefixes of the proxies whose `X-Forwarded-*` headers and PROXY protocol are believed. |
| `external_scheme` | `"https"` with TLS or a trusted proxy, otherwise `"http"` | The scheme browsers use to reach Moorage. |

## `[access]`

Who may reach each kind of environment, unless its manifest or a maintainer says otherwise. Each is `public`, `password`,
`login` or `share`: see [Who can reach an app](access.md).

| Setting | Default |
|---|---|
| `production` | `"public"` |
| `named` | `"public"` |
| `preview` | `"login"` |

A manifest may ask for another gate, but it may not open an environment to `public` where this does not.

## `[isolation]`

See [How apps are kept apart](isolation.md).

| Setting | Default | What it says |
|---|---|---|
| `level` | `"auto"` | `auto` is `user` where the server runs as root on Linux, and `none` elsewhere. `user` gives each app an operating system user of its own. `sandbox` also confines it to its own files, with Landlock. |
| `uid_base` | `300000` | Where the users of apps start. |

## `[auth]`

For a proxy that signs people in. See [How people sign in](sign-in.md#behind-a-proxy-that-signs-people-in).

| Setting | Default | What it says |
|---|---|---|
| `trusted_header` | | The header in which the proxy says who someone is. It counts only from `proxy.trusted`, which it needs. |
| `trusted_name_header` | | The header with their name, if there is one. |
| `trusted_provision` | `false` | Make an account for someone new, the first time the proxy says who they are. |

## `[notifications]`

See [Notifications](notifications.md#what-notifications-may-reach).

| Setting | Default | What it says |
|---|---|---|
| `allow` | none | Addresses and prefixes that webhooks, mail servers, telemetry sinks and forges may be at, though they would be refused as this machine, a link-local address or a cloud's metadata service. |
| `disk_percent` | `90` | How full the data volume may get before someone is told. |

## `[toolchains]`

Builds download Go, Node.js, uv and Python on demand, and check them. These are mirrors of the official places, for proxies
and networks with no way out.

| Setting | Default | What it says |
|---|---|---|
| `go_url` | `https://go.dev/dl/` | Serves Go's download index and files. |
| `node_url` | `https://nodejs.org/dist/` | Serves Node.js releases. |
| `uv_url` | `https://github.com/astral-sh/uv/releases` | Serves uv's releases. |
| `python_url` | | Handed to uv, as its Python mirror. |
| `signatures` | `"require"` | What to do with a release whose signature cannot be checked. `require` refuses it, and `warn` goes on and says so, for a mirror with no signatures or a new releaser. A signature that is wrong is always refused. |

## `[containers]`

See [Running apps in containers](containers.md).

| Setting | Default | What it says |
|---|---|---|
| `engine` | `"auto"` | `auto` is Podman if it is installed, or else Docker. Or the path of a binary. |

## `[log]`

The server's own log. See [Where the logs are](troubleshooting.md#where-the-logs-are).

| Setting | Default | What it says |
|---|---|---|
| `level` | `"info"` | `debug`, `info`, `warn` or `error`. |
| `format` | `"text"` | `text` or `json`, for what it writes to its standard error. The file it keeps is always JSON. |

## Settings in the database

Everything else is changed in the console's **Settings**, or with `moor`, and takes effect at once: people, projects,
container registries, notification channels and rules, telemetry sinks, backups and single sign-on.

To keep them in a file, review them, or bring them to another server:

```bash
moor server config export --output moorage-config.toml
```

```bash
moor server config import moorage-config.toml --dry-run
```

The file holds no secrets: where one belongs, it names a variable of the environment, such as
`${MOORAGE_SSO_CLIENT_SECRET}`, which is read when it is brought in. Bringing one in adds and changes, and removes nothing.
People's passwords and second factors do not go with it.
