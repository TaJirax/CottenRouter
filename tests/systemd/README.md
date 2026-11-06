# Disposable WSL2 validation

These opt-in tests change real Linux services and paths. They refuse to run
outside the explicitly named `CottenRouter-Test` WSL distribution as root. Use
Ubuntu 24.04, systemd as PID 1, Go 1.25+, GCC, Python 3, curl, git and iproute2.
Run from a Linux copy of the current checkout. Do not use a production server.

On 2026-10-18, the tests below passed under WSL2 after SVM/Hyper-V was enabled.
WSL's synthetic DNS proxy occupied UDP 53; DNS tunneling was temporarily disabled
and a working upstream resolver configured. The installer correctly refused the
unrelated port owner before that adjustment. Restore temporary WSL settings at
the end of a run.

## Core installation and Soar lifecycle

Start without an existing router installation:

```bash
COTTENROUTER_SYSTEMD_TEST=1 bash tests/systemd/bootstrap-test.sh
COTTENROUTER_TEST_SYSTEMD=1 go test -v -count=1 -timeout=10m \
  -run '^TestSoarSystemdLifecycle$' ./internal/installer
(cd tests/integration/soar && COTTENROUTER_TEST_SYSTEMD=1 \
  go test -v -count=1 -run '^TestSoarSystemdDestinationGuard$')
```

The bootstrap test builds the current checkout and substitutes only release
transport with local checksummed artifacts. It runs the actual installer,
unprivileged service, health probes, upgrade of a root-only existing config,
and recovery from a checksum-valid executable that fails at startup. Its logs
persist under `/root/cottenrouter-validation/bootstrap.*`. It leaves the router
installed. The Soar lifecycle test builds actual upstream source and checks
configuration rollback against both restored files and the running listener,
identity persistence, stop/start/restart, detach, reattach and purge.

## Official Soar and WhiteDNS Soar 2.2 coexistence

After a fresh bootstrap, with neither Soar backend installed:

```bash
COTTENROUTER_TEST_SYSTEMD=1 go test -v -count=1 -timeout=15m \
  -run '^TestSoarSystemdCoexistence$' ./internal/installer
COTTENROUTER_TEST_WHITEDNS_BUNDLE=1 go test -v -count=1 -timeout=4m \
  -run '^TestWhiteDNSSoarBundleLive$' ./internal/installer
```

On 2026-10-18, both real systemd runtimes served independent zones on ports
5305/5306. Reinstalling, configuring, restarting, detaching, reattaching and
purging Soar 2.2 preserved official Soar's key, configuration, binary, unit,
route and running PID. An injected activation failure restored the legacy
configuration and live listener. The bundle test also verified the packaged
older server accepts the 197-character zone boundary; the standalone wizard's
shorter profile limit does not apply to CottenRouter's configuration surface.

The coexistence test purges both backends and restores the bootstrap router.
To test application traffic, install `soar` with zone `soar.systemd.test` and
`soar22` with zone `soar22.systemd.test`, then run:

```bash
(cd tests/integration/soar && COTTENROUTER_TEST_SYSTEMD_NETWORK=1 \
  go test -v -count=1 -run '^TestInstalled.*PublicHTTP$')
```

Both versions returned HTTP 200 concurrently through encrypted tunnels on the
same public DNS port. These probes use the pinned upstream v0.2.3 client against
the official source server and the actual WhiteDNS v0.2.2 packaged server.

## All native backends together

For the September-to-current CottenDNS update regression, start with no router
or backend installation and run:

```bash
COTTENROUTER_SYSTEMD_TEST=1 bash tests/systemd/cottendns-update-test.sh
```

This installs a real checksum-verified September 26 server, makes its executable
newer by mtime than the current release, and runs the backend-only update. On
2026-10-18 it selected and verified the October 8 WhiteDNS release while keeping
the key, backend/router config and CottenRouter executable checksums unchanged.
It leaves the one updated backend installed for inspection and cleanup.

The current checkout can be installed and tested through the actual 80×24 TUI:

```bash
bash scripts/install.sh --local-source="$PWD" --no-swap
COTTENROUTER_SYSTEMD_TEST=1 python3 tests/systemd/tui-install-test.py
```

On 2026-10-18, the PTY test installed all seven projects at 80×24 with a
restrictive umask of 077. It submitted each guided form, answered the real
thefeed/SlipGate prompts with disposable test data, read persistent public key
results, and returned to the TUI. It requires no pre-existing backend data.
This exposed and verified a repair for SlipGate's root-only public resolver
drop-in, which otherwise made systemd-resolved reopen port 53.
The default online bootstrap selects a published release; `--local-source`
builds the current checkout and its uncommitted changes.

For the current Feed and SlipGate feature setup, start after bootstrap with
neither backend installed, then run:

```bash
COTTENROUTER_SYSTEMD_TEST=1 python3 tests/systemd/feed-slipgate-install-test.py
```

This PTY harness selects Feed aliases, messenger, MTProto and DNS media, plus
SlipGate DNSTT and built-in Soar. It checks native installation paths, retained
quotas, imported domains, TCP-to-UDP mode and active services. It requires public
port 53 to be available in the test distro, as the normal installer does.

The isolated native checks can also run when the host already owns port 53:

```bash
COTTENROUTER_NAMESPACE_TEST=1 \
  COTTENROUTER_BINARY=/path/to/cottenrouter \
  SLIPGATE_BINARY=/path/to/slipgate-linux-amd64 \
  bash scripts/native-slipgate-contract-test.sh

COTTENROUTER_NAMESPACE_TEST=1 \
  COTTENROUTER_BINARY=/path/to/cottenrouter \
  THEFEED_BINARY=/path/to/thefeed-server-linux-amd64 \
  THEFEED_INSTALLER=/path/to/thefeed/scripts/install.sh \
  bash scripts/native-thefeed-contract-test.sh
```

Both require root and Linux mount, PID and network namespaces. They leave host
paths and networking intact. On November 6, 2026, the current Feed source and
installer (`a9dea470`) passed native wizard, generated-unit, identity/settings
preservation and opaque UDP routing checks for all four domain types. SlipGate
v1.6.8 passed native Soar creation, key/unit paths and byte-exact signed TXT/AAAA
replies through public UDP and TCP. Systemd/package/download boundaries are
shimmed; external Telegram login, MTProto connections, media downloads, public
DNS delegation and real systemd activation remain separate integration checks.

The additional Telegram transport regression uses the real Feed v0.42.0 client
and server with a namespace-local test DC, without account authentication or a
public Telegram connection:

```bash
COTTENROUTER_NAMESPACE_TEST=1 \
  COTTENROUTER_BINARY=/path/to/cottenrouter \
  THEFEED_BINARY=/path/to/thefeed-server-linux-amd64 \
  THEFEED_SOURCE=/path/to/thefeed-v0.42.0 \
  bash scripts/native-thefeed-telegram-test.sh
```

Feed's source requires its own compatible Go toolchain to build the test client;
the Router module is unchanged. The probe checks signed discovery, sealed
handshakes and bidirectional streams in twelve direct/routed encoding/cell-budget
cases. It does not validate real Telegram login or public-network reachability.

For the current stable WhiteDNS Panel, download its native AMD64 archive and
checksum, then run:

```bash
COTTENROUTER_NAMESPACE_TEST=1 \
  PANEL_ARCHIVE=/path/to/soar-panel-0.14.4-linux-amd64.tar.gz \
  PANEL_CHECKSUM=/path/to/soar-panel-0.14.4-linux-amd64.tar.gz.sha256 \
  bash scripts/native-panel-installer-contract-test.sh
```

This invokes real Router install/update/configure/Advanced operations against the
published Panel bundle. It checks native account identities, keys, quotas and
per-engine limits, serving-zone changes, live private DNS UDP/TCP and rollback
after an injected route conflict. Release transport and systemd are substituted
inside private namespaces; generated service commands run actual processes.
`scripts/native-panel-contract-test.sh` separately compares native DNS replies
with routed replies using a supplied Router binary.

Use the real CLI/native installers in a WSL terminal:

```bash
cottenrouter install --project cottendns --domain cottendns.systemd.test --tcp
cottenrouter install --project masterdnsvpn --domain master.systemd.test
cottenrouter install --project stormdns --domain storm.systemd.test
cottenrouter install --project thefeed --domain feed.systemd.test
cottenrouter install --project slipgate
cottenrouter install --project soar --domain soar.systemd.test
cottenrouter install --project soar22 --domain soar22.systemd.test
```

For thefeed, use a public channel (the local run used `@telegram`), a disposable
passphrase, no private channels, no external relays, and the mode without Telegram
login. At least one source is required. For SlipGate, choose DNSTT, SOCKS,
`slip.systemd.test`, MTU 1232, no new SSH user, and no WARP. The native installation
and actual private listeners use the same guided installation path.

```bash
COTTENROUTER_SYSTEMD_TEST=1 python3 tests/systemd/native-routing.py
(cd tests/integration/soar && COTTENROUTER_TEST_SYSTEMD_NETWORK=1 \
  go test -v -count=1 -run '^TestInstalled.*PublicHTTP$')
```

The routing check compares 50 UDP replies from each native backend byte for byte
with its direct reply, concurrently with identical transaction IDs across zones.
It verifies CottenDNS TCP forwarding and explicit TCP SERVFAIL for UDP-only
routes. Both installed Soar probes returned HTTP 200 concurrently through
encrypted traffic on the real shared port 53. The default isolated Soar suite additionally verifies
four 32 KiB encrypted echo streams over two zones, with race detection.

## Docker and removal

With a Docker daemon inside the test distro:

```bash
docker build -t cottenrouter:systemd-test .
COTTENROUTER_SYSTEMD_TEST=1 python3 tests/systemd/docker-test.py
# Removes the router and all seven test backends after checking preservation first:
COTTENROUTER_SYSTEMD_TEST=1 bash tests/systemd/removal-test.sh
```

Docker was verified with UID/GID 65532, a read-only filesystem, all capabilities
dropped, health checks, and real UDP/TCP forwarding to a local fixture. Removal
verified restart recovery, default uninstall preserving every configuration/key
byte and live backend, then full purge removing services, data, binaries and the
installer-created accounts.

The full Go suite, `go vet`, race detector, build, shell parsing and installer state
regressions passed. WSL testing does not validate public DNS delegation, a real
certificate authority's issuance, private Telegram login, every optional
SlipGate transport/WARP, or every backend's end-user client protocol. The
separate WhiteDNS panel was reviewed for compatibility, not installed as a
replacement for the official Lantern engine.
