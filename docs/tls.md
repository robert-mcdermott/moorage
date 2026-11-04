# TLS and trusting the server

Moorage serves apps over TLS in one of three ways, chosen by `tls.mode` in
`server.toml`:

| `tls.mode` | Certificates | For |
|---|---|---|
| `internal` | Issued by Moorage's own CA, for each hostname as it is first asked for, and renewed by themselves | A server on your network. The default |
| `files` | Files you provide under `[[tls.certificates]]`, such as a corporate wildcard, reloaded when they change | A company that issues its own |
| `off` | None: plain HTTP | [Behind a TLS proxy](proxy.md), or on your own computer |

A certificate from the internal CA goes only to the console's hostname and to
names of apps under the base domain, so a stranger's guess at a hostname gets
nothing.

## Trusting the internal CA

Browsers warn about the certificates of a CA they do not know. On each computer
that uses the server, once:

```bash
moor trust --server https://moorage.apps.example.com
```

```text
Moorage CA for apps.example.com
  Fingerprint  SHA-256 4F:9A:…:C1
  May sign for apps.example.com, and nothing else
  Expires      3 Nov 2036
  From         https://moorage.apps.example.com

Nothing vouches for this answer yet: compare the fingerprint with what
moor server status shows on the server. Trust this CA on this computer? [y/N]
```

The CA goes into the operating system's list, which Safari, Chrome and Edge use
on macOS and Windows, and command-line tools on Linux. On Linux it also goes into
the lists Chromium and Firefox keep for themselves, if `certutil` is installed.
Firefox may keep its own list on other systems too: `moor trust --print` writes
the certificate, to import in its settings.

The CA that Moorage generates may sign only for the server's domain, so trusting
it does not let the server pretend to be your bank. `moor trust` says what a CA
may sign for, and warns of one that may sign for any site.

| | |
|---|---|
| On the server's own machine | `moor trust` reads the server's files, and asks nothing |
| In a script | `moor trust --server URL --fingerprint AB:CD:…` asks nothing if the fingerprint matches, and fails if it does not |
| With a file you obtained safely | `moor trust --file corporate-root.pem` |
| To see it and change nothing | `moor trust --print` |
| To take it back | `moor trust --remove` |

On macOS and Windows, `moor trust` changes your own list, and the system asks you
to confirm; with `sudo` or as an Administrator, it changes the machine's. On Linux
it uses `sudo`, which may ask for your password. On macOS, `--remove` may also ask
someone at the screen to confirm removing the trust setting: after two minutes with
no answer, the certificate is gone and the setting stays, and `moor trust` says so.
The fingerprint is also in the server's log when it starts. Each server's CA is at `/.moorage/ca.crt`, for anyone
who would rather install it by hand.

`moor` itself uses the system's list. On a machine where you would rather not
change it, `--ca-cert FILE` trusts a CA for one command, and `moor login --ca-cert`
for one server.

## With your company's CA

Company machines usually trust a company's root already. An intermediate CA issued
from it, and ideally limited to the base domain, makes Moorage's certificates
trusted everywhere with nothing to install:

```toml
[tls]
mode    = "internal"
ca_cert = "/etc/moorage/tls/intermediate.crt"
ca_key  = "/etc/moorage/tls/intermediate.key"
```

Or use a wildcard certificate for the base domain:

```toml
[tls]
mode = "files"

[[tls.certificates]]
cert = "/etc/moorage/tls/wildcard.crt"
key  = "/etc/moorage/tls/wildcard.key"
```

[`moor server doctor`](troubleshooting.md) checks the certificates: that they exist,
match their keys, cover the hostnames, and when they expire.
