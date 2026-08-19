#!/usr/bin/env bash

set -euo pipefail

tool="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/qemu-prepared-receipt.sh"
test_repo="$(mktemp -d)"
trap 'rm -rf -- "${test_repo}"' EXIT

expect_verify_failure() {
  if "${tool}" verify receipt artifact.bin 2>/dev/null; then
    echo "expected receipt verification to fail" >&2
    exit 1
  fi
}

git -C "${test_repo}" init -q
printf 'artifact.bin\nreceipt\n.tmp/\n' > "${test_repo}/.gitignore"
printf 'tracked\n' > "${test_repo}/source.txt"
git -C "${test_repo}" add .gitignore source.txt
git -C "${test_repo}" -c user.name=test -c user.email=test@example.invalid commit -qm initial

printf 'artifact\n' > "${test_repo}/artifact.bin"

(
  cd "${test_repo}"
  "${tool}" write receipt artifact.bin
  [[ "$(wc -l < receipt)" -eq 3 ]]
  [[ "$(sed -n '1p' receipt)" =~ ^source\ [0-9a-f]{64}$ ]]
  [[ "$(sed -n '2p' receipt)" == "version $(git describe --tag --always --dirty --match 'v[0-9]*')" ]]
  [[ "$("${tool}" version receipt)" == "$(git describe --tag --always --dirty --match 'v[0-9]*')" ]]
  "${tool}" verify receipt artifact.bin

  sed -i '2c version stale' receipt
  expect_verify_failure
  "${tool}" write receipt artifact.bin
  "${tool}" verify receipt artifact.bin

  mkdir -p .tmp
  printf 'ignored diagnostic\n' > .tmp/diagnostic
  "${tool}" verify receipt artifact.bin

  printf 'changed\n' > source.txt
  expect_verify_failure
  printf 'tracked\n' > source.txt
  "${tool}" verify receipt artifact.bin

  printf 'untracked source\n' > new-source.txt
  expect_verify_failure
  rm -f new-source.txt
  "${tool}" verify receipt artifact.bin

  printf 'changed artifact\n' > artifact.bin
  expect_verify_failure
)
