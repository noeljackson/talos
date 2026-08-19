#!/usr/bin/env bash

set -euo pipefail

usage() {
  echo "usage: $0 write|verify RECEIPT ARTIFACT..." >&2
}

source_fingerprint() {
  {
    git rev-parse HEAD
    git diff --binary HEAD

    while IFS= read -r -d '' path; do
      case "${path}" in
        .tmp/*) continue ;;
      esac

      if [[ -L "${path}" ]]; then
        printf 'symlink %s %s\n' "${path}" "$(readlink "${path}")"
      else
        sha256sum -- "${path}"
      fi
    done < <(git ls-files --others --exclude-standard -z)
  } | sha256sum | awk '{ print $1 }'
}

if (( $# < 3 )); then
  usage
  exit 2
fi

mode="$1"
receipt="$2"
shift 2

case "${mode}" in
  write)
    umask 077

    fingerprint="$(source_fingerprint)"
    receipt_tmp="$(mktemp "${receipt}.tmp.XXXXXX")"
    trap 'rm -f -- "${receipt_tmp}"' EXIT

    {
      printf 'source %s\n' "${fingerprint}"
      sha256sum -- "$@"
    } > "${receipt_tmp}"

    mv -f -- "${receipt_tmp}" "${receipt}"
    trap - EXIT
    ;;
  verify)
    [[ -r "${receipt}" ]] || {
      echo "run e2e-qemu-prepare first" >&2
      exit 1
    }

    expected_source="source $(source_fingerprint)"
    [[ "$(sed -n '1p' "${receipt}")" == "${expected_source}" ]] || {
      echo "prepared QEMU artifacts belong to a different source snapshot" >&2
      exit 1
    }

    expected_hashes="$(mktemp)"
    trap 'rm -f -- "${expected_hashes}"' EXIT
    sha256sum -- "$@" > "${expected_hashes}"

    cmp -s <(tail -n +2 "${receipt}") "${expected_hashes}" || {
      echo "prepared QEMU artifacts changed; rerun e2e-qemu-prepare" >&2
      exit 1
    }
    ;;
  *)
    usage
    exit 2
    ;;
esac
