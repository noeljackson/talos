# Native RAID cold-boot proof

This opt-in provision suite uses the existing native QEMU/CNI topology, one
Kubernetes/etcd-disabled node with 4 GiB RAM, and disposable serial-selected disks.
It is not an online hot-unplug test or physical-hardware qualification. Passing
unit tests or compiling the suite is not evidence that the VM lifecycle passed.

The `lifecycle` fixture installs on a two-member metadata-1.0 system mirror and
uses a separate two-member metadata-1.2 data mirror. A filesystem on the latter
is **test-only**; production data remains unformatted. Each member of each mirror
is physically absent during a cold boot, its surviving peer accepts a new durable
marker, and the same original backing disk is restored. The suite requires exact
serial membership, two configured members (no grow), no degraded members and idle
resync before accepting rejoin. It reads the final recovered peer alone and
reorders all disks with data/decoy devices first. Array UUIDs, filesystem UUID,
STATE identity, and both markers must persist. In a one-member state Talos's MD
controller may not publish a UUID; the test does not claim an unobserved UUID
match, and verifies the UUID again on rejoin.

Separate fresh `wrong-serial` and `missing-serial` fixtures require installation
to stay pending and no system disk or installation partitions to appear, including
on data and prefix-decoy devices. Before that observation they verify the actual
initial QEMU launch and complete guest physical serial inventory (five disks for
`wrong-serial`, four for `missing-serial`); missing discovery is a failure, not
evidence that the selector safely blocked installation. Guest inventory must stay
complete throughout the sustained negative observation. They never format the
data mirror.

## Required artifact closure and host review

Before authorizing execution, bind checksums/source heads for:

- The native integration-test-provision and talosctl binaries containing the
  disk-control helper; both must be from this fixture's source. The integration
  binary's embedded `gendata.ArtifactsPath` must point at the reviewed artifacts.
- Matching `vmlinuz-amd64`, `initramfs-amd64.xz`, and a reachable installer image
  selected by the exact version/registry flags below. The installed UKI must boot
  with SELinux enforcing without command-line reinjection. Initial raw-kernel
  boot explicitly gets `enforcing=1`; every observed boot checks SELinux enforcing
  and `SecurityProfileConfig.workloadIsolation=true`. No permissive fallback is
  accepted. The initial installer does not auto-reboot, so the fixture can observe
  the raw boot before performing its first strict disk-only cold boot.
- The native CNI bundle `talosctl-cni-bundle-amd64.tar.gz`, QEMU, and the UEFI
  firmware selected by the existing native provisioner. Record their versions.
- The existing debug-suite image `docker.io/library/alpine:3.23`, resolved and
  recorded by digest through the reviewed registry/mirror path. Its ordinary
  privileged DebugService profile writes only the two fixture markers; no HOST_NS
  profile, custom host route, relay, firewall exception, or boot-media fallback
  is added by this suite.

The host must be Linux/amd64 with usable `/dev/kvm`, privileges for the checked-in
native CNI/network-namespace topology, and at least 32 GiB free under `/var/tmp`
(full RAID resync makes sparse disks consume real space). Allow 4 GiB per VM plus
host overhead. The three fixtures run serially. Review that native CIDR
`172.21.0.0/24` and cluster names `raid-lifecycle`, `raid-wrong-serial`, and
`raid-missing-serial` do not collide with existing resources. No live execution is
implied by this document or the unit/compile checks.

## Focused entrypoint

After artifact/topology review and explicit VM execution coordination, run the
native integration binary directly with the same CNI and MTU inputs as
`provision-tests.sh`, plus the explicit opt-in. The shell variables below must be
bound to the reviewed artifacts and image version; this command does not build,
publish, load images, acquire credentials, or modify firewall rules.

```sh
"${RAID_ARTIFACTS:?}/integration-test-provision-linux-amd64" \
  -test.v -test.timeout=2h \
  -test.run='^TestIntegration$/^provision[.]RAIDBootSuite[.](lifecycle|wrong-serial|missing-serial)-TR3$/^TestProof$' \
  -talos.provision.raid-proof \
  -talos.config=/dev/null \
  -talos.talosctlpath="${RAID_ARTIFACTS:?}/talosctl-linux-amd64" \
  -talos.version="${RAID_VERSION:?}" \
  -talos.provision.target-installer-registry="${RAID_INSTALLER_REGISTRY:?}" \
  -talos.provision.mtu=1430 \
  -talos.provision.cni-bundle-url="${RAID_ARTIFACTS:?}/talosctl-cni-bundle-\${ARCH}.tar.gz"
```

Use only the existing native `-talos.provision.registry-mirror` option if a reviewed
mirror is required. The fixture creates its private state and CNI root under
`/var/tmp/talos-raid-*` and never opens or merges a user talosconfig. Teardown
destroys only this cluster and removes only its private state; native log/support
archives use the existing `/tmp/logs-raid-*.tar.gz` and `/tmp/support-raid-*.zip`
locations. Preserve the test transcript and those archives, including verified
generation/PID/actual launch inventories for each cold boot and the negative
fixtures' initial boots. The generation is checked against the live process via
`/proc`, not just the requested layout. Every observed disk/backend path and the
ordered firmware paths must match the original state-owned launch configuration;
correct serial labels on substituted backing paths do not count as proof.

Every disk-only invocation excludes kernel/initramfs/ISO/USB/config media and
type-11 command-line reinjection, preserves firmware variables, uses strict UEFI
boot with disk boot indexes, and disables the management NIC option ROM without
a network boot index. The helper rejects PXE/fabric/IOMMU extra-NIC topology and
extra QEMU arguments. Wrong/duplicate serials, symlinks, caller backing paths,
unbounded records, missing required layouts, and stale process receipts fail.

## Fast checks without VM execution

```sh
go test -race ./pkg/provision/providers/qemu ./pkg/provision/providers/remote -run '^TestDisk' -count=1
go test -race -tags integration,integration_provision ./internal/integration/provision -run '^TestRAID' -count=1
go test -tags integration,integration_provision ./internal/integration -run '^$'
```

Keep failures as failures. In particular, a metadata-1.2 degraded assembly,
enforcing DebugService write, firmware boot, or resync failure is an implementation
or prerequisite finding, not permission to skip the phase or relax its assertions.
