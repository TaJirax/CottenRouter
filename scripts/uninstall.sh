#!/usr/bin/env bash
set -euo pipefail

readonly CONFIG_DIR=/etc/cottenrouter
readonly BIN_PATH=/usr/local/bin/cottenrouter
readonly UNINSTALL_BIN_PATH=/usr/local/bin/cottenrouter-uninstall
readonly LIB_DIR=/usr/local/lib/cottenrouter
readonly UNIT_PATH=/etc/systemd/system/cottenrouter.service
readonly SLICE_PATH=/etc/systemd/system/cottenrouter-backends.slice
readonly SWAP_DIRECTORY=/var/lib/cottenrouter
readonly SWAP_FILE=${SWAP_DIRECTORY}/swapfile
readonly SWAP_MARKER=${SWAP_DIRECTORY}/swapfile.created-by-cottenrouter
readonly RESOLVED_DROPIN=/etc/systemd/resolved.conf.d/cottenrouter.conf
readonly RESOLVED_MARKER=${SWAP_DIRECTORY}/resolved-dropin.created-by-cottenrouter
readonly RESOLVED_MARKER_VALUE=cottenrouter-resolved-v1
readonly ACCOUNT_MARKER=${SWAP_DIRECTORY}/account.created-by-cottenrouter
readonly ACCOUNT_MARKER_VALUE=cottenrouter-account-v1
readonly FIREWALL_MARKER=${SWAP_DIRECTORY}/firewall.created-by-cottenrouter

purge=false
purge_backends=false
remove_swap=false
confirmation=""

usage() {
  cat <<'EOF'
Usage: cottenrouter-uninstall [options]

Safely removes CottenRouter while leaving every upstream backend, panel,
pre-existing firewall rule, and backend data directory untouched. Firewall
rules and the service account are removed only when this installer created
them, as recorded by its ownership markers.

Options:
  --purge                 Also delete /etc/cottenrouter and its router config.
  --purge-backends        Also stop, disable, and uninstall all DNS backend
                          services that CottenRouter installed (cottendns,
                          masterdnsvpn, stormdns, thefeed-server, slipgate-*).
                          Requires --purge and --confirm CottenRouter.
  --remove-swap           Remove swap only if this installer created it.
  --confirm CottenRouter  Required with --purge, --purge-backends, or --remove-swap.
  -h, --help              Show this help.
EOF
}

fail() {
  printf 'CottenRouter uninstaller: %s\n' "$*" >&2
  exit 1
}

remove_owned_dropin() {
  local dropin_path=$1
  local dropin_dir=${dropin_path%/*}
  [[ ${dropin_dir} == /etc/systemd/system/*.service.d ]] || fail "unsafe systemd drop-in path: ${dropin_path}"
  [[ ! -L ${dropin_dir} ]] || fail "refusing symlinked systemd drop-in directory: ${dropin_dir}"
  rm -f -- "${dropin_path}"
  rmdir --ignore-fail-on-non-empty "${dropin_dir}" 2>/dev/null || true
}

while (( $# > 0 )); do
  case "$1" in
    --purge)
      purge=true
      shift
      ;;
    --purge-backends)
      purge_backends=true
      shift
      ;;
    --remove-swap)
      remove_swap=true
      shift
      ;;
    --confirm)
      (( $# >= 2 )) || fail "--confirm requires CottenRouter"
      confirmation=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown option: $1"
      ;;
  esac
done

if [[ ${EUID} -ne 0 ]]; then
  fail "run as root"
fi
if [[ ! -r /proc/sys/kernel/ostype ]]; then
  fail "server removal supports Linux only"
fi
if { ${purge} || ${purge_backends} || ${remove_swap}; } && [[ ${confirmation} != CottenRouter ]]; then
  fail "--purge, --purge-backends, and --remove-swap require --confirm CottenRouter"
fi
if ${purge_backends} && ! ${purge}; then
  fail "--purge-backends requires --purge"
fi
if ${remove_swap} && [[ ! -f ${SWAP_MARKER} ]]; then
  fail "swap ownership marker is absent; refusing to remove a possibly user-managed swap file"
fi
if [[ -L ${ACCOUNT_MARKER} || -L ${FIREWALL_MARKER} ]]; then
  fail "refusing symlinked ownership marker in ${SWAP_DIRECTORY}"
fi
if ${purge} && [[ -L ${CONFIG_DIR} ]]; then
  fail "refusing to purge symlinked config directory ${CONFIG_DIR}"
fi
if [[ -L ${LIB_DIR} ]]; then
  fail "refusing to remove symlinked library directory ${LIB_DIR}"
fi
if [[ -L ${SWAP_DIRECTORY} ]]; then
  fail "refusing symlinked CottenRouter state directory ${SWAP_DIRECTORY}"
fi

# Resolve ownership before stopping or deleting anything, so a modified or
# ambiguous DNS configuration cannot leave a half-completed uninstall.
resolved_owned=false
if [[ -e ${RESOLVED_MARKER} || -L ${RESOLVED_MARKER} ]]; then
  [[ ! -L /etc/systemd/resolved.conf.d ]] || fail "refusing symlinked systemd-resolved drop-in directory"
  [[ -f ${RESOLVED_MARKER} && ! -L ${RESOLVED_MARKER} ]] || fail "unsafe systemd-resolved ownership marker; preserving DNS configuration"
  [[ -f ${RESOLVED_DROPIN} && ! -L ${RESOLVED_DROPIN} ]] || fail "owned systemd-resolved drop-in is missing or unsafe; preserving ownership marker"
  [[ $(<"${RESOLVED_MARKER}") == "${RESOLVED_MARKER_VALUE}" ]] || fail "invalid systemd-resolved ownership marker; preserving DNS configuration"
  cmp -s "${RESOLVED_DROPIN}" <(printf '# Managed by CottenRouter installer.\n[Resolve]\nDNSStubListener=no\n') || fail "managed systemd-resolved drop-in was modified; preserving it"
  resolved_owned=true
fi

if systemctl is-active --quiet cottenrouter 2>/dev/null; then
  systemctl stop cottenrouter
  systemctl is-active --quiet cottenrouter 2>/dev/null && fail "service is still active"
fi
systemctl disable cottenrouter >/dev/null 2>&1 || true

# Remove only drop-ins created by CottenRouter. Upstream unit files, binaries,
# data, users, firewall rules, and panel files remain owned by their projects.
managed_services=(cottendns masterdnsvpn stormdns thefeed-server slipgate-dnsrouter)
for service_name in "${managed_services[@]}"; do
  dropin_dir="/etc/systemd/system/${service_name}.service.d"
  remove_owned_dropin "${dropin_dir}/cottenrouter.conf"
done
shopt -s nullglob
for dropin_path in /etc/systemd/system/slipgate-*.service.d/cottenrouter.conf; do
  [[ ${dropin_path} == /etc/systemd/system/slipgate-*.service.d/cottenrouter.conf ]] || fail "unsafe SlipGate drop-in path"
  remove_owned_dropin "${dropin_path}"
done
shopt -u nullglob

rm -f -- "${UNIT_PATH}" "${SLICE_PATH}" "${BIN_PATH}"
systemctl daemon-reload
systemctl reset-failed cottenrouter >/dev/null 2>&1 || true

resolved_removed=false
if ${resolved_owned}; then
  rm -f -- "${RESOLVED_DROPIN}" "${RESOLVED_MARKER}"
  resolved_removed=true
fi
if ${resolved_removed}; then
  if systemctl is-active --quiet systemd-resolved 2>/dev/null; then
    systemctl restart systemd-resolved
  fi
fi

# Remove only the firewall rules install.sh recorded as its own. Anything the
# operator or another project added is left in place.
if [[ -f ${FIREWALL_MARKER} ]]; then
  firewall_kind=$(awk '$1 == "kind" { print $2; exit }' "${FIREWALL_MARKER}")
  mapfile -t owned_ports < <(awk '$1 == "port" { print $2 }' "${FIREWALL_MARKER}")
  for owned_port in "${owned_ports[@]}"; do
    [[ ${owned_port} =~ ^[0-9]{1,5}/(udp|tcp)$ ]] || fail "unsafe firewall ownership record: ${owned_port}"
  done
  if [[ ${firewall_kind} == ufw ]] && command -v ufw >/dev/null 2>&1; then
    for owned_port in "${owned_ports[@]}"; do
      ufw --force delete allow "${owned_port}" >/dev/null 2>&1 || true
    done
  elif [[ ${firewall_kind} == firewalld ]] && command -v firewall-cmd >/dev/null 2>&1; then
    for owned_port in "${owned_ports[@]}"; do
      firewall-cmd --permanent --remove-port="${owned_port}" >/dev/null 2>&1 || true
    done
    (( ${#owned_ports[@]} == 0 )) || firewall-cmd --reload >/dev/null 2>&1 || true
  fi
  rm -f -- "${FIREWALL_MARKER}"
  firewall_removed=true
else
  firewall_removed=false
fi

if ${remove_swap}; then
  if swapon --show=NAME --noheadings 2>/dev/null | grep -Fxq "${SWAP_FILE}"; then
    swapoff "${SWAP_FILE}"
  fi
  rm -f -- "${SWAP_FILE}" "${SWAP_MARKER}"
  if [[ -f /etc/fstab ]]; then
    fstab_temp=$(mktemp /etc/fstab.cottenrouter.XXXXXX)
    awk -v entry="${SWAP_FILE} none swap sw 0 0" '$0 != entry { print }' /etc/fstab > "${fstab_temp}"
    chown --reference=/etc/fstab "${fstab_temp}"
    chmod --reference=/etc/fstab "${fstab_temp}"
    mv -f -- "${fstab_temp}" /etc/fstab
  fi
  rmdir --ignore-fail-on-non-empty "${SWAP_DIRECTORY}" 2>/dev/null || true
fi

account_removed=false
if ${purge}; then
  rm -rf -- "${CONFIG_DIR}"
  # Delete the service account only when this installer created it. Without the
  # ownership marker the cottenrouter name may belong to someone else.
  if [[ -f ${ACCOUNT_MARKER} && $(<"${ACCOUNT_MARKER}") == "${ACCOUNT_MARKER_VALUE}" ]]; then
    if id cottenrouter >/dev/null 2>&1; then
      userdel cottenrouter
    fi
    if getent group cottenrouter >/dev/null 2>&1; then
      groupdel cottenrouter 2>/dev/null || true
    fi
    rm -f -- "${ACCOUNT_MARKER}"
    account_removed=true
  fi
fi

rm -rf -- "${LIB_DIR}"
rm -f -- "${UNINSTALL_BIN_PATH}"

# Remove all managed DNS backend services and their data when requested.
backends_removed=false
if ${purge_backends}; then
  readonly BACKEND_SERVICES=(cottendns masterdnsvpn stormdns thefeed-server)
  readonly BACKEND_DIRS=(
    /opt/cottenrouter/backends/cottendns
    /opt/cottenrouter/backends/masterdnsvpn
    /opt/cottenrouter/backends/stormdns
    /opt/thefeed
  )
  readonly BACKEND_IDS=(cottendns masterdnsvpn stormdns thefeed)
  readonly PROJECT_STATE_DIR=/var/lib/cottenrouter/projects
  readonly UPSTREAM_STATE_DIR=/var/lib/cottenrouter/upstreams

  for service_name in "${BACKEND_SERVICES[@]}"; do
    if systemctl is-active --quiet "${service_name}" 2>/dev/null; then
      systemctl stop "${service_name}" 2>/dev/null || true
    fi
    systemctl disable "${service_name}" 2>/dev/null || true
  done

  # Stop and disable all SlipGate services (slipgate-dnsrouter plus tunnel units).
  shopt -s nullglob
  for unit_file in /etc/systemd/system/slipgate-*.service; do
    svc_name=$(basename "${unit_file}" .service)
    [[ ${svc_name} =~ ^[A-Za-z0-9_.@-]+$ ]] || continue
    if systemctl is-active --quiet "${svc_name}" 2>/dev/null; then
      systemctl stop "${svc_name}" 2>/dev/null || true
    fi
    systemctl disable "${svc_name}" 2>/dev/null || true
  done
  shopt -u nullglob

  # Run pinned native uninstallers for each backend that has an install record.
  for backend_id in "${BACKEND_IDS[@]}" slipgate; do
    manifest="${PROJECT_STATE_DIR}/${backend_id}.json"
    pending="${PROJECT_STATE_DIR}/${backend_id}.pending.json"
    for record in "${manifest}" "${pending}"; do
      [[ -f ${record} ]] || continue
      installer_file=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['installer_file'])" "${record}" 2>/dev/null) || continue
      [[ -f ${installer_file} && ${installer_file} == /var/lib/cottenrouter/upstreams/* ]] || continue
      echo "Running pinned native uninstaller for ${backend_id}..."
      bash "${installer_file}" --uninstall 2>/dev/null || echo "  native ${backend_id} uninstaller returned non-zero (may be expected)"
      break
    done
  done

  # SlipGate native uninstaller
  if command -v slipgate >/dev/null 2>&1; then
    slipgate uninstall 2>/dev/null || true
  fi

  # Remove backend directories and systemd units.
  for backend_dir in "${BACKEND_DIRS[@]}"; do
    [[ -L ${backend_dir} ]] && continue
    rm -rf -- "${backend_dir}" 2>/dev/null || true
  done
  if [[ -d /etc/slipgate && ! -L /etc/slipgate ]]; then
    rm -rf -- /etc/slipgate 2>/dev/null || true
  fi
  rmdir --ignore-fail-on-non-empty /opt/cottenrouter/backends 2>/dev/null || true
  rmdir --ignore-fail-on-non-empty /opt/cottenrouter 2>/dev/null || true

  # Remove backend service units left behind.
  for service_name in "${BACKEND_SERVICES[@]}" slipgate-dnsrouter; do
    rm -f -- "/etc/systemd/system/${service_name}.service" 2>/dev/null || true
    rm -rf -- "/etc/systemd/system/${service_name}.service.d" 2>/dev/null || true
  done
  shopt -s nullglob
  for unit_file in /etc/systemd/system/slipgate-*.service; do
    rm -f -- "${unit_file}" 2>/dev/null || true
  done
  for dropin_dir in /etc/systemd/system/slipgate-*.service.d; do
    rm -rf -- "${dropin_dir}" 2>/dev/null || true
  done
  shopt -u nullglob

  # Remove install records.
  rm -rf -- "${PROJECT_STATE_DIR}" "${UPSTREAM_STATE_DIR}" 2>/dev/null || true

  systemctl daemon-reload
  backends_removed=true
fi

echo "CottenRouter was removed."
if ! ${purge}; then
  echo "Router configuration was preserved in ${CONFIG_DIR}."
fi
if ! ${remove_swap}; then
  echo "Swap was preserved. Use --remove-swap --confirm CottenRouter to remove installer-owned swap."
fi
if ${purge} && ! ${account_removed} && id cottenrouter >/dev/null 2>&1; then
  echo "The cottenrouter account was preserved: this installer has no ownership marker for it."
fi
if ${firewall_removed}; then
  echo "Firewall rules added by this installer were removed."
fi
if ${backends_removed}; then
  echo "All managed DNS backend services and their data were removed."
else
  echo "Backend projects, panels, backend data, and pre-existing firewall rules were not removed."
  if ! ${purge_backends}; then
    echo "Use --purge --purge-backends --confirm CottenRouter to also remove all DNS backends."
  fi
fi
if [[ -d ${SWAP_DIRECTORY}/projects || -d ${SWAP_DIRECTORY}/upstreams ]]; then
  echo "Pinned backend uninstall records were preserved in ${SWAP_DIRECTORY}."
fi
