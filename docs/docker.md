# Running CottenRouter in Docker

The container runs the **routing core only**, as an unprivileged user, with no
installer and no access to the host. Use it when you want CottenRouter in front
of backends you already manage yourself.

## What the container does not do

The Bubble Tea control deck and the per-project installers are deliberately
absent from this path. They install upstream projects as host systemd services,
rewrite host firewall state, and manage host accounts. None of that is
meaningful — or safe — from inside a container, and shelling out to the host to
fake it would hand the container exactly the root it is supposed to avoid.

In Docker you own backend lifecycle. Run CottenDNS, StormDNS, SlipGate and the
rest as their own containers, and point routes at them by service name. If you
want the guided installer, use the host install described in the README.

## Quick start

```bash
curl -fsSLO https://raw.githubusercontent.com/TaJirax/CottenRouter/main/docker-compose.yml
curl -fsSLO https://raw.githubusercontent.com/TaJirax/CottenRouter/main/cottenrouter.docker.json
# edit cottenrouter.docker.json: replace the placeholder route
docker compose up -d
```

Check it came up:

```bash
docker compose exec cottenrouter /usr/local/bin/cottenrouter healthz \
  -config /etc/cottenrouter/config.json
```

## Why it never needs root

The router listens on **5353** inside the container, not 53. 5353 is
unprivileged, so the process needs neither root nor `CAP_NET_BIND_SERVICE`. The
compose file publishes host `53` to container `5353`; Docker's own proxy is what
binds the privileged host port.

That is why the container can run with `user: 65532`, `cap_drop: ALL`,
`read_only: true`, and `no-new-privileges`. Nothing in the image needs to write
to its own filesystem, and the image itself has no shell and no package manager
(`gcr.io/distroless/static:nonroot`), so there is nothing to exec into if it is
ever reached.

If you prefer `network_mode: host`, the router must bind 53 directly. Then you
do need `cap_add: [NET_BIND_SERVICE]` and `listen_udp`/`listen_tcp` set back to
`:53`. That trades the isolation above for one fewer hop; the published-port
layout is the recommended one.

## Configuration

`cottenrouter.docker.json` differs from the host config in two ways:

- **Ports are 5353**, for the reason above.
- **`allow_remote_backends` is `true`.** The host build requires loopback
  backends, because on a single host anything else means the router is
  forwarding off-box. In Docker your backends are other containers with their
  own IPs, so that check cannot apply. Compensate at the network layer: keep
  backends on the compose network and publish none of their ports to the host.
  Only the router should be reachable from outside.

The admin API stays bound to `127.0.0.1:9088` and is validated as loopback-only,
so it is reachable from inside the container and nowhere else. Read it with
`docker compose exec`, not a published port.

## Routes

Point each route at a container name on the shared network:

```json
{
  "name": "cottendns",
  "domains": ["tunnel.example.com"],
  "backend": "cottendns:5301",
  "tcp_backend": "cottendns:5301"
}
```

Add the backend to `docker-compose.yml` on the same `backends` network, with no
`ports:` entry of its own.

## Images

Published to `ghcr.io/tajirax/cottenrouter` for `linux/amd64` and `linux/arm64`
on every release tag. `:1`, `:1.0`, `:1.0.0`, and `:latest` are all published;
pin at least the major tag in production.

## Limits

`mem_limit` and `pids_limit` in the compose file are a blast-radius cap, not a
capacity plan. The router's own budgets — worker counts, token buckets,
connection caps — are what you actually tune, and they live in the config file.
Read the dashboard before changing either.

## Updating

```bash
docker compose pull && docker compose up -d
```

Configuration is a read-only bind mount, so it survives image replacement
untouched. There is no in-container upgrade path and no installer state to
migrate; the container is disposable by design.
