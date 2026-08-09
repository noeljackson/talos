#!/usr/bin/env bash

set -euo pipefail
umask 077

die() {
  printf 'validate-squashfs-labels: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat >&2 <<'USAGE'
Usage: validate-squashfs-labels.sh <rootfs.sqsh>
       validate-squashfs-labels.sh --pseudo-file <unsquashfs-pseudo-file>
USAGE
}

require_label() {
  local path="$1"
  local label="$2"

  LC_ALL=C grep -aqE "^${path} x security[.]selinux=.*:${label}:" "$pseudo_file" || \
    die "required SELinux label is absent: ${path} -> ${label}"
}

pseudo_file=""
temporary_pseudo=""

cleanup() {
  if [[ -n "$temporary_pseudo" && -f "$temporary_pseudo" ]]; then
    rm -f "$temporary_pseudo"
  fi
}
trap cleanup EXIT INT TERM

case "$#:${1:-}" in
  2:--pseudo-file)
    pseudo_file="$2"
    ;;
  1:*)
    command -v unsquashfs >/dev/null 2>&1 || die 'unsquashfs is required'
    [[ -f "$1" && -s "$1" ]] || die "SquashFS image is absent or empty: $1"
    temporary_pseudo="$(mktemp "${TMPDIR:-/tmp}/talos-squashfs-labels.XXXXXX")" || \
      die 'could not allocate a temporary pseudo-file'
    pseudo_file="$temporary_pseudo"
    unsquashfs -pf "$pseudo_file" "$1" >/dev/null || \
      die 'could not inspect the generated SquashFS image'
    ;;
  *)
    usage
    exit 2
    ;;
esac

[[ -f "$pseudo_file" && -s "$pseudo_file" ]] || die 'pseudo-file is absent or empty'

label_count="$(LC_ALL=C grep -acF 'security.selinux=' "$pseudo_file" || true)"
[[ "$label_count" =~ ^[0-9]+$ && "$label_count" -gt 0 ]] || \
  die 'generated SquashFS contains no SELinux xattrs'

LC_ALL=C grep -aqE 'security[.]selinux=.*:unlabeled_t:' "$pseudo_file" && \
  die 'generated SquashFS contains an unlabeled_t entry'

require_label '/' 'rootfs_t'
require_label 'usr/bin' 'bin_exec_t'
require_label 'usr/bin/init' 'init_exec_t'
require_label 'usr/lib' 'lib_t'

printf 'validate-squashfs-labels: PASS labels=%s\n' "$label_count" >&2
