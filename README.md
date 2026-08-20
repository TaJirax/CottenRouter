# CottenRouter

CottenRouter is a small DNS front router for running CottenDNS, MasterDnsVPN,
StormDNS, thefeed, and SlipGate-managed DNS transports behind one public UDP
and TCP port 53. It can also share public DoT and HTTPS ports by inspecting TLS
SNI and passing each encrypted stream unchanged to the selected CottenDNS or
SlipGate backend. Each backend listens on a
private loopback port. CottenRouter extracts the first query name, selects the
longest matching configured suffix, forwards the datagram, and returns the
reply to the original client.

The router allocates its own DNS transaction IDs per backend. This prevents two
clients that chose the same 16-bit ID from receiving one another's responses.
Pending queries are bounded and expired, malformed queries are dropped, and
remote backends are rejected unless explicitly enabled. Fixed worker queues,
global/per-source token buckets, connection caps, byte-rate budgets, deadlines,
and bounded source tracking protect CPU, RAM, and uplink capacity.

SlipGate's native `/etc/slipgate/config.json` can be loaded directly. Its
DNSTT/NoizDNS, Slipstream, VayDNS, and external DNS routes are imported along
with compatible authenticated reachability/MTU probes. SlipGate continues to
manage keys, certificates, users, SOCKS/SSH backends, NaiveProxy, StunTLS, and
`slipnet://` sharing; only its competing `slipgate-dnsrouter` service is
replaced.

## Current phase

This repository contains the tested routing core and an online-refreshed
catalog of upstream installer URLs/commands. It intentionally does not execute
installers yet. The next phase will add the interactive Linux TUI and
project-specific installation adapters after local routing tests pass.

## Build and test

```bash
go test ./...
go vet ./...
go build ./cmd/cottenrouter
```

## Run locally

Copy `cottenrouter.example.json` to `cottenrouter.json`, replace the example
domains, and start each selected backend on its configured loopback port.

```bash
./cottenrouter check -config cottenrouter.json
sudo ./cottenrouter serve -config cottenrouter.json
```

Use `./cottenrouter catalog` to resolve each repository's current default branch,
verify its installer, and print the resulting latest command. `catalog
--offline` displays bundled fallback metadata but is never intended for an
installation. See
[`docs/backend-integration.md`](docs/backend-integration.md) before installing
multiple backends.

To use an existing SlipGate configuration either reference it with
`slipgate_configs`, as shown in `cottenrouter.slipgate.example.json`, or generate
a standalone router configuration:

```bash
cottenrouter slipgate-import --input /etc/slipgate/config.json --output cottenrouter.json
```

## DNS setup

Delegate every tunnel suffix to the same server IP. The suffix is the routing
key, so each backend must have at least one unique domain. CottenRouter adds no
DNS labels or protocol bytes and therefore does not reduce tunnel MTU.

CottenDNS's DoT, DoH, ACME, metrics, compression, ARQ, MTU discovery, record
channels, and SOCKS/TCP forwarding remain inside CottenDNS. CottenRouter routes
clear DNS-over-TCP on port 53 and can route DoT/DoH streams by SNI while
preserving CottenDNS's TLS and HTTP handling end to end. The same passthrough
supports SlipGate's TLS transports, including NaiveProxy and StunTLS. Likewise,
thefeed's feed, extra, chat, media, signing, and relay queries are
payload-transparent—list every feed/chat suffix on its route.

See [`docs/security.md`](docs/security.md) for flood controls, the hardened
systemd unit, and the idempotent helper that ensures at least 2 GiB of Linux
swap before production deployment.
