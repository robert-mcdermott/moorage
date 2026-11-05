# TLS and trusting the server

Moorage serves apps over TLS in one of four ways, chosen by `tls.mode` in
`server.toml`:

| `tls.mode` | Certificates | For |
|---|---|---|
| `internal` | Issued by Moorage's own CA, for each hostname as it is first asked for, and renewed by themselves | A server on your network. The default |
| `files` | Files you provide under `[[tls.certificates]]`, such as a corporate wildcard, reloaded when they change | A company that issues its own |
| `acme` | [From a public CA](#from-a-public-ca-with-acme), or an internal ACME server, and renewed by themselves | A server on the internet |
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

## From a public CA, with ACME

> **What this has been tried against.** CI runs each way of getting a certificate described here against Pebble, the test server that
> Let's Encrypt publishes, and RFC 2136 against a real DNS server. It has not yet got a certificate from Let's Encrypt itself, or set a
> record at Cloudflare. If you try them first, please [say how it goes](https://github.com/robert-mcdermott/moorage/issues).

```bash
sudo moor server init --preset public --domain apps.example.com --email ops@example.com --agree-terms
```

`--agree-terms` says that you have read and accept [the terms of Let's Encrypt](https://letsencrypt.org/repository/), which is the CA unless
you say otherwise. Point a wildcard DNS record, `*.apps.example.com`, at the server, and open ports 80 and 443 to the internet, as that is
where the CA looks. That is all: a certificate comes the first time a hostname is asked for, and is renewed by itself. Browsers already
trust it, so there is nothing to install.

The preset also puts everything but production behind a sign-in. See [`[access]`](configuration.md#access).

**Only what the server serves gets a certificate:** the console, and the hostnames of apps, environments and [your own domains](domains.md)
that it knows. A CA limits how many certificates a domain can have, and anyone who connects can say any name they like, so a made-up name
gets nothing, and the CA is not asked.

### One certificate for the whole domain

Each hostname having its own certificate is simple, but [Let's Encrypt allows 50 for a domain each week](https://letsencrypt.org/docs/rate-limits/),
the first visit to each waits a few seconds for one, and each name is published in the CA's public logs. With many previews, get one certificate
for `*.apps.example.com` instead, which takes showing the CA a DNS record, so tell Moorage how to set one:

```toml
[tls.acme.dns]
provider = "cloudflare"
token_file = "/etc/moorage/cloudflare.token"
```

The file holds an API token that can edit that zone's DNS, and should be readable only by the server. The server gets the certificate when it starts.
Your own domains, which are not under that wildcard, still get one each.

For a DNS server of your own, `rfc2136` sends it signed updates:

```toml
[tls.acme.dns]
provider = "rfc2136"
server = "ns1.example.com:53"
key_name = "moorage"
key_file = "/etc/moorage/tsig.key"
```

For anything else, `command` runs a program of yours, which sets a TXT record on any DNS service you can reach from a script:

```toml
[tls.acme.dns]
provider = "command"
command = "/usr/local/bin/moorage-dns"
wait = "60s"
```

It is run as `moorage-dns add _acme-challenge.apps.example.com VALUE` before the CA looks, and with `remove` afterwards. `wait` is how long to give
the record to reach the servers the CA asks. A failure, and what the program said, goes in the server's log.

### Another CA, or one of your own

`directory` names another ACME server, and `ca_file` the root to trust it by, if that is not a public one:

```toml
[tls.acme]
directory = "https://ca.internal.example.com/acme/directory"
ca_file = "/etc/moorage/tls/internal-root.pem"
email = "ops@example.com"
agree_terms = true
```

To try it first, with no limits to speak of, use Let's Encrypt's staging directory, `https://acme-staging-v02.api.letsencrypt.org/directory`, whose
certificates browsers do not trust.

### When it cannot get one

[`moor server doctor`](troubleshooting.md) says how many certificates there are and when the next one ends. A certificate is renewed when a third of its life is
left, and if that fails, the server's log says why, it goes on trying, and [those concerned are told](notifications.md), once, and again when it has worked.
What it was serving goes on being served until the certificate ends.

Port 80 and port 443 are what a CA checks, on the hostname itself. With only one of them open, `challenges = ["http-01"]` or `["tls-alpn-01"]` says which to use. Behind a
port forward, or with `listen.http` and `listen.https` on other ports, what the CA reaches on 80 and 443 has to arrive there.

The account and the certificates are kept in `certs/acme` in the data directory. [Backups](backups.md) hold them, with their private keys sealed.
