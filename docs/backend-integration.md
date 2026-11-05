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
| Soar | `soar-server` | `SOAR_LISTEN=127.0.0.1:5305` | `127.0.0.1:5305` UDP only |
| Soar 2.2 | `soar22-server` | `SOAR_LISTEN=127.0.0.1:5306` | `127.0.0.1:5306` UDP only |
| SlipGate DNS tunnels | `slipgate-{tag}` | Native tunnel ports from `/etc/slipgate/config.json` | Automatically imported |

Use the TUI or `cottenrouter install` to install one project at a time. Its
adapter plans private listeners, snapshots configuration, and checks port
ownership before committing the router changes. Native upstream installers
often assume standalone public port 53; CottenRouter applies its loopback
integration before accepting the installation. Soar builds the latest resolved
official Lantern source. The separate Soar 2.2 option uses WhiteDNS standalone
setup 0.1.3's verified amd64 bundle with the older Lantern engine v0.2.2. Both
use CottenRouter's lifecycle instead of WhiteDNS's native standalone wizard,
manager, or optional web panel.

Each routed service needs a unique domain or subdomain, for example
`cotten.example.com`, `master.example.com`, `storm.example.com`,
`feed.example.com`, `soar.example.com`, and `soar22.example.com`. Two services
cannot own the exact same suffix. Nested suffixes are supported; the longest
suffix wins.

## Feature boundaries

- UDP DNS payloads are passed byte-for-byte except for the temporary DNS
  transaction-ID mapping, which is restored on replies. A reply consumes that
  mapping only when its echoed question name, type, and class match the question
  the router sent under that ID; otherwise it is dropped and the mapping stays
  open for the genuine reply.
- Clear DNS-over-TCP/53 uses standard RFC 1035 framing and can pipeline queries
  for different backends on one client connection. A route's `tcp_backend`
  accepts a TCP address, `disabled`, or `udp` to bridge framed TCP messages to
  its UDP backend. Imported SlipGate DNS tunnels use `udp`, including Soar;
  verification probes work over both public DNS transports.
- CottenDNS DoT and DoH bind private loopback ports. Public TLS listeners route
  by SNI and pass encrypted bytes unchanged, preserving configured certificates,
  HTTP paths, and authentication. CottenDNS's current ACME switch checks its
  local DoH port for `:443`; that is false behind CottenRouter's private port.
  Router-front DoH therefore requires `TLS_CERT_FILE`/`TLS_KEY_FILE` until an
  upstream `ACME_EXTERNAL_PORT` setting is available. ACME is never claimed on
  an alternate public port.
- CottenDNS domain rotation (`ADVERTISE_DOMAINS`) only advertises domains that
  are also in `DOMAIN`. Add each standby domain as an extra domain first; that
  puts it in `DOMAIN` and in the router's route. `ZONE_NS` needs nothing from
  the router: apex NS queries already reach CottenDNS.
- SlipGate NaiveProxy and StunTLS use TLS passthrough. NaiveProxy routes by
  hostname; native StunTLS has no domain field and uses the listener's default
  backend, including clients without SNI. Each backend gets a private loopback
  port. Only one default backend can use a public TLS port.
- Thefeed's main, extra, messenger and MTProto domains are automatically added
  to the same route on install, update and Advanced save. Future native
  `THEFEED_*_DOMAINS` lists are imported too. Native wizard selections and
  MTProto quotas, media, signing and relay settings remain in
  `/opt/thefeed/data/thefeed.env`; payloads pass through unchanged. Each feature
  still needs the distinct delegated zones required by thefeed.
- Soar and Soar 2.2 each use one delegated zone and a UDP-only listener. Each
  route sets `tcp_backend` to `disabled`; do not enable backend TCP, DoT, or DoH. All DNS
  question types under the zone reach Soar, including SOA and intermediate
  names used by QNAME-minimising resolvers. Share its Ed25519 public key with
  clients and retain the private seed on the server. They can run together
  using distinct zones, ports, and seeds. See [Soar setup](soar.md).
- When `slipgate_configs` is used, stop and disable `slipgate-dnsrouter` only.
  Keep all `slipgate-{tag}`, SOCKS, SSH, NaiveProxy, StunTLS and enabled WARP
  services. HMAC verification probes are answered using imported
  DNSTT/VayDNS public keys and Slipstream certificates. Soar uses its native
  Ed25519 handshake; its public key is also available through `cottenrouter keys`.

The native SlipGate import covers DNSTT, Slipstream, VayDNS, Soar and external
DNS transports and their applicable verification material. Native public
address, client links, users, outbound rules and transport tuning stay under
SlipGate's control. Removed or disabled DNS tunnels lose their router routes
on reintegration. The installer moves managed TLS services to private loopback
listeners and creates their router TLS routes transactionally. See
`cottenrouter.example.json` for TLS routing examples.

## Installer freshness

`cottenrouter catalog` contacts GitHub for every project's current default
branch (or an explicitly pinned compatibility release), pins its commit, and verifies the installer path before producing a
command. For Soar it verifies `go.mod` and produces a pinned `go install`
command. Soar 2.2 instead pins WhiteDNS's standalone 0.1.3 release and verifies
the downloaded archive and its bundled manifest before using its prebuilt
Lantern v0.2.2 server. The TUI and CLI resolve the selected repository immediately
before installation; the bundled offline catalog is informational.
