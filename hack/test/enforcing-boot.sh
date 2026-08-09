#!/usr/bin/env bash

set -euo pipefail
umask 077

die() {
  printf 'enforcing-boot: %s\n' "$*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

usage() {
  cat >&2 <<'USAGE'
Usage: enforcing-boot.sh <imager-image>

Builds an amd64 UKI with SELinux enforcing, extracts its boot payload, and
requires a stable no-network QEMU maintenance boot with no AVC, init-exec, or
reboot evidence. Protected build and serial captures are never printed.
USAGE
}

[[ $# -eq 1 ]] || {
  usage
  exit 2
}

imager_image="$1"
boot_seconds="${TALOS_ENFORCING_BOOT_SECONDS:-45}"
tmp_root="${TALOS_ENFORCING_BOOT_TMPDIR:-${TMPDIR:-/tmp}}"
tmp_dir=""

[[ "$boot_seconds" =~ ^[0-9]+$ && "$boot_seconds" -ge 30 ]] || \
  die 'TALOS_ENFORCING_BOOT_SECONDS must be an integer of at least 30'

need docker
need objcopy
need qemu-system-x86_64
need shred
need timeout
need zstd

mkdir -p "$tmp_root"
[[ -d "$tmp_root" && -w "$tmp_root" && -x "$tmp_root" ]] || \
  die "temporary directory is not usable: $tmp_root"
tmp_dir="$(mktemp -d "$tmp_root/talos-enforcing-boot.XXXXXX")" || \
  die 'could not create protected temporary directory'
chmod 0700 "$tmp_dir"

cleanup() {
  local protected

  if [[ -n "$tmp_dir" && -d "$tmp_dir" ]]; then
    for protected in "$tmp_dir/build.log" "$tmp_dir/serial.log" "$tmp_dir/qemu.stderr"; do
      [[ -f "$protected" ]] && shred -u "$protected" 2>/dev/null || true
    done
    rm -rf "$tmp_dir"
  fi
}
trap cleanup EXIT INT TERM

mkdir -m 0700 "$tmp_dir/out"
: >"$tmp_dir/build.log"
chmod 0600 "$tmp_dir/build.log"

docker run --rm \
  --network=none \
  --user "$(id -u):$(id -g)" \
  --volume "$tmp_dir/out:/out" \
  --env SOURCE_DATE_EPOCH=0 \
  --env DETERMINISTIC_SEED=1 \
  "$imager_image" metal-uki \
  --arch amd64 \
  --extra-kernel-arg selinux=1 \
  --extra-kernel-arg enforcing=1 \
  --extra-kernel-arg console=ttyS0 \
  >"$tmp_dir/build.log" 2>&1 || die 'candidate UKI build failed'

candidate="$tmp_dir/out/metal-amd64-uki.efi"
if [[ ! -s "$candidate" && -s "$candidate.zst" ]]; then
  zstd -q -d "$candidate.zst" -o "$candidate" || die 'could not decompress candidate UKI'
fi
[[ -s "$candidate" ]] || die 'candidate UKI was not produced'

objcopy \
  --dump-section .linux="$tmp_dir/vmlinuz" \
  --dump-section .initrd="$tmp_dir/initramfs" \
  --dump-section .cmdline="$tmp_dir/cmdline.raw" \
  "$candidate" >/dev/null 2>&1 || die 'could not extract candidate UKI sections'
[[ -s "$tmp_dir/vmlinuz" && -s "$tmp_dir/initramfs" && -s "$tmp_dir/cmdline.raw" ]] || \
  die 'candidate UKI is missing a required boot section'

tr -d '\000' <"$tmp_dir/cmdline.raw" >"$tmp_dir/cmdline"
grep -qE '(^|[[:space:]])selinux=1($|[[:space:]])' "$tmp_dir/cmdline" || \
  die 'candidate UKI does not enable SELinux'
grep -qE '(^|[[:space:]])enforcing=1($|[[:space:]])' "$tmp_dir/cmdline" || \
  die 'candidate UKI does not enable enforcement'

truncate -s 8G "$tmp_dir/disk.raw"
: >"$tmp_dir/serial.log"
: >"$tmp_dir/qemu.stderr"
chmod 0600 "$tmp_dir/serial.log" "$tmp_dir/qemu.stderr"

set +e
timeout --signal=TERM --kill-after=5 "$boot_seconds" \
  qemu-system-x86_64 \
  -machine q35,accel=kvm \
  -cpu host \
  -smp 2 \
  -m 2048 \
  -kernel "$tmp_dir/vmlinuz" \
  -initrd "$tmp_dir/initramfs" \
  -append "$(<"$tmp_dir/cmdline")" \
  -drive "file=$tmp_dir/disk.raw,format=raw,if=virtio" \
  -display none \
  -monitor none \
  -serial "file:$tmp_dir/serial.log" \
  -nic none \
  -no-reboot \
  >/dev/null 2>"$tmp_dir/qemu.stderr"
qemu_status=$?
set -e

[[ "$qemu_status" -eq 124 ]] || die "QEMU did not remain stable for ${boot_seconds}s"
grep -aiq 'apid' "$tmp_dir/serial.log" || die 'apid startup marker is absent'
grep -aiq 'maintenance service' "$tmp_dir/serial.log" || die 'maintenance startup marker is absent'
grep -aiqE 'machine.*running' "$tmp_dir/serial.log" || die 'running-state marker is absent'
! grep -aiqE 'avc:.*denied' "$tmp_dir/serial.log" || die 'SELinux AVC denial detected'
! grep -aiqE 'failed to execute.*/sbin/init|permission denied.*(/sbin/)?init' "$tmp_dir/serial.log" || \
  die 'init execution failure detected'
! grep -aiqE 'rebooting|restarting system|will reboot|reboot sequence' "$tmp_dir/serial.log" || \
  die 'guest reboot marker detected'

printf 'enforcing-boot: PASS seconds=%s network=none selinux=enforcing avc=0\n' "$boot_seconds"
