#!/usr/bin/env bash
set -euo pipefail

readonly REPOSITORY=TaJirax/CottenRouter
readonly CONFIG_DIR=/etc/cottenrouter
readonly CONFIG_FILE=${CONFIG_DIR}/config.json
readonly BIN_PATH=/usr/local/bin/cottenrouter
readonly LIB_DIR=/usr/local/lib/cottenrouter

if [[ ${EUID} -ne 0 ]]; then
  echo "CottenRouter installation must run as root." >&2
  exit 1
fi
if [[ ! -r /proc/sys/kernel/ostype ]]; then
  echo "CottenRouter server installation supports Linux only." >&2
  exit 1
fi

install_packages() {
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl git golang-go tar
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y ca-certificates curl git golang tar
  elif command -v yum >/dev/null 2>&1; then
    yum install -y ca-certificates curl git golang tar
  else
    echo "Install Go, Git, curl, and tar, then rerun this installer." >&2
    exit 1
  fi
}

if ! command -v go >/dev/null 2>&1 || ! command -v git >/dev/null 2>&1; then
  install_packages
fi

occupied=$(ss -H -lntup 2>/dev/null | awk '$5 ~ /:53$/ && $0 !~ /cottenrouter/ {print}' || true)
if [[ -n ${occupied} ]]; then
  if grep -q 'systemd-resolve' <<<"${occupied}"; then
    install -d -m 0755 /etc/systemd/resolved.conf.d
    printf '[Resolve]\nDNSStubListener=no\n' > /etc/systemd/resolved.conf.d/cottenrouter.conf
    systemctl restart systemd-resolved
  else
    echo "Port 53 has a protected owner. Nothing was stopped:" >&2
    printf '%s\n' "${occupied}" >&2
    exit 1
  fi
fi

work_dir=$(mktemp -d /tmp/cottenrouter-install.XXXXXX)
trap 'rm -rf -- "${work_dir}"' EXIT
branch=$(curl -fsSL -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/${REPOSITORY}" | sed -n 's/.*"default_branch"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)
[[ ${branch} =~ ^[A-Za-z0-9._/-]+$ ]] || { echo "Could not resolve a safe default branch." >&2; exit 1; }
curl -fsSL "https://github.com/${REPOSITORY}/archive/refs/heads/${branch}.tar.gz" -o "${work_dir}/source.tar.gz"
mkdir "${work_dir}/source"
tar -xzf "${work_dir}/source.tar.gz" -C "${work_dir}/source" --strip-components=1

export GOTOOLCHAIN=auto
export GOPROXY=https://proxy.golang.org,direct
(cd "${work_dir}/source" && go test ./... && go build -trimpath -ldflags='-s -w' -o "${work_dir}/cottenrouter" ./cmd/cottenrouter)
install -m 0755 "${work_dir}/cottenrouter" "${BIN_PATH}"

getent group cottenrouter >/dev/null || groupadd --system cottenrouter
id cottenrouter >/dev/null 2>&1 || useradd --system --gid cottenrouter --home-dir /nonexistent --shell /usr/sbin/nologin cottenrouter
install -d -o root -g cottenrouter -m 2750 "${CONFIG_DIR}"
install -d -o root -g root -m 0755 "${LIB_DIR}"
if [[ ! -f ${CONFIG_FILE} ]]; then
  install -o root -g cottenrouter -m 0640 "${work_dir}/source/cottenrouter.bootstrap.json" "${CONFIG_FILE}"
fi
install -m 0755 "${work_dir}/source/scripts/ensure-swap.sh" "${LIB_DIR}/ensure-swap.sh"
install -m 0644 "${work_dir}/source/packaging/cottenrouter.service" /etc/systemd/system/cottenrouter.service
"${LIB_DIR}/ensure-swap.sh"

if command -v ufw >/dev/null 2>&1 && ufw status | grep -qw active; then
  ufw allow 53/udp >/dev/null
  ufw allow 53/tcp >/dev/null
elif command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld; then
  firewall-cmd --permanent --add-port=53/udp >/dev/null
  firewall-cmd --permanent --add-port=53/tcp >/dev/null
  firewall-cmd --reload >/dev/null
fi

systemctl daemon-reload
systemctl enable --now cottenrouter
echo
echo "CottenRouter is installed. Launch the control deck with:"
echo "  sudo cottenrouter tui"
