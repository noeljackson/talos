#!/usr/bin/env bash

set -euo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
validator="$repo_root/hack/validate-squashfs-labels.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/talos-squashfs-label-test.XXXXXX")"

cleanup() {
  rm -rf "$test_root"
}
trap cleanup EXIT INT TERM

write_good_fixture() {
  printf '%s\n' \
    '/ D 0 755 0 0' \
    '/ x security.selinux=0tsystem_u:object_r:rootfs_t:s0\000' \
    'usr/bin D 0 755 0 0' \
    'usr/bin x security.selinux=0tsystem_u:object_r:bin_exec_t:s0\000' \
    'usr/bin/init R 0 755 0 0 1 1 0' \
    'usr/bin/init x security.selinux=0tsystem_u:object_r:init_exec_t:s0\000' \
    'usr/lib D 0 755 0 0' \
    'usr/lib x security.selinux=0tsystem_u:object_r:lib_t:s0\000'
}

expect_failure() {
  local fixture="$1"

  if "$validator" --pseudo-file "$fixture" >/dev/null 2>&1; then
    printf 'squashfs-labels test: expected rejection for %s\n' "${fixture##*/}" >&2
    exit 1
  fi
}

write_good_fixture >"$test_root/good"
"$validator" --pseudo-file "$test_root/good" >/dev/null

printf '%s\n' '/ D 0 755 0 0' >"$test_root/no-xattrs"
expect_failure "$test_root/no-xattrs"

write_good_fixture | sed 's/:init_exec_t:/:unlabeled_t:/' >"$test_root/unlabeled-init"
expect_failure "$test_root/unlabeled-init"

write_good_fixture | grep -v '^usr/bin/init x ' >"$test_root/missing-init-label"
expect_failure "$test_root/missing-init-label"

write_good_fixture | grep -v '^usr/lib x ' >"$test_root/missing-lib-label"
expect_failure "$test_root/missing-lib-label"

printf 'squashfs-labels test: PASS\n'
