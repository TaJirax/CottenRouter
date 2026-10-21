# CottenDNS fills its cache but loses the first response

Published update, 2026-10-21: the fix is included in the current
[CottenDNS server and terminal-client release](https://github.com/WhiteDNS/CottenDNS/releases/tag/v2026.10.21.140901-0a7a241),
the [Desktop beta](https://github.com/WhiteDNS/WhiteDNS-Desktop/actions/runs/39601468697/artifacts/12346273079)
and [Android beta](https://github.com/WhiteDNS/WhiteDNS-Android/actions/runs/39604706552/artifacts/12348436491).
See the [release and update notes](cottendns-release-and-update-2026-10-21.md)
for installation, compatibility and mode-switching instructions.

The original defect was confirmed on 2026-10-19 in the local CottenDNS checkout and in the published
WhiteDNS/CottenDNS `v2026.10.08.163633-1f3cc52` client source.

`ProcessDNSQuery` previously dispatched cache misses without retaining the
response callback. `HandleDNSQueryRes` assembled the reply and populated the
cache, but never delivered it to the original caller. Only a later query could
get a cache hit. Both the local UDP DNS listener and SOCKS5 UDP DNS use this
code. CottenRouter forwards the tunnel packets and does not own this cache.

The client fix retains bounded pending callbacks, registers them atomically
with cache lookup, and delivers a completed reply to each waiting caller with
its original DNS transaction ID and RD/CD flags. It copies query buffers reused
by SOCKS UDP, delivers only complete response fragments, and invokes callbacks
outside locks. Pending state expires with the configured response-fragment
timeout, sends SERVFAIL on timeout/capacity exhaustion, and is cleared on
runtime reset. Limits are 4096 pending callers total and 64 per question.

The fix is applied to the separate local `cottenDNS` checkout, both desktop and
Android CottenDNS cores, desktop MasterDNS/StormDNS, and Android PhantomDNS.
Max now carries local DNS through the same server resolver/cache. The focused
[patch](../tests/integration/cottendns/patches/deliver-first-dns-response.patch)
contains only this fix and its unit tests; unrelated local CottenMax work is
excluded. To apply it to a compatible CottenDNS source checkout:

```bash
git apply --check /path/to/CottenRouter/tests/integration/cottendns/patches/deliver-first-dns-response.patch
git apply /path/to/CottenRouter/tests/integration/cottendns/patches/deliver-first-dns-response.patch
```

Build and distribute a corrected **CottenDNS client** to affected users.
Upgrading only CottenRouter or the CottenDNS server cannot repair an already
installed client. CottenRouter v1.2.21's backend updater independently installs
the latest compatible published server, but users still need the corrected
terminal client or Desktop/Android beta build. At the time of the original
2026-10-19 investigation no separate release had been published; that historical
status is superseded by the 2026-10-21 release above.

Validation:

- Before the fix, the regression failed after confirming that the response
  reached the cache while the original callback received nothing.
- Complete CottenDNS unit suites and `go vet` passed on Windows and Ubuntu
  24.04/WSL2. The focused delivery suite passed 20 race-detector repetitions.
- Real client/server binaries passed direct and CottenRouter paths over both
  UDP and TCP. Each path answered eight cold queries and eight cache hits with
  correct IDs, without retransmitting; a controlled delayed upstream received
  exactly one lookup per path. The final binary suite covers both normal
  CottenDNS and Max: eight paths total. Normal uses AES-128-GCM; Max uses its
  authenticated stream transport and the existing shared CottenDNS credential.
- Focused tests also cover simultaneous cache publication, reused query
  buffers, response fragmentation, duplicate replies, callback reentry,
  timeout/reset cleanup, capacity limits and operation without a local cache.

Reproduce the fixed behavior without installing services:

```bash
COTTENDNS_DIR=/path/to/cottenDNS bash tests/integration/cottendns/verify-dns-delivery.sh
```

See the [Max integration and performance report](cottenmax-performance-2026-10-19.md)
for the shared-profile client changes, Soar version, measurements and limits.
