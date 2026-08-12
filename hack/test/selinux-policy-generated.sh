#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
generated_dir="$(mktemp -d "${TMPDIR:-/tmp}/talos-selinux-policy.XXXXXX")"

cleanup() {
    find "${generated_dir}" -type f -delete 2>/dev/null || true
    find "${generated_dir}" -depth -type d -empty -delete 2>/dev/null || true
}
trap cleanup EXIT INT TERM

make -C "${repo_root}" local-selinux-generate \
    DEST="${generated_dir}" \
    PLATFORM=linux/amd64 >/dev/null

generated_policy="${generated_dir}/policy/policy.33"
tracked_policy="${repo_root}/internal/pkg/selinux/policy/policy.33"

if ! cmp -s "${generated_policy}" "${tracked_policy}"; then
    echo "compiled SELinux policy is stale; run 'make generate-selinux' and commit policy.33" >&2

    exit 1
fi

echo "compiled SELinux policy matches the deterministic CIL output"
