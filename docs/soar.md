# Soar setup

[Soar](https://github.com/getlantern/soar) carries encrypted TCP streams through
recursive DNS resolvers. CottenRouter manages its server and routes the
delegated zone to a private UDP listener alongside the other DNS backends.
The project manager offers **Soar** for the latest official Lantern source and
**Soar 2.2** for WhiteDNS's standalone bundle of the older engine **v0.2.2**.

## Install

Delegate a unique zone such as `soar.example.com` to your server, then select
**Soar** in `sudo cottenrouter tui` or run:

```bash
sudo cottenrouter install --project=soar --domain=soar.example.com
sudo cottenrouter keys --project=soar
```

Installation resolves the current upstream default-branch commit, verifies
`go.mod`, and builds `github.com/getlantern/soar/cmd/soar-server` at that exact
commit. A compatible Go toolchain is provisioned when needed. Allow network
access for GitHub, Go toolchain downloads, module downloads, and checksums, and
provide disk space for the compiler, dependencies, and build cache. Build or
download failure stops installation before the new backend is activated.
If the official Go archive cannot be fetched, the installer tries a mirror;
both sources must match the same pinned official SHA-256 before extraction.

For **Soar 2.2**, select that separate project or run:

```bash
sudo cottenrouter install --project=soar22 --domain=soar22.example.com
sudo cottenrouter keys --project=soar22
```

This option downloads the checksum-verified **WhiteDNS standalone setup 0.1.3**
release bundle and uses its prebuilt Lantern **v0.2.2** server. Its installer is
resolved from the `v0.1.3` tag to an immutable commit so future default-branch
updates cannot silently change this compatibility option. The bundle is
**Linux amd64 only** and does not need a local Go compiler. CottenRouter creates
its own private listener, service, configuration, and key; WhiteDNS's native
standalone wizard, `soar-manager`, and optional web panel are not run.
The original archive, including its license notices, documentation, and
diagnostic tool, is retained at
`/opt/cottenrouter/backends/soar22/whitedns-bundle.tar.gz` with checksum and
provenance records. The bundled standalone script remains inside the archive;
CottenRouter generates the active configuration and service.

Both projects can be installed together. Give each a distinct delegated zone;
their default ports, services, directories, and private seeds are independent.
The label **Soar 2.2** identifies this compatibility option. Its actual upstream
engine version is **v0.2.2**; official Soar tracks the current Lantern source.

## Configuration and keys

| Item | Soar | Soar 2.2 |
|---|---|---|
| Project ID | `soar` | `soar22` |
| Service | `soar-server.service` | `soar22-server.service` |
| Binary | `/opt/cottenrouter/backends/soar/soar-server` | `/opt/cottenrouter/backends/soar22/soar22-server` |
| Configuration | `/opt/cottenrouter/backends/soar/soar.env` | `/opt/cottenrouter/backends/soar22/soar.env` |
| Private seed | `/opt/cottenrouter/backends/soar/server.key` | `/opt/cottenrouter/backends/soar22/server.key` |
| Zone | `SOAR_ZONE=soar.example.com` | `SOAR_ZONE=soar22.example.com` |
| Listener | `SOAR_LISTEN=127.0.0.1:5305` | `SOAR_LISTEN=127.0.0.1:5306` |
| Destination ports | `SOAR_ALLOW_PORTS=80,443` | `SOAR_ALLOW_PORTS=80,443` |

The seed file is mode 600 and contains only the base64 Ed25519 seed. It survives
reinstallation, so existing clients keep the same server identity. The public
key printed by `cottenrouter keys --project=soar` or `--project=soar22` belongs
in client configuration. Clients never need the private seed. Upstream does not use the
public key as a per-client password; the server authenticates itself with it.

Use the guided form to change the zone or private port:

```bash
sudo cottenrouter configure --project=soar --domain=soar.example.com --port=5305
sudo cottenrouter advanced --project=soar
sudo cottenrouter configure --project=soar22 --domain=soar22.example.com --port=5306
sudo cottenrouter advanced --project=soar22
```

The Advanced editor supports the destination port allowlist. The production
server permits public destination addresses and ports 80 and 443 by default.
Configuration changes are validated and synchronized with the router.

## WhiteDNS Soar-Server comparison

[WhiteDNS/Soar-Server](https://github.com/WhiteDNS/Soar-Server) provides the
older standalone Soar installer and the separate current WhiteDNS account Panel.
CottenRouter manages these through distinct project entries:

| Component | CottenRouter Soar | CottenRouter Soar 2.2 | WhiteDNS Panel |
|---|---|---|---|
| Engine | Resolved official Lantern source; tested v0.2.3 | Soar v0.2.2 | Native Soar, Cotten and MasterDNS in the selected Panel release |
| Installation | Builds the pinned upstream commit | Verified WhiteDNS standalone 0.1.3 AMD64 bundle | Latest stable verified AMD64 or ARM64 bundle |
| Management | CottenRouter TUI/CLI | CottenRouter TUI/CLI | Native accounts, quotas, expiry, subscriptions, QR and backups |
| Identity | One delegated zone and server seed | One delegated zone and server seed | Independent account subzones and keys |
| DNS listener | Private `127.0.0.1:5305` UDP | Private `127.0.0.1:5306` UDP | Private `127.0.0.1:5307` UDP/TCP |

The native standalone installer owns `soar-server.service`,
`/etc/soar/server.conf` and `/etc/soar/server.key`. The managed Soar 2.2 option
uses `soar22-server.service` and its own directory, so it can coexist with
official Soar without replacing another installation.

Choose `whitedns-panel` for the current account Panel. Router preserves its
database and keys at `/opt/cottenrouter/backends/whitedns-panel/panel.db`,
assigns private DNS and administration ports, and manages upgrades through
`cottenrouter update --project whitedns-panel`. Administration normally uses
`127.0.0.1:2054` through SSH forwarding. The native privileged updater and public
HTTPS installer are not used in Router mode. See the
[Panel setup](../README.md#whitedns-panel-and-independent-updates) for commands.

The service sandbox permits `AF_NETLINK`: Soar's public-destination filter
enumerates local interface addresses through Linux netlink. A successful DNS
handshake or SOA response alone would miss an egress failure caused by denying
that address family.

## Routing

Both Soar servers listen on UDP only. Their entries in the router's `routes`
array are:

```json
[
  {
    "name": "soar",
    "domains": ["soar.example.com"],
    "backend": "127.0.0.1:5305",
    "tcp_backend": "disabled"
  },
  {
    "name": "soar22",
    "domains": ["soar22.example.com"],
    "backend": "127.0.0.1:5306",
    "tcp_backend": "disabled"
  }
]
```

Each backend accepts one zone. Extra domains, chat domains, backend TCP, DoT,
and DoH are unavailable. Soar's TCP-to-recursive-resolver client option does
not add a TCP listener to its authoritative server. CottenRouter routes all
question types under the zone, including SOA and partial names used by
QNAME-minimising resolvers, without modifying the tunnel payload. Keep the
router query timeout above Soar's held-poll window (up to 1.5 seconds). The
installer requires at least 2 seconds; the example configuration uses 10 seconds.

## Verify and maintain

```bash
sudo systemctl status cottenrouter soar-server soar22-server
sudo journalctl -u soar-server -n 50 --no-pager
sudo journalctl -u soar22-server -n 50 --no-pager
sudo cottenrouter check --config=/etc/cottenrouter/config.json
sudo cottenrouter healthz --config=/etc/cottenrouter/config.json
dig @127.0.0.1 soar.example.com SOA
dig @127.0.0.1 soar22.example.com SOA
```

The SOA response checks routing and server availability. To verify application
traffic, configure a compatible Soar client with the same zone and public key,
working recursive resolvers, and a public TCP destination allowed by the
server. A successful health probe alone does not verify external delegation,
resolver reachability, or the client's tunnel.

`sudo cottenrouter service --project=soar --action=restart` restarts the
backend. `remove --project=soar` detaches it while preserving its data;
`remove --project=soar --purge --confirm=soar` also deletes its managed directory
and private seed.
Use `--project=soar22` and `--confirm=soar22` for the independent Soar 2.2
service and data. Removing one option preserves the other option's service,
route, and private seed.

## Development validation on Windows

Use a dedicated Linux distribution for tests. The 2026-10-18 local validation
used `CottenRouter-Test`, Ubuntu 24.04 with Go 1.25.0 and GCC 13. After firmware
virtualization was enabled, it was converted to WSL2 and tested with systemd
running as PID 1.

Validated in that Linux environment on 2026-10-18:

- The full Go test suite, race detector, static analysis, and CLI build.
- Installer shell syntax, argument parsing, and upgrade-state regressions.
- Generated Soar service syntax through `systemd-analyze verify`.
- Real upstream encrypted Soar streams through shared routing, including race checks.
- The native installer's actual pinned source build, private-seed/public-key
  compatibility, executable startup flags, and authoritative loopback SOA reply.
- Actual Soar installation, configuration, failed-configuration rollback,
  stop/start/restart, identity persistence, detach, reattach, and purge under systemd.
- The hardened service's public-address guard, including the required AF_NETLINK
  access and a negative test without it.
- An HTTP HEAD request through an encrypted upstream client, the installed Soar
  service, and CottenRouter's shared port 53, returning HTTP 200.
- All seven backend installers through the real 80×24 TUI queue, concurrent live UDP routing, supported TCP
  routing, restart recovery, safe uninstall, full purge, and Docker forwarding.

The source-build smoke test is opt-in because it downloads upstream modules:

```bash
COTTENROUTER_TEST_SOURCE_BUILD=1 go test -v -count=1 -timeout=5m \
  -run '^TestSoarPinnedSourceBuildLive$' ./internal/installer
```

See [the systemd validation instructions](../tests/systemd/README.md) for the
gated service tests and the boundaries of the live validation. WSL's synthetic
DNS listener can occupy port 53; use a disposable test distribution and restore
any temporary WSL DNS settings after testing.
