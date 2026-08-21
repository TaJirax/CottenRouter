#!/usr/bin/env bash
set -euo pipefail

readonly REPOSITORY=TaJirax/CottenRouter
readonly CONFIG_DIR=/etc/cottenrouter
readonly CONFIG_FILE=${CONFIG_DIR}/config.json
readonly BIN_PATH=/usr/local/bin/cottenrouter
readonly UNINSTALL_BIN_PATH=/usr/local/bin/cottenrouter-uninstall
readonly LIB_DIR=/usr/local/lib/cottenrouter
readonly UNIT_PATH=/etc/systemd/system/cottenrouter.service
readonly RESOLVED_DROPIN=/etc/systemd/resolved.conf.d/cottenrouter.conf
readonly STATE_DIR=/var/lib/cottenrouter
readonly RESOLVED_MARKER=${STATE_DIR}/resolved-dropin.created-by-cottenrouter
readonly SWAP_FILE=${STATE_DIR}/swapfile
readonly SWAP_MARKER=${STATE_DIR}/swapfile.created-by-cottenrouter
readonly RESOLVED_MARKER_VALUE=cottenrouter-resolved-v1
readonly ACCOUNT_MARKER=${STATE_DIR}/account.created-by-cottenrouter
readonly ACCOUNT_MARKER_VALUE=cottenrouter-account-v1
readonly FIREWALL_MARKER=${STATE_DIR}/firewall.created-by-cottenrouter

# Standardised download behaviour: bounded, retried, never hangs forever.
fetch() {
  curl --fail --location --silent --show-error \
       --retry 5 --retry-all-errors \
       --connect-timeout 10 --max-time 300 "$@"
}

fail() {
  printf 'CottenRouter installer: %s\n' "$*" >&2
  exit 1
}

if [[ ${EUID} -ne 0 ]]; then
  fail "installation must run as root"
fi
if [[ ! -r /proc/sys/kernel/ostype ]]; then
  fail "server installation supports Linux only"
fi
if [[ -L ${CONFIG_DIR} || -L ${LIB_DIR} || -L ${STATE_DIR} ]]; then
  fail "refusing symlinked CottenRouter configuration, library, or state directory"
fi

swap_mode=auto
channel=stable
requested_version=""
build_mode=auto
usage() {
  cat <<'USAGE'
Usage: install.sh [options]

  --version=vX.Y.Z     install this tagged release (implies --channel=stable)
  --channel=stable     install the latest tagged release (default)
  --channel=edge       build the current default-branch commit from source
  --build-from-source  compile locally even when a release binary exists
  --no-swap            skip the swap safeguard entirely
USAGE
}
for arg in "$@"; do
  case ${arg} in
    --no-swap) swap_mode=off ;;
    --build-from-source) build_mode=source ;;
    --channel=stable|--channel=edge) channel=${arg#--channel=} ;;
    --version=*)
      requested_version=${arg#--version=}
      channel=stable
      [[ ${requested_version} =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "--version expects a tag such as v1.2.3"
      ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; fail "unknown option: ${arg}" ;;
  esac
done
[[ ${channel} != edge ]] || build_mode=source

package_manager=""
if command -v apt-get >/dev/null 2>&1; then
  package_manager=apt
elif command -v dnf >/dev/null 2>&1; then
  package_manager=dnf
elif command -v yum >/dev/null 2>&1; then
  package_manager=yum
else
  fail "supported servers require systemd and apt, dnf, or yum (the bundled upstream installers do not support pacman/apk)"
fi

# The bootstrap downloads the exact Go version declared by go.mod when the
# host does not already have it. Distribution Go packages are intentionally not
# used: an older compiler can neither parse nor safely build a newer module.
install_base_packages() {
  if [[ ${package_manager} == apt ]]; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y \
      ca-certificates coreutils curl git gzip iproute2 libc-bin passwd tar util-linux
  elif [[ ${package_manager} == dnf ]]; then
    dnf install -y \
      ca-certificates coreutils curl git gzip iproute procps-ng shadow-utils tar util-linux
  elif [[ ${package_manager} == yum ]]; then
    yum install -y \
      ca-certificates coreutils curl git gzip iproute procps-ng shadow-utils tar util-linux
  fi
}

required_commands=(
  awk chmod chown cmp cp curl dd df fallocate flock getent git grep groupadd head id install
  mkdir mktemp mkswap mv rm sed sha256sum sleep ss stat swapoff swapon systemctl tail tar uname useradd
)
missing_commands=()
for command_name in "${required_commands[@]}"; do
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    missing_commands+=("${command_name}")
  fi
done
if (( ${#missing_commands[@]} > 0 )); then
  printf 'Installing missing base tools: %s\n' "${missing_commands[*]}"
  install_base_packages
fi
for command_name in "${required_commands[@]}"; do
  command -v "${command_name}" >/dev/null 2>&1 || fail "required command is still missing: ${command_name}"
done
[[ -d /run/systemd/system ]] || fail "systemd is not running; CottenRouter requires a systemd server"
# The shipped unit relies on AmbientCapabilities, MemoryHigh and OOMPolicy.
# systemd 245 (Ubuntu 20.04, Debian 11, RHEL/Rocky/Alma 9) is the floor;
# older releases silently ignore directives the sandbox depends on.
systemd_version=$(systemctl --version 2>/dev/null | awk 'NR == 1 { print $2; exit }')
if [[ ${systemd_version} =~ ^[0-9]+$ ]]; then
  (( systemd_version >= 245 )) || fail "systemd ${systemd_version} is too old; CottenRouter needs systemd 245 or newer (Ubuntu 20.04+, Debian 11+, RHEL/Rocky/Alma 9+)"
else
  printf 'Warning: could not determine the systemd version; assuming it is 245 or newer.\n' >&2
fi
is_wsl() { grep -qiE 'microsoft|wsl' /proc/sys/kernel/osrelease 2>/dev/null; }
if is_wsl; then
  printf 'Note: WSL detected. CottenRouter supports WSL for development/testing only; its UDP stack and virtual DNS differ from a native Linux server.\n' >&2
fi
# Serialise concurrent `curl | bash` runs: they would otherwise race over
# staging files, systemd state, backups, swap and firewall rules.
mkdir -p /run/lock
exec 9>/run/lock/cottenrouter-install.lock
flock -n 9 || fail "another CottenRouter installation is already running"
for managed_dir in "${CONFIG_DIR}" "${LIB_DIR}" "${STATE_DIR}"; do
  if [[ -e ${managed_dir} || -L ${managed_dir} ]]; then
    [[ -d ${managed_dir} && ! -L ${managed_dir} ]] || fail "managed path is not a safe directory: ${managed_dir}"
    [[ $(stat -c '%u' "${managed_dir}") == 0 ]] || fail "managed directory is not root-owned: ${managed_dir}"
  fi
done

# Pick the tmp root with the most free space so the Go build has room.
_tmp_free_tmp=$(df --output=avail /tmp 2>/dev/null | tail -1 || echo 0)
_tmp_free_var=$(df --output=avail /var/tmp 2>/dev/null | tail -1 || echo 0)
if (( _tmp_free_var > _tmp_free_tmp )); then
  _tmpbase=/var/tmp
else
  _tmpbase=/tmp
fi
# Disk preflight before anything is downloaded. A release binary plus the
# source archive needs little; a local Go build is checked again later.
_tmp_avail=$(df --output=avail "${_tmpbase}" 2>/dev/null | tail -1 || echo 0)
(( _tmp_avail >= 131072 )) || fail "not enough free space on ${_tmpbase} (need at least 128 MiB, have $((_tmp_avail / 1024)) MiB)"
work_dir=$(mktemp -d "${_tmpbase}/cottenrouter-install.XXXXXX")
backup_dir=${work_dir}/rollback
mkdir -m 0700 "${backup_dir}"
transaction_started=false
committed=false
prior_router_active=false
prior_resolved_active=false
prior_router_enabled=not-found
resolved_changed=false
swap_preexisting=false
swap_was_active=false
firewall_kind=none
firewall_added_ports=()
user_created=false
group_created=false

path_present() { [[ -e $1 || -L $1 ]]; }

backup_file() {
  local label=$1 path=$2
  if path_present "${path}"; then
    [[ ! -d ${path} || -L ${path} ]] || fail "managed file path is a directory: ${path}"
    printf 'present\n' > "${backup_dir}/${label}.state"
    cp -a -- "${path}" "${backup_dir}/${label}"
  else
    printf 'absent\n' > "${backup_dir}/${label}.state"
  fi
}

restore_file() {
  local label=$1 path=$2
  rm -f -- "${path}" "${path}.new"
  if [[ $(<"${backup_dir}/${label}.state") == present ]]; then
    cp -a -- "${backup_dir}/${label}" "${path}"
  fi
}

remove_transaction_swap() {
  if ${swap_preexisting}; then
    if ! ${swap_was_active} && swapon --show=NAME --noheadings 2>/dev/null | grep -Fxq "${SWAP_FILE}"; then
      swapoff "${SWAP_FILE}" >/dev/null 2>&1 || true
    fi
    return 0
  fi
  [[ -f ${SWAP_MARKER} && ! -L ${SWAP_MARKER} ]] || return 0
  [[ -f ${SWAP_FILE} && ! -L ${SWAP_FILE} ]] || return 0
  if swapon --show=NAME --noheadings 2>/dev/null | grep -Fxq "${SWAP_FILE}"; then
    swapoff "${SWAP_FILE}" >/dev/null 2>&1 || return 0
  fi
  rm -f -- "${SWAP_FILE}" "${SWAP_MARKER}"
  if [[ -f /etc/fstab && ! -L /etc/fstab ]]; then
    local fstab_temp
    fstab_temp=$(mktemp /etc/fstab.cottenrouter.XXXXXX) || return 0
    awk -v entry="${SWAP_FILE} none swap sw 0 0" '$0 != entry { print }' /etc/fstab > "${fstab_temp}"
    chown --reference=/etc/fstab "${fstab_temp}"
    chmod --reference=/etc/fstab "${fstab_temp}"
    mv -f -- "${fstab_temp}" /etc/fstab
  fi
}

# Only undo an account this run actually created. An upgrade that reused the
# account from an earlier install must leave it, and its marker, alone.
rollback_account() {
  if ${user_created} || ${group_created}; then
    ${user_created} && userdel cottenrouter >/dev/null 2>&1
    ${group_created} && groupdel cottenrouter >/dev/null 2>&1
    rm -f -- "${ACCOUNT_MARKER}"
  fi
  return 0
}

rollback_firewall() {
  rm -f -- "${FIREWALL_MARKER}" "${FIREWALL_MARKER}.new"
  (( ${#firewall_added_ports[@]} > 0 )) || return 0
  if [[ ${firewall_kind} == ufw ]]; then
    for added_port in "${firewall_added_ports[@]}"; do
      ufw --force delete allow "${added_port}" >/dev/null 2>&1
    done
  elif [[ ${firewall_kind} == firewalld ]]; then
    for added_port in "${firewall_added_ports[@]}"; do
      firewall-cmd --permanent --remove-port="${added_port}" >/dev/null 2>&1
    done
    firewall-cmd --reload >/dev/null 2>&1
  fi
  return 0
}

rollback_install() {
  set +e
  printf 'CottenRouter startup failed; restoring the previous installation.\n' >&2
  rollback_firewall
  systemctl stop cottenrouter >/dev/null 2>&1
  restore_file binary "${BIN_PATH}"
  restore_file unit "${UNIT_PATH}"
  restore_file ensure_swap "${LIB_DIR}/ensure-swap.sh"
  restore_file uninstall_lib "${LIB_DIR}/uninstall.sh"
  restore_file uninstall_bin "${UNINSTALL_BIN_PATH}"
  restore_file config "${CONFIG_FILE}"
  restore_file resolved "${RESOLVED_DROPIN}"
  restore_file resolved_marker "${RESOLVED_MARKER}"
  restore_file resolvconf /etc/resolv.conf
  remove_transaction_swap
  rollback_account
  systemctl daemon-reload >/dev/null 2>&1
  case ${prior_router_enabled} in
    enabled) systemctl enable cottenrouter >/dev/null 2>&1 ;;
    enabled-runtime) systemctl enable --runtime cottenrouter >/dev/null 2>&1 ;;
    masked|masked-runtime) ;;
    *) systemctl disable cottenrouter >/dev/null 2>&1 ;;
  esac
  if ${resolved_changed}; then
    if ${prior_resolved_active}; then
      systemctl restart systemd-resolved >/dev/null 2>&1
    else
      systemctl stop systemd-resolved >/dev/null 2>&1
    fi
  fi
  if ${prior_router_active}; then
    systemctl restart cottenrouter >/dev/null 2>&1
  fi
  set -e
}

finish_install() {
  local status=$?
  trap - EXIT
  if [[ ${transaction_started} == true && ${committed} != true ]]; then
    rollback_install
  fi
  if [[ -d ${work_dir} ]]; then
    case ${work_dir} in
      /tmp/cottenrouter-install.*|/var/tmp/cottenrouter-install.*) rm -rf -- "${work_dir}" ;;
    esac
  fi
  exit "${status}"
}
trap finish_install EXIT
# Installing whatever is on the default branch means a commit merged
# seconds ago can break every fresh server. Stable installs pin a tagged
# release; --channel=edge is the explicit opt-in to default-branch code.
source_ref=""
if [[ ${channel} == stable ]]; then
  if [[ -n ${requested_version} ]]; then
    source_ref=${requested_version}
  else
    # git ls-remote instead of the GitHub API: shared datacenter and NAT
    # addresses regularly hit the unauthenticated API rate limit.
    source_ref=$(git ls-remote --tags --refs "https://github.com/${REPOSITORY}.git" 2>/dev/null \
      | awk '{ print $2 }' | sed 's#refs/tags/##' \
      | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n1 || true)
    if [[ -z ${source_ref} ]]; then
      source_ref=$(fetch -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/${REPOSITORY}/releases/latest" 2>/dev/null \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1 || true)
    fi
    [[ -n ${source_ref} ]] || fail "could not resolve the latest tagged release; retry with --version=vX.Y.Z or --channel=edge"
  fi
  [[ ${source_ref} =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "resolved an unsafe release tag: ${source_ref}"
  source_url="https://github.com/${REPOSITORY}/archive/refs/tags/${source_ref}.tar.gz"
  printf 'Installing CottenRouter %s (stable channel).\n' "${source_ref}"
else
  branch=$(fetch -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/${REPOSITORY}" | sed -n 's/.*"default_branch"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)
  [[ ${branch} =~ ^[A-Za-z0-9._/-]+$ ]] || fail "could not resolve a safe default branch"
  source_ref=$(fetch -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/${REPOSITORY}/commits/${branch}" | sed -n 's/.*"sha"[[:space:]]*:[[:space:]]*"\([0-9a-fA-F]*\)".*/\1/p' | head -n1)
  [[ ${source_ref} =~ ^[0-9a-fA-F]{40,64}$ ]] || fail "could not pin the current default-branch commit"
  source_url="https://github.com/${REPOSITORY}/archive/${source_ref}.tar.gz"
  printf 'Installing CottenRouter from %s@%s (edge channel).\n' "${branch}" "${source_ref:0:12}"
fi

# The unit file, bootstrap configuration and helper scripts come from the
# matching source archive whichever way the binary is obtained.
fetch "${source_url}" -o "${work_dir}/source.tar.gz"
mkdir "${work_dir}/source"
tar -xzf "${work_dir}/source.tar.gz" -C "${work_dir}/source" --strip-components=1

required_go=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' "${work_dir}/source/go.mod")
if [[ -z ${required_go} ]]; then
  required_go=$(awk '$1 == "go" { print $2; exit }' "${work_dir}/source/go.mod")
fi
[[ ${required_go} =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || fail "go.mod does not declare a safe Go version"

build_from_source() {
go_binary=""
# Reuse the host toolchain when it is at least as new as go.mod requires;
# an exact patch match would download Go again for 1.25.0 vs 1.25.1.
host_go_version=$(go env GOVERSION 2>/dev/null | sed "s/^go//" || true)
if [[ ${host_go_version} =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] && [[ $(printf "%s\n%s\n" "${required_go}" "${host_go_version}" | sort -V | head -n1) == "${required_go}" ]]; then
  go_binary=$(command -v go)
else
  case $(uname -m) in
    x86_64|amd64) go_arch=amd64 ;;
    aarch64|arm64) go_arch=arm64 ;;
    armv6l|armv7l) go_arch=armv6l ;;
    ppc64le) go_arch=ppc64le ;;
    riscv64) go_arch=riscv64 ;;
    s390x) go_arch=s390x ;;
    *) fail "unsupported architecture for the official Go toolchain: $(uname -m)" ;;
  esac
  go_archive="${work_dir}/go${required_go}.linux-${go_arch}.tar.gz"
  go_url="https://go.dev/dl/go${required_go}.linux-${go_arch}.tar.gz"
  printf 'Downloading Go %s for %s...\n' "${required_go}" "${go_arch}"
  go_tarball="go${required_go}.linux-${go_arch}.tar.gz"
  expected_sha=""
  # Level 1: go.dev .sha256 sidecar (plain-text, fast).
  _raw=$(fetch "${go_url}.sha256" 2>/dev/null || true)
  if [[ ${_raw} =~ ^[a-fA-F0-9]{64} ]]; then
    expected_sha=$(printf '%s' "${_raw}" | awk 'NR==1{print $1}')
  fi
  # Level 2: go.dev JSON download index (works when sidecar redirects to HTML).
  if [[ ! ${expected_sha} =~ ^[a-fA-F0-9]{64}$ ]]; then
    expected_sha=$(fetch "https://go.dev/dl/?mode=json&include=all" 2>/dev/null \
      | awk -v fn="${go_tarball}" '/"filename"/ && $0 ~ fn { found=1 } found && /"sha256"/ { gsub(/[^a-fA-F0-9]/,"",$NF); print $NF; exit }' \
      || true)
  fi
  # Level 3: bundled checksums in this repository (works when go.dev is blocked).
  if [[ ! ${expected_sha} =~ ^[a-fA-F0-9]{64}$ ]]; then
    _checksums_url="https://raw.githubusercontent.com/${REPOSITORY}/${source_ref}/scripts/go-checksums.txt"
    expected_sha=$(fetch "${_checksums_url}" 2>/dev/null \
      | awk -v fn="${go_tarball}" '$2 == fn { print $1; exit }' \
      || true)
  fi
  [[ ${expected_sha} =~ ^[a-fA-F0-9]{64}$ ]] || fail "could not obtain the official Go checksum"
  fetch "${go_url}" -o "${go_archive}"
  printf '%s  %s\n' "${expected_sha}" "${go_archive}" | sha256sum -c -
  mkdir "${work_dir}/go-toolchain"
  tar -xzf "${go_archive}" -C "${work_dir}/go-toolchain"
  go_binary="${work_dir}/go-toolchain/go/bin/go"
fi
go_binary_version=$("${go_binary}" env GOVERSION | sed "s/^go//")
[[ $(printf "%s\n%s\n" "${required_go}" "${go_binary_version}" | sort -V | head -n1) == "${required_go}" ]] || fail "the selected Go toolchain (${go_binary_version}) is older than go.mod requires (${required_go})"

export GOTOOLCHAIN=local
export CGO_ENABLED=0
# With a pipe, Go falls back to direct VCS on any proxy/network error. A comma
# only falls back for 404/410 responses, which is unsuitable on censored links.
export GOPROXY='https://proxy.golang.org|direct'
# Keep ALL Go build artifacts inside work_dir so they land on the same
# filesystem as the downloaded source/toolchain. This avoids "no space left
# on device" when /tmp is a small tmpfs but work_dir is on the main disk.
export GOTMPDIR="${work_dir}/gotmp"
export GOCACHE="${work_dir}/gocache"
mkdir -p "${GOTMPDIR}" "${GOCACHE}"
# A Go toolchain, module cache and build cache need roughly 2 GiB together.
_build_avail=$(df --output=avail "${work_dir}" 2>/dev/null | tail -1 || echo 0)
(( _build_avail >= 2097152 )) || fail "not enough disk space for the Go build (need at least 2 GiB free on $(df --output=target "${work_dir}" 2>/dev/null | tail -1))"
# Build only. The test suite contains performance- and timing-sensitive
# cases that belong in CI, not on arbitrary VPS hardware, and running
# downloaded test code as root buys nothing here.
(cd "${work_dir}/source" && "${go_binary}" build -trimpath -ldflags='-s -w' -o "${work_dir}/cottenrouter" ./cmd/cottenrouter)
}

# Prefer a published release binary: compiling on every server makes the
# install depend on the Go proxy, on CPU, on RAM and on ~2 GiB of scratch
# disk. Source compilation stays as the fallback and as --build-from-source.
download_release_binary() {
  local rel_arch asset base sums_file expected
  case $(uname -m) in
    x86_64|amd64) rel_arch=amd64 ;;
    aarch64|arm64) rel_arch=arm64 ;;
    *) return 1 ;;
  esac
  asset="cottenrouter-linux-${rel_arch}"
  base="https://github.com/${REPOSITORY}/releases/download/${source_ref}"
  sums_file="${work_dir}/SHA256SUMS"
  fetch "${base}/SHA256SUMS" -o "${sums_file}" || return 1
  expected=$(awk -v asset="${asset}" '$2 == asset || $2 == "*" asset { print $1; exit }' "${sums_file}")
  [[ ${expected} =~ ^[a-fA-F0-9]{64}$ ]] || return 1
  fetch "${base}/${asset}" -o "${work_dir}/cottenrouter" || return 1
  printf '%s  %s\n' "${expected}" "${work_dir}/cottenrouter" | sha256sum -c - >/dev/null || return 1
  chmod 0755 "${work_dir}/cottenrouter"
  return 0
}

if [[ ${build_mode} == source ]]; then
  build_from_source
elif download_release_binary; then
  printf 'Verified release binary for %s.\n' "$(uname -m)"
else
  printf 'No verified release binary for this host; compiling from source instead.\n' >&2
  build_from_source
fi
"${work_dir}/cottenrouter" version >/dev/null 2>&1 || fail "the installed CottenRouter binary does not run on this host"

[[ ! -L ${CONFIG_FILE} ]] || fail "refusing symlinked router configuration"
if path_present "${CONFIG_FILE}"; then
  [[ -f ${CONFIG_FILE} ]] || fail "router configuration is not a regular file"
  [[ $(stat -c '%u' "${CONFIG_FILE}") == 0 ]] || fail "router configuration is not root-owned"
  config_mode=$(stat -c '%a' "${CONFIG_FILE}")
  (( (8#${config_mode} & 022) == 0 )) || fail "router configuration must not be group/world writable"
fi
config_to_check=${CONFIG_FILE}
if [[ ! -f ${CONFIG_FILE} ]]; then
  config_to_check=${work_dir}/source/cottenrouter.bootstrap.json
fi
"${work_dir}/cottenrouter" check -config "${config_to_check}"

# Every port check below comes from the effective configuration instead of
# assuming UDP+TCP on :53. listen_tcp and the TLS listeners are optional.
json_string() { sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$2" | head -n1; }
addr_port() { local addr=$1; printf '%s' "${addr##*:}"; }

listen_udp=$(json_string listen_udp "${config_to_check}")
[[ -n ${listen_udp} ]] || listen_udp=0.0.0.0:53
listen_tcp=$(json_string listen_tcp "${config_to_check}")
admin_listen=$(json_string admin_listen "${config_to_check}")
[[ -n ${admin_listen} ]] || admin_listen=127.0.0.1:9088
# TLS listeners are objects inside tls_listeners, each with its own "listen".
mapfile -t tls_addrs < <(grep -o '"listen"[[:space:]]*:[[:space:]]*"[^"]*"' "${config_to_check}" | sed 's/.*"\([^"]*\)"$/\1/' || true)

udp_port=$(addr_port "${listen_udp}")
tcp_port=""
[[ -n ${listen_tcp} ]] && tcp_port=$(addr_port "${listen_tcp}") || true
admin_port=$(addr_port "${admin_listen}")

# proto:port pairs this installation must be able to bind.
required_listeners=("udp:${udp_port}" "tcp:${admin_port}")
[[ -n ${tcp_port} ]] && required_listeners+=("tcp:${tcp_port}") || true
for tls_addr in "${tls_addrs[@]:-}"; do
  [[ -n ${tls_addr} ]] && required_listeners+=("tcp:$(addr_port "${tls_addr}")") || true
done

listener_owner() {
  local proto=$1 port=$2 flag
  if [[ ${proto} == udp ]]; then flag=-lnup; else flag=-lntp; fi
  # Protocol-specific ss output has no Netid column, so the local address
  # is field 4. Only the combined -lntup form puts it in field 5.
  ss -H "${flag}" 2>/dev/null | awk -v port=":${port}$" '$4 ~ port {print}' || true
}

printf '# Managed by CottenRouter installer.\n[Resolve]\nDNSStubListener=no\n' > "${work_dir}/resolved.conf"
printf '%s\n' "${RESOLVED_MARKER_VALUE}" > "${work_dir}/resolved.marker"
if path_present "${RESOLVED_DROPIN}" || path_present "${RESOLVED_MARKER}"; then
  [[ -f ${RESOLVED_DROPIN} && ! -L ${RESOLVED_DROPIN} ]] || fail "resolved drop-in ownership is incomplete or unsafe: ${RESOLVED_DROPIN}"
  [[ -f ${RESOLVED_MARKER} && ! -L ${RESOLVED_MARKER} ]] || fail "refusing an unmarked systemd-resolved drop-in; inspect ${RESOLVED_DROPIN} manually"
  cmp -s "${RESOLVED_DROPIN}" "${work_dir}/resolved.conf" || fail "managed systemd-resolved drop-in was modified; refusing to overwrite it"
  cmp -s "${RESOLVED_MARKER}" "${work_dir}/resolved.marker" || fail "invalid systemd-resolved ownership marker"
fi

# Preflight every configured listener, not only :53, so a conflict on the
# admin port or a TLS port is caught before anything on the host changes.
resolved_owns_dns=false
for listener in "${required_listeners[@]}"; do
  occupied=$(listener_owner "${listener%%:*}" "${listener##*:}" | grep -v cottenrouter || true)
  [[ -n ${occupied} ]] || continue
  if grep -q 'systemd-resolve' <<<"${occupied}"; then
    resolved_owns_dns=true
    occupied=$(grep -v 'systemd-resolve' <<<"${occupied}" || true)
  fi
  [[ -n ${occupied} ]] || continue
  if is_wsl; then
    printf 'WSL virtual DNS infrastructure owns %s. Stop it from Windows or use a native Linux host:\n%s\n' "${listener}" "${occupied}" >&2
  else
    printf '%s is owned by another service. CottenRouter will not stop it automatically; stop or reconfigure it, then re-run this installer:\n%s\n' "${listener}" "${occupied}" >&2
  fi
  exit 1
done
for staging_path in "${BIN_PATH}.new" "${UNIT_PATH}.new" "${RESOLVED_DROPIN}.new" "${RESOLVED_MARKER}.new"; do
  path_present "${staging_path}" && fail "refusing pre-existing staging path: ${staging_path}"
done
backup_file binary "${BIN_PATH}"
backup_file unit "${UNIT_PATH}"
backup_file ensure_swap "${LIB_DIR}/ensure-swap.sh"
backup_file uninstall_lib "${LIB_DIR}/uninstall.sh"
backup_file uninstall_bin "${UNINSTALL_BIN_PATH}"
backup_file config "${CONFIG_FILE}"
backup_file resolved "${RESOLVED_DROPIN}"
backup_file resolved_marker "${RESOLVED_MARKER}"
backup_file resolvconf /etc/resolv.conf
prior_router_enabled=$(systemctl is-enabled cottenrouter 2>/dev/null || true)
systemctl is-active --quiet cottenrouter && prior_router_active=true
systemctl is-active --quiet systemd-resolved && prior_resolved_active=true
transaction_started=true

if [[ ! -d ${STATE_DIR} ]]; then
  install -d -o root -g root -m 0700 "${STATE_DIR}"
fi
[[ ! -L ${ACCOUNT_MARKER} ]] || fail "refusing symlinked account ownership marker"

# The cottenrouter account is only reused when it is unmistakably the service
# account this installer creates. A human or unrelated project may already own
# that name, and purge must never delete their account.
if ! getent group cottenrouter >/dev/null; then
  groupadd --system cottenrouter
  group_created=true
fi
if id cottenrouter >/dev/null 2>&1; then
  if [[ ! -f ${ACCOUNT_MARKER} ]]; then
    existing_shell=$(getent passwd cottenrouter | awk -F: '{print $7}')
    existing_home=$(getent passwd cottenrouter | awk -F: '{print $6}')
    case ${existing_shell} in
      */nologin|/bin/false|/usr/bin/false|/sbin/nologin) ;;
      *) fail "an unrelated cottenrouter account already exists (shell ${existing_shell}); rename it or remove it manually" ;;
    esac
    [[ ${existing_home} == /nonexistent ]] || fail "an unrelated cottenrouter account already exists (home ${existing_home}); rename it or remove it manually"
  fi
else
  nologin_shell=$(command -v nologin || true)
  [[ -n ${nologin_shell} ]] || nologin_shell=/bin/false
  useradd --system --gid cottenrouter --home-dir /nonexistent --shell "${nologin_shell}" cottenrouter
  user_created=true
fi
if ${user_created} || ${group_created}; then
  printf '%s
' "${ACCOUNT_MARKER_VALUE}" > "${ACCOUNT_MARKER}.new"
  chmod 0600 "${ACCOUNT_MARKER}.new"
  mv -f -- "${ACCOUNT_MARKER}.new" "${ACCOUNT_MARKER}"
fi
if [[ ! -d ${CONFIG_DIR} ]]; then
  install -d -o root -g cottenrouter -m 2750 "${CONFIG_DIR}"
fi
if [[ ! -d ${LIB_DIR} ]]; then
  install -d -o root -g root -m 0755 "${LIB_DIR}"
fi
# A pre-existing but inactive swap file must not be left activated by a
# failed run, so file existence and activation are tracked separately.
if [[ -f ${SWAP_MARKER} && -f ${SWAP_FILE} ]]; then
  swap_preexisting=true
  swapon --show=NAME --noheadings 2>/dev/null | grep -Fxq "${SWAP_FILE}" && swap_was_active=true || true
fi

if ${resolved_owns_dns}; then
  [[ ! -L /etc/systemd/resolved.conf.d ]] || fail "refusing symlinked systemd-resolved drop-in directory"
  install -d -o root -g root -m 0755 /etc/systemd/resolved.conf.d
  install -o root -g root -m 0644 "${work_dir}/resolved.conf" "${RESOLVED_DROPIN}.new"
  mv -- "${RESOLVED_DROPIN}.new" "${RESOLVED_DROPIN}"
  install -o root -g root -m 0600 "${work_dir}/resolved.marker" "${RESOLVED_MARKER}.new"
  mv -- "${RESOLVED_MARKER}.new" "${RESOLVED_MARKER}"
  resolved_changed=true
  # Disabling the stub listener strands every resolver client still pointed
  # at 127.0.0.53, which would cost the host its own DNS. Repoint the
  # symlink at the real upstream list; rollback restores the original.
  if [[ -L /etc/resolv.conf && $(readlink -f /etc/resolv.conf) == */stub-resolv.conf ]]; then
    ln -sfn /run/systemd/resolve/resolv.conf /etc/resolv.conf
    printf 'Repointed /etc/resolv.conf away from the disabled 127.0.0.53 stub.\n'
  fi
  systemctl restart systemd-resolved
  remaining_resolved=$(listener_owner udp "${udp_port}" | grep systemd-resolve || true)
  [[ -z ${remaining_resolved} ]] || fail "systemd-resolved still owns port ${udp_port} after disabling its stub listener"
  getent hosts go.dev >/dev/null 2>&1 ||     printf 'Warning: the host could not resolve go.dev after the systemd-resolved change; check /etc/resolv.conf.\n' >&2
fi
if [[ ! -f ${CONFIG_FILE} ]]; then
  install -o root -g cottenrouter -m 0640 "${work_dir}/source/cottenrouter.bootstrap.json" "${CONFIG_FILE}"
fi
"${work_dir}/cottenrouter" check -config "${CONFIG_FILE}"

[[ ! -L ${BIN_PATH}.new ]] || fail "refusing symlinked binary staging path"
install -m 0755 "${work_dir}/cottenrouter" "${BIN_PATH}.new"
mv -f -- "${BIN_PATH}.new" "${BIN_PATH}"
install -m 0755 "${work_dir}/source/scripts/ensure-swap.sh" "${LIB_DIR}/ensure-swap.sh"
install -m 0755 "${work_dir}/source/scripts/uninstall.sh" "${LIB_DIR}/uninstall.sh"
install -m 0755 "${work_dir}/source/scripts/uninstall.sh" "${UNINSTALL_BIN_PATH}"
[[ ! -L ${UNIT_PATH}.new ]] || fail "refusing symlinked unit staging path"
install -m 0644 "${work_dir}/source/packaging/cottenrouter.service" "${UNIT_PATH}.new"
mv -f -- "${UNIT_PATH}.new" "${UNIT_PATH}"
# A capability or sandbox directive this systemd cannot honour would look
# exactly like a port-binding failure later, so surface it here instead.
if command -v systemd-analyze >/dev/null 2>&1; then
  systemd-analyze verify "${UNIT_PATH}" >/dev/null 2>&1 ||     printf 'Warning: systemd-analyze reported issues with %s; run it manually if startup fails.\n' "${UNIT_PATH}" >&2
fi
if [[ ${swap_mode} == off ]]; then
  printf 'Skipping the swap safeguard (--no-swap).\n'
else
  "${LIB_DIR}/ensure-swap.sh"
fi

systemctl daemon-reload
systemctl enable cottenrouter
if ${prior_router_active}; then
  systemctl restart cottenrouter
else
  systemctl start cottenrouter
fi
# The binary's own health command reads admin_listen from the same
# configuration the service uses, so no port is hardcoded here. If any
# listener fails to bind, ListenAndServe exits and the probe never passes.
router_ready=false
health_error=""
for _ in {1..60}; do
  if systemctl is-failed --quiet cottenrouter; then
    break
  fi
  if systemctl is-active --quiet cottenrouter; then
    if health_error=$("${BIN_PATH}" healthz -config "${CONFIG_FILE}" -timeout 2s 2>&1); then
      router_ready=true
      break
    fi
  fi
  sleep 0.5
done
if ! ${router_ready}; then
  printf '\n----- cottenrouter service status -----\n' >&2
  systemctl status cottenrouter --no-pager -l >&2 2>&1 || true
  printf '\n----- cottenrouter journal (last 100 lines) -----\n' >&2
  journalctl -u cottenrouter -n 100 --no-pager >&2 2>&1 || true
  printf '\n----- listening sockets -----\n' >&2
  ss -lntup >&2 2>&1 || true
  printf '\n----- health probe -----\n%s\n\n' "${health_error:-service never became active}" >&2
  fail "CottenRouter did not pass its health check within 30 seconds"
fi

# Deterministic smoke test: prove the UDP listener actually answers, not
# just that the process started. Failure is a warning, since a locked-down
# bootstrap configuration may legitimately refuse this query.
if command -v dig >/dev/null 2>&1; then
  dig +short +time=2 +tries=1 -p "${udp_port}" @127.0.0.1 localhost >/dev/null 2>&1 ||     printf 'Warning: a test DNS query to 127.0.0.1:%s returned no answer; check the routing rules.\n' "${udp_port}" >&2
fi

# Open exactly the ports this configuration serves publicly. The admin
# listener is loopback-only and is deliberately never opened.
public_ports=("${udp_port}/udp")
[[ -n ${tcp_port} ]] && public_ports+=("${tcp_port}/tcp") || true
for tls_addr in "${tls_addrs[@]:-}"; do
  [[ -n ${tls_addr} ]] && public_ports+=("$(addr_port "${tls_addr}")/tcp") || true
done

if command -v ufw >/dev/null 2>&1 && ufw status | grep -qw active; then
  firewall_kind=ufw
  for public_port in "${public_ports[@]}"; do
    if ! ufw status | grep -Eq "(^|[[:space:]])${public_port}([[:space:]]|\$).*ALLOW"; then
      ufw allow "${public_port}" >/dev/null
      firewall_added_ports+=("${public_port}")
    fi
  done
elif command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld; then
  firewall_kind=firewalld
  for public_port in "${public_ports[@]}"; do
    if ! firewall-cmd --permanent --query-port="${public_port}" >/dev/null 2>&1; then
      firewall-cmd --permanent --add-port="${public_port}" >/dev/null
      firewall_added_ports+=("${public_port}")
    fi
  done
  (( ${#firewall_added_ports[@]} == 0 )) || firewall-cmd --reload >/dev/null
elif command -v nft >/dev/null 2>&1 || command -v iptables >/dev/null 2>&1; then
  # Rewriting an arbitrary nftables/iptables policy is not safe to
  # automate, so state the requirement instead.
  printf 'CottenRouter is installed, but firewall management is unsupported on this host (nftables/iptables only).\n' >&2
  printf 'Allow inbound: %s\n' "${public_ports[*]}" >&2
fi
# Record exactly which rules this installer added so the uninstaller removes
# those and nothing else. Rules that already existed are never touched.
rm -f -- "${FIREWALL_MARKER}"
if (( ${#firewall_added_ports[@]} > 0 )); then
  [[ ! -L ${FIREWALL_MARKER} ]] || fail "refusing symlinked firewall ownership marker"
  {
    printf 'kind %s
' "${firewall_kind}"
    for added_port in "${firewall_added_ports[@]}"; do
      printf 'port %s
' "${added_port}"
    done
    true
  } > "${FIREWALL_MARKER}.new"
  chmod 0600 "${FIREWALL_MARKER}.new"
  mv -f -- "${FIREWALL_MARKER}.new" "${FIREWALL_MARKER}"
fi
committed=true

echo
printf 'Required inbound ports on any provider firewall or security group (Hetzner, AWS, Oracle, ...): %s\n' "${public_ports[*]}"
echo "CottenRouter is installed. Launch the control deck with:"
echo "  sudo cottenrouter tui"
echo "Safe router removal (backend data is preserved by default):"
echo "  sudo cottenrouter-uninstall"
