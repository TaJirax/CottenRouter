# CottenRouter

One public port 53 in front of many DNS tunnels.

CottenRouter is a small DNS front router. It reads the first question name of
every query, picks the longest matching configured suffix, forwards the packet
unchanged to that backend's private port, and returns the reply to the original
client. It can share public DoT and HTTPS ports the same way, by reading TLS SNI
and passing the encrypted stream through untouched.

That lets CottenDNS, MasterDnsVPN, StormDNS, thefeed, and SlipGate-managed
transports coexist on a single server and a single IP, each on its own domain,
without any of them fighting over port 53.

CottenRouter adds no DNS labels and no protocol bytes, so it does not reduce
tunnel MTU.

---

## Choose how to run it

|  | **Docker** | **Host install** |
|---|---|---|
| Runs as | unprivileged user (UID 65532) | root service via systemd |
| Gives you | the routing core | routing core **+** installers **+** control deck |
| Backends | you manage them (other containers) | installed and wired for you |
| Best for | existing stacks, low-trust hosts, easy rollback | a fresh server you want set up end to end |

Both run the same router. They differ only in how much of the host the thing is
allowed to touch.

### Docker

The container never runs as root, drops all capabilities, mounts its root
filesystem read-only, and has no shell inside it.

```bash
curl -fsSLO https://raw.githubusercontent.com/TaJirax/CottenRouter/main/docker-compose.yml
curl -fsSLO https://raw.githubusercontent.com/TaJirax/CottenRouter/main/cottenrouter.docker.json
# edit cottenrouter.docker.json and replace the placeholder route
docker compose up -d
```

It listens on unprivileged **5353** inside the container and Docker publishes
host 53 to it, which is why no capability is needed to own a privileged port.

The installers and the control deck are **not** in this path — they manage host
systemd units and host firewall state, which a container has no business doing.
See [`docs/docker.md`](docs/docker.md).

Images: `ghcr.io/tajirax/cottenrouter` (`linux/amd64`, `linux/arm64`).

### Host install

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh | sudo bash
sudo cottenrouter tui
```

The bootstrap checks every required host tool, downloads the exact Go toolchain
`go.mod` declares (verified against the official checksum), runs the test suite,
ensures at least 2 GiB of swap, and starts a safe DNS-only configuration.
Re-running it upgrades in place.

Removal preserves backend and panel data by default:

```bash
sudo cottenrouter-uninstall
```

`--purge --confirm CottenRouter` also deletes the router config.
`--remove-swap --confirm CottenRouter` removes installer-owned swap only.
Port-53 firewall rules are removed only when the installer recorded adding them;
pre-existing rules are never touched. The `cottenrouter` account is deleted on
`--purge` only when an ownership marker shows the installer created it.

---

## How a query is routed

1. Parse the first question name. Malformed queries are dropped.
2. Match the longest configured suffix. Nested suffixes are supported; two
   routes cannot own the same suffix.
3. Rewrite the 16-bit transaction ID to one the router allocated on a connected
   socket for that backend, and forward the packet byte-for-byte.
4. Accept the reply **only** if it echoes the exact question sent under that ID,
   then restore the client's original ID and deliver it.

IDs are never reused within a socket generation. When all 65,536 are spent the
router rotates to a new source port and retires the old socket, so a late reply
can never land in the new generation.

Step 4 is transaction-ID **and** question matching on a connected socket. It is
not cryptographic authentication — the backend itself is trusted.

## What protects the server

Fixed worker queues, global and per-source token buckets, connection caps,
byte-rate budgets, deadlines, and bounded source tracking, all configurable.
Pending queries are bounded and expire. Remote backends are rejected unless
explicitly enabled.

Both `max_packet_size` and `max_tcp_message_size` are capped at **16 KiB**.
DNS-over-TCP frames can in principle carry 65,535 bytes; CottenRouter rejects
longer messages rather than truncating them. The ceiling is a compatibility
limit, not a target packet size — MTU discovery should keep choosing
resolver-safe frames.

See [`docs/security.md`](docs/security.md).

## Configuration

Start from `cottenrouter.example.json`, or `cottenrouter.docker.json` for
containers. Validate before serving — unknown fields are rejected:

```bash
cottenrouter check -config cottenrouter.json
cottenrouter serve -config cottenrouter.json
```

Delegate every tunnel suffix to the same server IP. The suffix is the routing
key, so each backend needs at least one unique domain.

Existing SlipGate setups can be referenced with `slipgate_configs` (see
`cottenrouter.slipgate.example.json`) or converted:

```bash
cottenrouter slipgate-import --input /etc/slipgate/config.json --output cottenrouter.json
```

`cottenrouter catalog` resolves each project's current default branch and prints
the resulting install command. `catalog --offline` shows bundled fallback
metadata and is not meant for installing.

## Backends stay in charge of their own features

CottenRouter is payload-transparent. CottenDNS keeps its DoT, DoH, metrics,
compression, ARQ, MTU discovery, record channels, and SOCKS/TCP forwarding.
SlipGate keeps its keys, certificates, users, SOCKS/SSH backends, NaiveProxy,
StunTLS, and `slipnet://` sharing — only its competing `slipgate-dnsrouter`
service is replaced. thefeed's feed, extra, chat, media, signing, and relay
queries pass through untouched; list every suffix on its route.

One caveat: router-front DoH needs a manually configured certificate and key.
CottenDNS's ACME switch only activates when its own listener is `:443`, which is
false behind the router, and ACME cannot issue on an alternate public port. The
installer fails closed rather than quietly serving a self-signed certificate.

See [`docs/backend-integration.md`](docs/backend-integration.md) before adding
multiple backends.

## Status and monitoring

The control deck refreshes every two seconds with service health, protocols,
queries, bytes, sessions, drops, rate limits, memory, goroutines, and uptime.
The API is loopback-only at `http://127.0.0.1:9088/v1/status`, with `/healthz`
as its probe. In Docker, reach it with `docker compose exec`.

## Build and test

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/cottenrouter
```

CI additionally runs a five-backend isolation and throughput gate, a concurrent
TLS gate covering DoT/DoH and NaiveProxy/StunTLS SNI routing, a gofmt gate, and
a Docker job that builds the image and verifies it starts as a non-root user.

## Known limits

Worth knowing before you deploy this:

- **Host installers run as root.** Upstream project installers are run with
  PATH shims around `fuser`, `iptables`, `nft`, `ufw`, `firewall-cmd`,
  `systemctl`, and `service` to protect panels and firewall state. Those shims
  are a strong default, not a guarantee: a root installer can bypass them with
  an absolute path or a direct syscall. The post-install listener checks are the
  real gate, and they fail closed. Snapshot the server first — or use Docker,
  which sidesteps this entirely.
- **Full host lifecycle testing is not automated.** Fresh install, upgrade,
  mid-install failure, and purge against all five projects with a real panel are
  not yet covered by CI.
- **Routing overhead is measured, not guaranteed.** On a 3 ms representative
  backend RTT the best paired CI trial is within 5% of direct, with a median
  near 4%; shared runners spread it to about 9%. Numbers are logged on every CI
  run. Zero-delay loopback comparisons look far worse and are meaningless — the
  router necessarily adds two datagrams to a one-datagram baseline.

## Documentation

- [`docs/docker.md`](docs/docker.md) — containers, the no-root layout, images
- [`docs/installer.md`](docs/installer.md) — control deck, rollback, panel
  coexistence, ports, resource isolation, removal
- [`docs/backend-integration.md`](docs/backend-integration.md) — suffixes,
  feature boundaries, per-project notes
- [`docs/security.md`](docs/security.md) — flood controls, systemd hardening,
  swap helper

## License

See [`LICENSE`](LICENSE) and [`NOTICE.md`](NOTICE.md).
