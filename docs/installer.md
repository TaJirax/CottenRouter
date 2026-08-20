# Installer and control deck

Run `sudo cottenrouter tui` on Linux. The Install tab collects safe integration
settings, then hands control to the selected project's current native installer
for its advanced settings.

Each installation resolves the current upstream default branch, inventories
listeners, snapshots configuration, runs the native setup, forces DNS onto
loopback, validates and atomically updates the router, installs resource limits,
and restarts the services. Configuration snapshots are restored on failure.

## Ports and panels

- CottenRouter exclusively owns public UDP/TCP 53. The DNS suffix selects the
  private backend without changing its payload.
- When 443 is free, CottenRouter can route DoH by TLS SNI. When a panel owns
  443, CottenDNS uses behind-panel h2c and the panel remains in control. Add a
  reverse proxy for the chosen DoH hostname to the private port reported by the
  installer.
- An occupied 853 is preserved and DoT receives an alternate public port.
- SlipGate's competing DNS router is disabled and persistently prevented from
  reclaiming 53. Its DNS routes are imported and tunnel services keep running.

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
