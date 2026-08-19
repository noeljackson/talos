#!/usr/bin/env bash

set -euo pipefail

usage() {
  echo "usage: $0 write|verify RECEIPT TALOSCTL ARTIFACT... | version RECEIPT" >&2
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

artifact_version() {
  local output
  local talosctl="$1"
  local version

  output="$("${talosctl}" version --client --short)"
  version="$(sed -n 's/^Talos \(v[^[:space:]]*\)$/\1/p' <<< "${output}")"

  [[ -n "${version}" && "$(grep -c '^Talos v[^[:space:]]*$' <<< "${output}")" -eq 1 ]] || {
    echo "prepared talosctl did not report one exact client version" >&2
    exit 1
  }

  printf '%s\n' "${version}"
}

if (( $# < 2 )); then
  usage
  exit 2
fi

mode="$1"
receipt="$2"
shift 2

case "${mode}" in
  write)
    (( $# > 1 )) || {
      usage
      exit 2
    }

    talosctl="$1"
    shift
    umask 077

    fingerprint="$(source_fingerprint)"
    receipt_tmp="$(mktemp "${receipt}.tmp.XXXXXX")"
    trap 'rm -f -- "${receipt_tmp}"' EXIT

    {
      printf 'source %s\n' "${fingerprint}"
      printf 'version %s\n' "$(artifact_version "${talosctl}")"
      sha256sum -- "$@"
    } > "${receipt_tmp}"

    mv -f -- "${receipt_tmp}" "${receipt}"
    trap - EXIT
    ;;
  verify)
    (( $# > 1 )) || {
      usage
      exit 2
    }

    talosctl="$1"
    shift
    [[ -r "${receipt}" ]] || {
      echo "run e2e-qemu-prepare first" >&2
      exit 1
    }

    expected_source="source $(source_fingerprint)"
    [[ "$(sed -n '1p' "${receipt}")" == "${expected_source}" ]] || {
      echo "prepared QEMU artifacts belong to a different source snapshot" >&2
      exit 1
    }

    expected_version="version $(artifact_version "${talosctl}")"
    [[ "$(sed -n '2p' "${receipt}")" == "${expected_version}" ]] || {
      echo "prepared QEMU version belongs to a different source snapshot" >&2
      exit 1
    }

    expected_hashes="$(mktemp)"
    trap 'rm -f -- "${expected_hashes}"' EXIT
    sha256sum -- "$@" > "${expected_hashes}"

    cmp -s <(tail -n +3 "${receipt}") "${expected_hashes}" || {
      echo "prepared QEMU artifacts changed; rerun e2e-qemu-prepare" >&2
      exit 1
    }
    ;;
  version)
    (( $# == 0 )) || {
      usage
      exit 2
    }

    [[ -r "${receipt}" ]] || {
      echo "run e2e-qemu-prepare first" >&2
      exit 1
    }

    receipt_version="$(sed -n '2s/^version //p' "${receipt}")"
    [[ -n "${receipt_version}" && "${receipt_version}" != *[[:space:]]* ]] || {
      echo "prepared QEMU receipt has no valid version" >&2
      exit 1
    }

    printf '%s\n' "${receipt_version}"
    ;;
  *)
    usage
    exit 2
    ;;
esac
