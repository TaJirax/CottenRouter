# CottenRouter

CottenRouter is a small DNS front router for running CottenDNS, MasterDnsVPN,
StormDNS, thefeed, and SlipGate-managed DNS transports behind one public UDP
and TCP port 53. It can also share public DoT and HTTPS ports by inspecting TLS
SNI and passing each encrypted stream unchanged to the selected CottenDNS or
SlipGate backend. Each backend listens on a
private loopback port. CottenRouter extracts the first query name, selects the
longest matching configured suffix, forwards the datagram, and returns the
reply to the original client.

The router allocates its own DNS transaction IDs per connected backend socket
and never reuses an ID within one socket generation; once all 65,536 IDs are
issued it rotates to a new source port and retires the old socket. A reply is
accepted only when it echoes the exact question that was sent under that ID, so
a reply guessing the ID alone cannot consume or hijack a client's query. This
prevents two clients that chose the same 16-bit ID from receiving one another's
responses. It is transaction-ID and question matching on a connected socket,
not cryptographic authentication: a backend itself is trusted.
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

This repository includes the routing core, transactional project-specific
installers, and a Bubble Tea server control deck. Installers resolve the latest
upstream default branch at run time, retain each project's native advanced
setup, move public listeners to private loopback ports, and atomically update
the shared router configuration. Existing 3x-ui/x-ui/Hiddify port 443 listeners
are protected.

On a clean Linux server:

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh | sudo bash
sudo cottenrouter tui
```

The bootstrap checks every required host tool, uses the exact Go toolchain
declared by `go.mod` (with an official checksum-verified download when needed),
runs the test suite, creates at least 2 GiB of swap, and starts a safe DNS-only
configuration. Re-running it upgrades and explicitly restarts the active
service.

Remove only CottenRouter while preserving backend and panel data:

```bash
sudo cottenrouter-uninstall
```

Pass `--purge --confirm CottenRouter` to delete the router configuration too.
Installer-owned swap is removed only with the separate, explicitly confirmed
`--remove-swap` option. Port-53 firewall rules are removed only when the
installer recorded that it added them; rules that already existed, and all
upstream project data, are never deleted by the router uninstaller. The
`cottenrouter` service account is deleted on `--purge` only when the installer's
ownership marker shows it created that account.

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
installation. The router accepts bounded UDP datagrams up to 16 KiB while
keeping its fixed queue small enough to preserve the memory budget. See
[`docs/backend-integration.md`](docs/backend-integration.md) before installing
multiple backends.

See [`docs/installer.md`](docs/installer.md) for rollback, panel coexistence,
resource isolation, project selection/editing/uninstall/key workflows, packet
ceiling guidance, and status API details.

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

CottenDNS's DoT, DoH, metrics, compression, ARQ, MTU discovery, record
channels, and SOCKS/TCP forwarding remain inside CottenDNS. CottenRouter routes
clear DNS-over-TCP on port 53 and can route DoT/DoH streams by SNI while
preserving CottenDNS's configured TLS certificate and HTTP handling end to end.
Router-front DoH is gated on a manual certificate/key until upstream exposes
its external ACME port; alternate public ports cannot use ACME. The same passthrough
supports SlipGate's TLS transports, including NaiveProxy and StunTLS. Likewise,
thefeed's feed, extra, chat, media, signing, and relay queries are
payload-transparent—list every feed/chat suffix on its route.

See [`docs/security.md`](docs/security.md) for flood controls, the hardened
systemd unit, and the idempotent helper that ensures at least 2 GiB of Linux
swap before production deployment.
