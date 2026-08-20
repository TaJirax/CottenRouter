#!/usr/bin/env bash
set -euo pipefail

# Bounded queues are the primary memory protection. This file is an additional
# last-resort cushion for small Linux VPS instances.
readonly TARGET_SWAP_KIB=2097152
readonly SWAP_DIRECTORY=/var/lib/cottenrouter
readonly SWAP_FILE=${SWAP_DIRECTORY}/swapfile
readonly SWAP_MARKER=${SWAP_DIRECTORY}/swapfile.created-by-cottenrouter
readonly SWAP_CANDIDATE=${SWAP_FILE}.new

if [[ ${EUID} -ne 0 ]]; then
  echo "ensure-swap.sh must run as root" >&2
  exit 1
fi
if [[ ! -r /proc/meminfo ]]; then
  echo "ensure-swap.sh supports Linux only" >&2
  exit 1
fi

if [[ -L ${SWAP_DIRECTORY} || ( -e ${SWAP_DIRECTORY} && ! -d ${SWAP_DIRECTORY} ) ]]; then
  echo "Refusing unsafe CottenRouter swap directory: ${SWAP_DIRECTORY}" >&2
  exit 1
fi
if [[ -e ${SWAP_DIRECTORY} && $(stat -c '%u' "${SWAP_DIRECTORY}") != 0 ]]; then
  echo "Refusing non-root-owned CottenRouter swap directory" >&2
  exit 1
fi
if [[ -L ${SWAP_FILE} || -L ${SWAP_MARKER} || -L ${SWAP_CANDIDATE} ]]; then
  echo "Refusing symlinked CottenRouter swap path" >&2
  exit 1
fi
if [[ -e ${SWAP_FILE} && ! -f ${SWAP_MARKER} ]]; then
  echo "Refusing existing swap file without CottenRouter ownership marker: ${SWAP_FILE}" >&2
  exit 1
fi
if [[ -e ${SWAP_MARKER} && ! -f ${SWAP_FILE} ]]; then
  echo "Refusing stale CottenRouter swap ownership marker" >&2
  exit 1
fi
if [[ -e ${SWAP_FILE} ]]; then
  if [[ ! -f ${SWAP_FILE} || ! -f ${SWAP_MARKER} || $(stat -c '%u' "${SWAP_FILE}") != 0 || $(stat -c '%u' "${SWAP_MARKER}") != 0 ]]; then
    echo "Refusing swap artifacts not owned by root and CottenRouter" >&2
    exit 1
  fi
fi

current_swap_kib=$(awk '/^SwapTotal:/ { print $2 }' /proc/meminfo)
if (( current_swap_kib >= TARGET_SWAP_KIB )); then
  echo "Swap is already at least 2 GiB (${current_swap_kib} KiB)."
  exit 0
fi

install -d -o root -g root -m 0700 "${SWAP_DIRECTORY}"
if [[ ! -e ${SWAP_FILE} ]]; then
  [[ ! -e ${SWAP_CANDIDATE} ]] || { echo "Refusing pre-existing swap staging file" >&2; exit 1; }
  candidate_created=false
  final_created=false
  marker_created=false
  cleanup_candidate() {
    if ${candidate_created} && [[ -f ${SWAP_CANDIDATE} ]]; then
      rm -f -- "${SWAP_CANDIDATE}"
    fi
    if ${final_created} && ! ${marker_created} && [[ -f ${SWAP_FILE} ]]; then
      rm -f -- "${SWAP_FILE}"
    fi
  }
  trap cleanup_candidate ERR
  trap 'cleanup_candidate; exit 1' HUP INT TERM
  install -o root -g root -m 0600 /dev/null "${SWAP_CANDIDATE}"
  candidate_created=true
  allocation_kib=$((TARGET_SWAP_KIB - current_swap_kib))
  available_kib=$(df -Pk "${SWAP_DIRECTORY}" | awk 'NR == 2 { print $4 }')
  if [[ ! ${available_kib} =~ ^[0-9]+$ ]] || (( available_kib < allocation_kib + 65536 )); then
    echo "At least $((allocation_kib + 65536)) KiB of free disk is required to create safe swap headroom." >&2
    exit 1
  fi
  if ! fallocate -l "${allocation_kib}K" "${SWAP_CANDIDATE}"; then
    dd if=/dev/zero of="${SWAP_CANDIDATE}" bs=1K count="${allocation_kib}" status=progress
  fi
  mkswap "${SWAP_CANDIDATE}"
  mv -- "${SWAP_CANDIDATE}" "${SWAP_FILE}"
  candidate_created=false
  final_created=true
  install -o root -g root -m 0600 /dev/null "${SWAP_MARKER}"
  marker_created=true
  trap - ERR HUP INT TERM
fi

# chmod/swapon is reachable only for a root-owned regular file carrying the
# marker, so an unrelated administrator file is never mutated or activated.
chmod 0600 "${SWAP_FILE}"
if ! swapon --show=NAME --noheadings | grep -Fxq "${SWAP_FILE}"; then
  swapon "${SWAP_FILE}"
fi

fstab_entry="${SWAP_FILE} none swap sw 0 0"
if ! grep -Fqx "${fstab_entry}" /etc/fstab; then
  [[ -f /etc/fstab && ! -L /etc/fstab ]] || { echo "Refusing unsafe /etc/fstab" >&2; exit 1; }
  fstab_temp=$(mktemp /etc/fstab.cottenrouter.XXXXXX)
  cp --preserve=mode,ownership -- /etc/fstab "${fstab_temp}"
  printf '%s\n' "${fstab_entry}" >> "${fstab_temp}"
  mv -f -- "${fstab_temp}" /etc/fstab
fi

current_swap_kib=$(awk '/^SwapTotal:/ { print $2 }' /proc/meminfo)
if (( current_swap_kib < TARGET_SWAP_KIB )); then
  echo "Swap remains below 2 GiB; inspect existing swap configuration." >&2
  exit 1
fi
echo "Swap safeguard active (${current_swap_kib} KiB total)."
