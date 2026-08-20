#!/usr/bin/env bash
set -euo pipefail

# Bounded queues are the primary memory protection. This file is an additional
# last-resort cushion for small Linux VPS instances.
readonly TARGET_SWAP_KIB=2097152
readonly SWAP_DIRECTORY=/var/lib/cottenrouter
readonly SWAP_FILE=${SWAP_DIRECTORY}/swapfile

if [[ ${EUID} -ne 0 ]]; then
  echo "ensure-swap.sh must run as root" >&2
  exit 1
fi
if [[ ! -r /proc/meminfo ]]; then
  echo "ensure-swap.sh supports Linux only" >&2
  exit 1
fi

current_swap_kib=$(awk '/^SwapTotal:/ { print $2 }' /proc/meminfo)
if (( current_swap_kib >= TARGET_SWAP_KIB )); then
  echo "Swap is already at least 2 GiB (${current_swap_kib} KiB)."
  exit 0
fi

install -d -m 0700 "${SWAP_DIRECTORY}"
if [[ ! -e ${SWAP_FILE} ]]; then
  if ! fallocate -l 2G "${SWAP_FILE}"; then
    dd if=/dev/zero of="${SWAP_FILE}" bs=1M count=2048 status=progress
  fi
  chmod 0600 "${SWAP_FILE}"
  mkswap "${SWAP_FILE}"
fi

chmod 0600 "${SWAP_FILE}"
if ! swapon --show=NAME --noheadings | grep -Fxq "${SWAP_FILE}"; then
  swapon "${SWAP_FILE}"
fi

fstab_entry="${SWAP_FILE} none swap sw 0 0"
if ! grep -Fqx "${fstab_entry}" /etc/fstab; then
  printf '%s\n' "${fstab_entry}" >> /etc/fstab
fi

current_swap_kib=$(awk '/^SwapTotal:/ { print $2 }' /proc/meminfo)
if (( current_swap_kib < TARGET_SWAP_KIB )); then
  echo "Swap remains below 2 GiB; inspect existing swap configuration." >&2
  exit 1
fi
echo "Swap safeguard active (${current_swap_kib} KiB total)."
