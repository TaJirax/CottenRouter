# Resource and abuse safeguards

CottenRouter treats all public listeners as hostile. Its defaults bound work
before parsing or forwarding traffic:

- UDP uses a fixed worker pool and fixed queue. Excess packets are dropped.
- Global and per-source token buckets cap queries. The source table is bounded.
- Global ingress and response byte budgets limit amplification and TLS relay
  bandwidth in both directions.
- DNS transaction maps, packet sizes, TCP connections, connections per source,
  queries per connection, and in-flight queries are all capped.
- Read/write deadlines evict slow clients. Backends are loopback-only unless
  `allow_remote_backends` is deliberately enabled.
- TLS ClientHello parsing is size-limited and encrypted streams are routed
  without terminating TLS, so CottenRouter never stores backend private keys.

Tune the `limits` block for the server's CPU and uplink. A public recursive
resolver can make many end users appear under one source IP; only add its CIDR
to `trusted_resolver_cidrs` when necessary. Trusted entries bypass the
per-source query bucket, but never global query or bandwidth limits.

The sample systemd unit adds a 768 MiB hard memory ceiling, a two-CPU quota,
task/file limits, automatic restart, and service sandboxing. Adjust those
values with a systemd drop-in for larger deployments. The 2 GiB swap helper is
an emergency cushion, not capacity: sustained overload is intentionally
dropped before it can allocate unbounded memory.

```bash
sudo ./scripts/ensure-swap.sh
sudo install -m 0644 packaging/cottenrouter.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cottenrouter
```

`ensure-swap.sh` is idempotent. If total active Linux swap is below 2 GiB, it
creates a dedicated, mode-0600 file at `/var/lib/cottenrouter/swapfile`, enables
it, persists it in `/etc/fstab`, and verifies the resulting total.
