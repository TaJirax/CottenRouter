# Installer and control deck

Run `sudo cottenrouter tui` on Linux. The Project Manager collects integration
settings, then runs the selected project's current native installer or builds
Soar from its resolved upstream Go source.

Install this checkout, including uncommitted updates, from the repository root:

```bash
sudo bash scripts/install.sh --local-source="$PWD"
sudo cottenrouter tui
```

This builds the current local files and stages matching scripts and units. It
does not select a published release. The normal online bootstrap still installs
the latest stable release. Local builds need Go/module download access when
the required toolchain or dependencies are not already available.

The Project Manager distinguishes selected, installed, and integrated states:

- `space` selects/deselects projects and `i` starts a guided installation for
  every selected project.
- `U` updates the current installed backend from its current upstream installer
  while preserving its managed domains, ports and protocol settings. It does
  not update the CottenRouter executable. `Enter` edits configuration instead.
- `enter` or `e` edits stable common settings such as all DNS domains, private
  ports, TCP, DoT, DoH, and thefeed chat domains without rerunning an installer.
- `a` opens the complete native config/editor so project-specific transports,
  compression, ARQ, MTU, paths, users, and future upstream options remain
  available. Changes are synchronized, restarted, and rolled back on failure.
- `s` restarts a project, `u` detaches it while preserving files, and `x`
  performs an explicitly confirmed purge.
- `v` shows credential locations/public values. `V` requires confirmation
  before displaying shared secrets on screen.

All seven projects appear in the same list. The guided queue shows its current
step and keeps invalid entries open for correction. Tab/Shift+Tab and arrow keys
move between fields; Enter on the last field submits. Ctrl+O toggles DoH,
leaving Ctrl+H available for backspace. Key results stay visible until dismissed.
Current local CottenDNS development builds share the existing configuration
and credential between normal and Max mode; no separate Max key is required.
Older experimental identities can still show the validated public key from
`COTTENMAX_KEY_FILE.pub`. Signing seeds stay private, and the existing shared
key keeps its normal `--show-secrets` requirement.
CottenMax stays inside the CottenDNS project and shares its service and route.
A supporting server with CottenMax enabled also accepts normal CottenDNS
clients; users select normal/CottenMax mode in their clients.
See the [integration and measured performance](cottenmax-performance-2026-10-19.md)
for supported settings and the distinction between local builds and releases.
Thefeed's native wizard requires at least one channel/account source. SlipGate
uses its full native wizard. The layout adapts to ordinary 80×24 terminals.

Thefeed installs into `/opt/thefeed` with its native environment file at
`/opt/thefeed/data/thefeed.env`. Main, alias, messenger and MTProto DNS zones
selected in the native wizard are routed together, including future native
`THEFEED_*_DOMAINS` lists. The final native wizard choices take precedence over
the earlier guided form, including domain changes, removed aliases and disabled
messenger domains during an update. CottenRouter assigns the private loopback
listener. Advanced saves refresh those routes without changing native quotas,
relay or media options.

SlipGate installs its manager at `/usr/local/bin/slipgate` and keeps its native
configuration at `/etc/slipgate/config.json`. Its DNS transports, including
Soar, are imported with UDP and TCP ingress; TCP queries bridge to the private
UDP tunnel. NaiveProxy and StunTLS keep TLS passthrough, and enabled WARP stays
managed by SlipGate. New unrelated native settings do not block integration.

Soar and Soar 2.2 each have a guided form for a single delegated zone and private UDP port.
They have no extra/chat domains or TCP/DoT/DoH toggles. The Advanced editor manages
`SOAR_ZONE`, `SOAR_LISTEN`, and `SOAR_ALLOW_PORTS` in
`/opt/cottenrouter/backends/soar/soar.env`. See [Soar setup](soar.md).

Each installation resolves the current upstream default branch (Soar 2.2 uses
WhiteDNS's pinned `v0.1.3` release tag), inventories
listeners, snapshots configuration, runs the native setup, forces DNS onto
loopback, validates and atomically updates the router, installs resource limits,
and restarts the services. Configuration snapshots are restored on failure.
Soar's source build provisions a compatible Go toolchain when needed and
requires network access to Go toolchain/module downloads plus disk space for
the compiler, dependencies, and cache. Its private server seed is created once
and retained on reinstall; `keys --project=soar` displays the public client key.

Downloaded native installers are preserved at their pinned commit for removal.
The MasterDnsVPN execution copy repairs a known startup defect: its one-time
key-generation flags must not enter the persistent service's `ExecStart`.
Key initialization and the original recorded installer remain intact.
For SlipGate, the service guard makes its exact known public resolver drop-in
readable before restarting systemd-resolved, even with a restrictive user umask.
Keys and unrelated native output retain the caller's umask.

## Backend updates independent of CottenRouter

```bash
sudo cottenrouter update --project cottendns
```

Use the same command with another installed project ID to refresh that backend.
Each operation resolves and downloads the current upstream installer; an update
to the CottenRouter binary is not needed whenever a backend publishes a release.
Native installers may still ask their project-specific setup questions. Detached
projects must be configured into the router before this action.

CottenDNS always resolves `WhiteDNS/CottenDNS/releases/latest` at operation time,
passes that exact release tag to its fresh installer, and verifies the executable
that systemd will run reports the same tag. An unavailable release lookup or
version mismatch is an error, rather than a silent fallback to an old version.
The verified release tag is recorded and shown in the TUI. The old
`TaJirax/cottenDNS` fork is not used. Soar 2.2 remains the explicitly pinned older
compatibility option requested by users.
An old `CONFIG_VERSION` inside a preserved CottenDNS config describes that
configuration template; it is not the running executable's release version.

## Ports and panels

- CottenRouter exclusively owns public UDP/TCP 53. The DNS suffix selects the
  private backend without changing its payload.
- When 443 is free, CottenRouter routes DoH there by TLS SNI. When a panel owns
  443, the panel remains untouched and CottenRouter assigns a distinct free
  public TLS port plus a separate loopback backend port. The installer reports
  the alternate public port that clients must use. CottenDNS router-front DoH
  requires a configured certificate/key: its current ACME implementation only
  activates when its own local listener is `:443`, and ACME cannot issue on an
  alternate public port. The installer fails closed instead of silently relying
  on a self-signed certificate.
- An occupied 853 is preserved and DoT receives an alternate public port.
- SlipGate's competing DNS router is disabled and persistently prevented from
  reclaiming 53. Its DNS routes are imported and tunnel services keep running.
  Generated DNSTT/VayDNS private listeners are rewritten from wildcard to
  loopback after native setup so they cannot bypass CottenRouter. During the
  native SlipGate wizard, its destructive port helper is denied access to 53,
  443, 853, and every listener already owned by another running service; an
  alternate port must be chosen instead of stopping a panel or Xray inbound.
- Panel and firewall protection during native setup is enforced with PATH shims
  around `fuser`, `iptables`, `nft`, `ufw`, `firewall-cmd`, `systemctl`, and
  `service`. Upstream installers run as root and can call these tools by
  absolute path or through syscalls, so the shims are a strong default, not a
  guarantee. The post-install listener checks are the actual gate: they fail
  closed if an upstream release violates CottenRouter's ownership plan.
  Snapshot the server before installing an unreviewed upstream release.

## Safeguards

The router applies global/per-source query buckets, fixed queues, bounded
pending maps, byte budgets, connection caps, deadlines, and a 16 KiB packet
ceiling. CottenDNS receives matching TCP, DoH, encrypted connection, session,
stream, cache and queue caps. thefeed retains its native message/session quotas
and gets a finite persistent-account ceiling. Managed backends share a systemd
slice capped at a 1 GiB memory high watermark, 1.5 GiB hard maximum, three CPUs,
and 4096 tasks. At least 2 GiB of swap is an emergency cushion, not capacity.

## Status

The Overview refreshes every two seconds and shows service health, configured
protocols, queries, bytes, current/total sessions, drops, rate limits, router
memory, goroutines, and uptime. The loopback-only API is available at
`http://127.0.0.1:9088/v1/status`; `/healthz` is its health probe.

## Compatibility gate

CI repeatedly runs DNS backend shapes through one frontend under
parallel load. The gate requires every reply to retain its backend marker and
original transaction ID, accepts no loss, exercises the full 16 KiB datagram,
and rejects throughput below 100 local queries/second. A separate concurrent
TLS gate covers CottenDNS DoT/DoH and SlipGate NaiveProxy/StunTLS SNI routes.
These tests measure router overhead and isolation; real Internet throughput is
still bounded by the resolver, uplink, and each backend's own implementation.

## Packet ceilings and performance

The 16 KiB UDP ceiling is a compatibility limit, not a target packet size.
CottenDNS MTU discovery should continue choosing resolver-safe frames (often a
few hundred bytes). Increasing the ceiling further raises pooled-buffer RAM,
fragmentation, loss, and amplification exposure without improving normal
tunnel throughput. DNS-over-TCP frames can in principle carry 65,535 bytes, but
CottenRouter caps `max_tcp_message_size` at 16 KiB as well: both `max_packet_size`
and `max_tcp_message_size` are validated to the range 512-16384. Longer TCP
messages are rejected, not truncated. Throughput should be tuned with
worker/concurrency/rate budgets only after observing the dashboard.

## Removing CottenRouter itself

`sudo cottenrouter-uninstall` removes the router binary, service, resource
slice, and CottenRouter-owned service drop-ins. It preserves the router config,
all upstream project data, panels, pre-existing firewall rules, and swap by
default. Port-53 rules the installer itself added are removed, using the
ownership marker it wrote at install time; nothing else in the firewall is
touched. On `--purge` the `cottenrouter` account is deleted only when the
installer's account ownership marker records that the installer created it.
Use `--purge --confirm CottenRouter` for the router config and
`--purge --purge-backends --confirm CottenRouter` to additionally remove all
managed DNS backend services, pinned installers, and backend data. Use
`--remove-swap --confirm CottenRouter` only when the swap ownership marker is
present. Backend projects have their own separately confirmed removal workflow
in the Project Manager.
