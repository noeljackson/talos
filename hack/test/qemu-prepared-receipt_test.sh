#!/usr/bin/env bash

set -euo pipefail

tool="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/qemu-prepared-receipt.sh"
test_repo="$(mktemp -d)"
trap 'rm -rf -- "${test_repo}"' EXIT

expect_verify_failure() {
  if "${tool}" verify receipt talosctl artifact.bin talosctl 2>/dev/null; then
    echo "expected receipt verification to fail" >&2
    exit 1
  fi
}

git -C "${test_repo}" init -q
printf 'artifact.bin\ntalosctl\nreceipt\n.tmp/\n' > "${test_repo}/.gitignore"
printf 'tracked\n' > "${test_repo}/source.txt"
git -C "${test_repo}" add .gitignore source.txt
git -C "${test_repo}" -c user.name=test -c user.email=test@example.invalid commit -qm initial

printf 'artifact\n' > "${test_repo}/artifact.bin"
printf '%s\n' '#!/usr/bin/env bash' "printf 'Client:\\nTalos v1.2.3-test\\n'" > "${test_repo}/talosctl"
chmod +x "${test_repo}/talosctl"

(
  cd "${test_repo}"
  "${tool}" write receipt talosctl artifact.bin talosctl
  [[ "$(wc -l < receipt)" -eq 4 ]]
  [[ "$(sed -n '1p' receipt)" =~ ^source\ [0-9a-f]{64}$ ]]
  [[ "$(sed -n '2p' receipt)" == "version v1.2.3-test" ]]
  [[ "$("${tool}" version receipt)" == "v1.2.3-test" ]]
  "${tool}" verify receipt talosctl artifact.bin talosctl

  sed -i '2c version stale' receipt
  expect_verify_failure
  "${tool}" write receipt talosctl artifact.bin talosctl
  "${tool}" verify receipt talosctl artifact.bin talosctl

  mkdir -p .tmp
  printf 'ignored diagnostic\n' > .tmp/diagnostic
  "${tool}" verify receipt talosctl artifact.bin talosctl

  printf 'changed\n' > source.txt
  expect_verify_failure
  printf 'tracked\n' > source.txt
  "${tool}" verify receipt talosctl artifact.bin talosctl

  printf 'untracked source\n' > new-source.txt
  expect_verify_failure
  rm -f new-source.txt
  "${tool}" verify receipt talosctl artifact.bin talosctl

  printf 'changed artifact\n' > artifact.bin
  expect_verify_failure
)
