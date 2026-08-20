# Backend integration notes

CottenRouter must be the only public process bound to UDP/TCP port 53. Every DNS
application binds a distinct loopback port and keeps its own distinct delegated
DNS suffix. The router does not inspect or modify tunnel payloads; it only reads
the first DNS question name and rewrites the 16-bit DNS transaction ID while a
query is in flight.

| Project | systemd service | Private listener setting | Example router backend |
|---|---|---|---|
| CottenDNS | `cottendns` | `UDP_HOST = "127.0.0.1"`, `UDP_PORT = 5301`, `TCP_LISTENER_ENABLED = true` | `127.0.0.1:5301` UDP+TCP |
| MasterDnsVPN | `masterdnsvpn` | `UDP_HOST = "127.0.0.1"`, `UDP_PORT = 5302` | `127.0.0.1:5302` |
| StormDNS | `stormdns` | `UDP_HOST = "127.0.0.1"`, `UDP_PORT = 5303` | `127.0.0.1:5303` |
| thefeed | `thefeed-server` | `THEFEED_LISTEN=127.0.0.1:5304` | `127.0.0.1:5304` |
| SlipGate DNS tunnels | `slipgate-{tag}` | Native tunnel ports from `/etc/slipgate/config.json` | Automatically imported |

The upstream installers currently default to public port 53. During the future
TUI phase, an adapter must install one project at a time, change its listener
before it is started, and then restore/start CottenRouter. Blindly running all
four upstream scripts in sequence is unsafe because their port-conflict logic
was designed for standalone installation.

Each routed service needs a unique domain or subdomain, for example
`cotten.example.com`, `master.example.com`, `storm.example.com`, and
`feed.example.com`. Two services cannot own the exact same suffix. Nested
suffixes are supported; the longest suffix wins.

## Feature boundaries

- UDP DNS payloads are passed byte-for-byte except for the temporary DNS
  transaction-ID mapping, which is restored on replies. A reply consumes that
  mapping only when its echoed question name, type, and class match the question
  the router sent under that ID; otherwise it is dropped and the mapping stays
  open for the genuine reply.
- Clear DNS-over-TCP/53 uses standard RFC 1035 framing and can pipeline queries
  for different backends on one client connection.
- CottenDNS DoT and DoH bind private loopback ports. Public TLS listeners route
  by SNI and pass encrypted bytes unchanged, preserving configured certificates,
  HTTP paths, and authentication. CottenDNS's current ACME switch checks its
  local DoH port for `:443`; that is false behind CottenRouter's private port.
  Router-front DoH therefore requires `TLS_CERT_FILE`/`TLS_KEY_FILE` until an
  upstream `ACME_EXTERNAL_PORT` setting is available. ACME is never claimed on
  an alternate public port.
- SlipGate NaiveProxy and StunTLS can use the same TLS passthrough. Give every
  SNI-routed service its own hostname and private loopback port. StunTLS has no
  domain field in SlipGate's native config, so its hostname is entered in the
  CottenRouter TLS route explicitly.
- Add every thefeed main, extra, and chat domain to the same route. CottenRouter
  does not interpret feed, messenger, media, signing, or relay payloads.
- When `slipgate_configs` is used, stop and disable `slipgate-dnsrouter` only.
  Keep all `slipgate-{tag}`, SOCKS, SSH, NaiveProxy, and StunTLS services. HMAC
  verification probes are answered using imported public keys/certificates.

The native SlipGate import covers all DNS transports and their verification
material. TLS services stay explicit because SlipGate's default public `:443`
must first be moved to a private listener; the future TUI adapter will perform
that service rewrite transactionally. See `cottenrouter.example.json` for
NaiveProxy and StunTLS SNI routes.

## Installer freshness

`cottenrouter catalog` contacts GitHub for every project's current default
branch and verifies the installer path before producing a command. The future
TUI installer must call this online resolver immediately before showing or
executing a command; it must not install from the bundled offline fallback.
