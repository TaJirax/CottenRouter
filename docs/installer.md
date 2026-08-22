# Installer and control deck

Run `sudo cottenrouter tui` on Linux. The Install tab collects safe integration
settings, then hands control to the selected project's current native installer
for its advanced settings.

The Project Manager distinguishes selected, installed, and integrated states:

- `space` selects/deselects projects and `i` starts a guided installation for
  every selected project.
- `enter` or `e` edits stable common settings such as all DNS domains, private
  ports, TCP, DoT, DoH, and thefeed chat domains without rerunning an installer.
- `a` opens the complete native config/editor so project-specific transports,
  compression, ARQ, MTU, paths, users, and future upstream options remain
  available. Changes are synchronized, restarted, and rolled back on failure.
- `s` restarts a project, `u` detaches it while preserving files, and `x`
  performs an explicitly confirmed purge.
- `v` shows credential locations/public values. `V` requires confirmation
  before displaying shared secrets on screen.

Each installation resolves the current upstream default branch, inventories
listeners, snapshots configuration, runs the native setup, forces DNS onto
loopback, validates and atomically updates the router, installs resource limits,
and restarts the services. Configuration snapshots are restored on failure.

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

CI repeatedly runs all five DNS backend shapes through one frontend under
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
