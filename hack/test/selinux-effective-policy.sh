#!/usr/bin/env bash

# Run only in the repository's tools stage, using the same secilc and source
# set as selinux-generate. No host SELinux installation or live policy load.
set -euo pipefail

policy_dir="${1:?policy directory required}"
fixtures_dir="${2:?fixtures directory required}"
proof_dir="$(mktemp -d)"
trap 'find "${proof_dir}" -type f -delete; rmdir "${proof_dir}"' EXIT

shopt -s nullglob
sources=("${policy_dir}"/*/*.cil)
fixtures=("${fixtures_dir}"/*.cil)
if (( ${#sources[@]} == 0 || ${#fixtures[@]} == 0 )); then
    echo "SELinux policy or required proof fixtures are missing" >&2
    exit 1
fi

compile() {
    secilc -o "${proof_dir}/policy.33" -f "${proof_dir}/file_contexts" \
        -c 33 "${sources[@]}" "$@" -O
}

# The baseline must satisfy every checked-in neverallow.
compile
count=0
for fixture in "${fixtures[@]}"; do
    # Each fixture adds either one forbidden privilege (widen-*) or a
    # neverallow against one required positive edge (require-*).
    # Both must fail specifically because of the expanded allow/neverallow
    # conflict; syntax errors, unavailable tools, or missing types do not pass.
    if compile "${fixture}" >"${proof_dir}/compile.log" 2>&1; then
        echo "SELinux effective-policy fixture unexpectedly compiled: ${fixture##*/}" >&2
        exit 1
    fi
    if ! grep -q 'neverallow check failed' "${proof_dir}/compile.log"; then
        echo "SELinux fixture failed without proving its permission boundary: ${fixture##*/}" >&2
        cat "${proof_dir}/compile.log" >&2
        exit 1
    fi
    count=$((count + 1))
    echo "PASS ${fixture##*/}"
done
echo "SELinux effective policy: baseline and ${count} compiler-enforced fixtures passed"
