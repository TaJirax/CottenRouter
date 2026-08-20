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
  awk chmod chown cmp cp curl dd df fallocate getent git grep groupadd head id install
  mkdir mktemp mkswap mv rm sed sha256sum sleep ss stat swapoff swapon systemctl tar uname useradd
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
firewall_kind=none
firewall_udp_added=false
firewall_tcp_added=false
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
  ${swap_preexisting} && return 0
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
  if [[ ${firewall_kind} == ufw ]]; then
    ${firewall_udp_added} && ufw --force delete allow 53/udp >/dev/null 2>&1
    ${firewall_tcp_added} && ufw --force delete allow 53/tcp >/dev/null 2>&1
  elif [[ ${firewall_kind} == firewalld ]]; then
    ${firewall_udp_added} && firewall-cmd --permanent --remove-port=53/udp >/dev/null 2>&1
    ${firewall_tcp_added} && firewall-cmd --permanent --remove-port=53/tcp >/dev/null 2>&1
    if ${firewall_udp_added} || ${firewall_tcp_added}; then
      firewall-cmd --reload >/dev/null 2>&1
    fi
  fi
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
  if [[ ${work_dir} == /tmp/cottenrouter-install.* && -d ${work_dir} ]]; then
    rm -rf -- "${work_dir}"
  fi
  exit "${status}"
}
trap finish_install EXIT
branch=$(curl -fsSL -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/${REPOSITORY}" | sed -n 's/.*"default_branch"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)
[[ ${branch} =~ ^[A-Za-z0-9._/-]+$ ]] || fail "could not resolve a safe default branch"
commit=$(curl -fsSL --retry 3 -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/${REPOSITORY}/commits/${branch}" | sed -n 's/.*"sha"[[:space:]]*:[[:space:]]*"\([0-9a-fA-F]*\)".*/\1/p' | head -n1)
[[ ${commit} =~ ^[0-9a-fA-F]{40,64}$ ]] || fail "could not pin the current default-branch commit"
curl -fsSL --retry 3 "https://github.com/${REPOSITORY}/archive/${commit}.tar.gz" -o "${work_dir}/source.tar.gz"
mkdir "${work_dir}/source"
tar -xzf "${work_dir}/source.tar.gz" -C "${work_dir}/source" --strip-components=1

required_go=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' "${work_dir}/source/go.mod")
if [[ -z ${required_go} ]]; then
  required_go=$(awk '$1 == "go" { print $2; exit }' "${work_dir}/source/go.mod")
fi
[[ ${required_go} =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || fail "go.mod does not declare a safe Go version"

go_binary=""
if command -v go >/dev/null 2>&1 && [[ $(go env GOVERSION 2>/dev/null || true) == "go${required_go}" ]]; then
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
  _raw=$(curl -fsSL --retry 3 "${go_url}.sha256" 2>/dev/null || true)
  if [[ ${_raw} =~ ^[a-fA-F0-9]{64} ]]; then
    expected_sha=$(printf '%s' "${_raw}" | awk 'NR==1{print $1}')
  fi
  # Level 2: go.dev JSON download index (works when sidecar redirects to HTML).
  if [[ ! ${expected_sha} =~ ^[a-fA-F0-9]{64}$ ]]; then
    expected_sha=$(curl -fsSL --retry 3 "https://go.dev/dl/?mode=json&include=all" 2>/dev/null \
      | awk -v fn="${go_tarball}" '/"filename"/ && $0 ~ fn { found=1 } found && /"sha256"/ { gsub(/[^a-fA-F0-9]/,"",$NF); print $NF; exit }' \
      || true)
  fi
  # Level 3: bundled checksums in this repository (works when go.dev is blocked).
  if [[ ! ${expected_sha} =~ ^[a-fA-F0-9]{64}$ ]]; then
    _checksums_url="https://raw.githubusercontent.com/${REPOSITORY}/${commit}/scripts/go-checksums.txt"
    expected_sha=$(curl -fsSL --retry 3 "${_checksums_url}" 2>/dev/null \
      | awk -v fn="${go_tarball}" '$2 == fn { print $1; exit }' \
      || true)
  fi
  [[ ${expected_sha} =~ ^[a-fA-F0-9]{64}$ ]] || fail "could not obtain the official Go checksum"
  curl -fsSL --retry 3 "${go_url}" -o "${go_archive}"
  printf '%s  %s\n' "${expected_sha}" "${go_archive}" | sha256sum -c -
  mkdir "${work_dir}/go-toolchain"
  tar -xzf "${go_archive}" -C "${work_dir}/go-toolchain"
  go_binary="${work_dir}/go-toolchain/go/bin/go"
fi
[[ $("${go_binary}" env GOVERSION) == "go${required_go}" ]] || fail "the selected Go toolchain does not match go.mod"

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
# Require at least 1 GB free on the build filesystem before starting.
_build_avail=$(df --output=avail "${work_dir}" 2>/dev/null | tail -1 || echo 0)
(( _build_avail >= 1048576 )) || fail "not enough disk space for Go build (need ≥ 1 GB free on $(df --output=target "${work_dir}" 2>/dev/null | tail -1))"
(cd "${work_dir}/source" && "${go_binary}" test ./... && "${go_binary}" build -trimpath -ldflags='-s -w' -o "${work_dir}/cottenrouter" ./cmd/cottenrouter)

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

printf '# Managed by CottenRouter installer.\n[Resolve]\nDNSStubListener=no\n' > "${work_dir}/resolved.conf"
printf '%s\n' "${RESOLVED_MARKER_VALUE}" > "${work_dir}/resolved.marker"
if path_present "${RESOLVED_DROPIN}" || path_present "${RESOLVED_MARKER}"; then
  [[ -f ${RESOLVED_DROPIN} && ! -L ${RESOLVED_DROPIN} ]] || fail "resolved drop-in ownership is incomplete or unsafe: ${RESOLVED_DROPIN}"
  [[ -f ${RESOLVED_MARKER} && ! -L ${RESOLVED_MARKER} ]] || fail "refusing an unmarked systemd-resolved drop-in; inspect ${RESOLVED_DROPIN} manually"
  cmp -s "${RESOLVED_DROPIN}" "${work_dir}/resolved.conf" || fail "managed systemd-resolved drop-in was modified; refusing to overwrite it"
  cmp -s "${RESOLVED_MARKER}" "${work_dir}/resolved.marker" || fail "invalid systemd-resolved ownership marker"
fi

occupied=$(ss -H -lntup 2>/dev/null | awk '$5 ~ /:53$/ && $0 !~ /cottenrouter/ {print}' || true)
resolved_owns_53=false
if grep -q 'systemd-resolve' <<<"${occupied}"; then
  resolved_owns_53=true
fi
protected=$(grep -v 'systemd-resolve' <<<"${occupied}" || true)
if [[ -n ${protected} ]]; then
  printf 'Port 53 has a protected owner. Nothing was stopped:\n%s\n' "${protected}" >&2
  exit 1
fi
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
[[ -f ${SWAP_MARKER} && -f ${SWAP_FILE} ]] && swap_preexisting=true

if ${resolved_owns_53}; then
  [[ ! -L /etc/systemd/resolved.conf.d ]] || fail "refusing symlinked systemd-resolved drop-in directory"
  install -d -o root -g root -m 0755 /etc/systemd/resolved.conf.d
  install -o root -g root -m 0644 "${work_dir}/resolved.conf" "${RESOLVED_DROPIN}.new"
  mv -- "${RESOLVED_DROPIN}.new" "${RESOLVED_DROPIN}"
  install -o root -g root -m 0600 "${work_dir}/resolved.marker" "${RESOLVED_MARKER}.new"
  mv -- "${RESOLVED_MARKER}.new" "${RESOLVED_MARKER}"
  resolved_changed=true
  systemctl restart systemd-resolved
  remaining_resolved=$(ss -H -lntup 2>/dev/null | awk '$5 ~ /:53$/ && $0 ~ /systemd-resolve/ {print}' || true)
  [[ -z ${remaining_resolved} ]] || fail "systemd-resolved still owns port 53 after disabling its stub listener"
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
"${LIB_DIR}/ensure-swap.sh"

systemctl daemon-reload
systemctl enable cottenrouter
if ${prior_router_active}; then
  systemctl restart cottenrouter
else
  systemctl start cottenrouter
fi
router_ready=false
for _ in {1..20}; do
  if systemctl is-active --quiet cottenrouter; then
    udp_listener=$(ss -H -lnup 2>/dev/null | awk '$5 ~ /:53$/ && $0 ~ /cottenrouter/ {print}' || true)
    tcp_listener=$(ss -H -lntp 2>/dev/null | awk '$4 ~ /:53$/ && $0 ~ /cottenrouter/ {print}' || true)
    if [[ -n ${udp_listener} && -n ${tcp_listener} ]] && curl -fsS --max-time 1 http://127.0.0.1:9088/healthz >/dev/null 2>&1; then
      router_ready=true
      break
    fi
  fi
  sleep 0.25
done
${router_ready} || fail "CottenRouter did not acquire UDP/TCP port 53 and pass its health check"

if command -v ufw >/dev/null 2>&1 && ufw status | grep -qw active; then
  firewall_kind=ufw
  if ! ufw status | grep -Eq '(^|[[:space:]])53/udp([[:space:]]|$).*ALLOW'; then
    ufw allow 53/udp >/dev/null
    firewall_udp_added=true
  fi
  if ! ufw status | grep -Eq '(^|[[:space:]])53/tcp([[:space:]]|$).*ALLOW'; then
    ufw allow 53/tcp >/dev/null
    firewall_tcp_added=true
  fi
elif command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld; then
  firewall_kind=firewalld
  if ! firewall-cmd --permanent --query-port=53/udp >/dev/null 2>&1; then
    firewall-cmd --permanent --add-port=53/udp >/dev/null
    firewall_udp_added=true
  fi
  if ! firewall-cmd --permanent --query-port=53/tcp >/dev/null 2>&1; then
    firewall-cmd --permanent --add-port=53/tcp >/dev/null
    firewall_tcp_added=true
  fi
  if ${firewall_udp_added} || ${firewall_tcp_added}; then
    firewall-cmd --reload >/dev/null
  fi
fi
# Record exactly which rules this installer added so the uninstaller removes
# those and nothing else. Rules that already existed are never touched.
rm -f -- "${FIREWALL_MARKER}"
if ${firewall_udp_added} || ${firewall_tcp_added}; then
  [[ ! -L ${FIREWALL_MARKER} ]] || fail "refusing symlinked firewall ownership marker"
  {
    printf 'kind %s
' "${firewall_kind}"
    ${firewall_udp_added} && printf 'port 53/udp
'
    ${firewall_tcp_added} && printf 'port 53/tcp
'
    true
  } > "${FIREWALL_MARKER}.new"
  chmod 0600 "${FIREWALL_MARKER}.new"
  mv -f -- "${FIREWALL_MARKER}.new" "${FIREWALL_MARKER}"
fi
committed=true

echo
echo "CottenRouter is installed. Launch the control deck with:"
echo "  sudo cottenrouter tui"
echo "Safe router removal (backend data is preserved by default):"
echo "  sudo cottenrouter-uninstall"
