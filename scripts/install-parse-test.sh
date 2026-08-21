#!/usr/bin/env bash
# Checks the installer's config-parsing helpers against a fixture, so the
# port enumeration that drives preflight, health checks and firewall rules
# cannot silently break. Run: bash scripts/install-parse-test.sh
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# Take the helper definitions from install.sh itself rather than copying them.
eval "$(sed -n '/^json_string() {/p;/^addr_port() {/p' "${script_dir}/install.sh")"
[[ $(type -t json_string) == function && $(type -t addr_port) == function ]] ||
  { echo "FAIL: could not extract helpers from install.sh" >&2; exit 1; }

fixture=$(mktemp)
trap 'rm -f -- "${fixture}"' EXIT
cat > "${fixture}" <<'JSON'
{
  "listen_udp": "0.0.0.0:5353",
  "admin_listen": "127.0.0.1:9090",
  "tls_listeners": [
    {"name": "dot", "listen": "0.0.0.0:853", "routes": []}
  ],
  "routes": []
}
JSON

assert() {
  [[ $2 == "$3" ]] || { printf 'FAIL %s: got %q want %q\n' "$1" "$2" "$3" >&2; exit 1; }
}

assert listen_udp "$(json_string listen_udp "${fixture}")" 0.0.0.0:5353
assert admin_listen "$(json_string admin_listen "${fixture}")" 127.0.0.1:9090
# listen_tcp is optional: an absent key must yield an empty string, not :53.
assert listen_tcp "$(json_string listen_tcp "${fixture}")" ""
assert udp_port "$(addr_port "$(json_string listen_udp "${fixture}")")" 5353
assert admin_port "$(addr_port "$(json_string admin_listen "${fixture}")")" 9090
assert ipv6_port "$(addr_port '[::1]:8600')" 8600

mapfile -t tls_addrs < <(grep -o '"listen"[[:space:]]*:[[:space:]]*"[^"]*"' "${fixture}" | sed 's/.*"\([^"]*\)"$/\1/' || true)
assert tls_count "${#tls_addrs[@]}" 1
assert tls_port "$(addr_port "${tls_addrs[0]}")" 853

echo "install.sh config parsing OK"
