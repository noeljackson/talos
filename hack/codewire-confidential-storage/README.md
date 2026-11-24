# Codewire Talos runtime helper publication

This deployment-only overlay publishes the two generic Talos helper images
owned by this fork: `imager` and `installer-base`. It deliberately does not
compose or publish a host installer. The reviewed Infra runtime-node recipe
remains the sole owner of combining these helpers with the exact Extensions
artifact and host profile, running the enforcing boot gate, and publishing the
final installer.

The active upstreamable source branch is
`downstream/confidential-storage-source-v1.14.7`, based on Talos v1.14.7. The
earlier source branches are kept as the provenance of installers built from
them (see "Source provenance" below). Neither contains a
publication workflow. A pull request into `downstream/confidential-storage`
runs only the source-free publication contract. Landing its exact head starts
one automatic `linux/amd64` build from that event commit and publishes:

```text
ghcr.io/noeljackson/imager:<pinned-release>-<revision-distance>-g<commit>
ghcr.io/noeljackson/installer-base:<pinned-release>-<revision-distance>-g<commit>
```

The exact upstream release and peeled release commit are pinned in
`runtime-identity.json`. The publisher requires that commit to be an ancestor
of the deployment revision, requires the matching Talos machinery version in
`go.mod`, and derives the immutable tag without depending on which Git tags a
fork happens to contain. Both images carry the full deployment commit in
`org.opencontainers.image.revision`, the same derived version in the Talos and
OCI version labels, BuildKit SBOM and provenance attestations, and a GitHub
deployment attestation. The workflow builds and verifies both OCI archives
before it writes either tag. Buildx and the BuildKit image index are pinned,
and the OCI exporter rewrites payload timestamps to the exact deployment
commit epoch. The build gate also opens the final imager layer and requires
every tar entry to carry that exact epoch, preventing an older cached layer
from surviving the export rewrite. BuildKit's embedded provenance
intentionally records each build invocation and can therefore change the
top-level index. A retry preserves an existing index only when that exact
platform manifest, two-descriptor image/attestation topology, and source
labels match; it records both the retained registry digest and rebuilt index
digest in the receipt. Any payload-manifest or imager timestamp difference
fails closed before either tag is written.

Infra consumes only the resulting digest-pinned references. Future source
maintenance follows the two-branch procedure documented in Codewire at
`docs/runbooks/confidential-runtime-fork-maintenance.md`; no third persistent
combination branch or manual helper publication is needed.

## Local archive qualification (no publication)

The current release identity is Talos `v1.14.7`, peeled upstream commit
`55021d7c274f6bc207bf18efa2a684a31a35cd9f`. The full checked-out deployment
commit determines the version suffix and source epoch; do not hardcode the
source-branch commit as the deployment identity. Compose the accepted source
with the current deployment head using the ordered two-parent procedure before
qualifying the exact deployment artifacts.

Run the synthetic contract first:

```sh
./hack/codewire-confidential-storage/test-publication-contract.sh
shellcheck hack/codewire-confidential-storage/*.sh
git diff --check
```

The contract uses only synthetic Git repositories, builder responses, and OCI
fixtures. It never executes a real builder or contacts a registry.

The archive-only commands share the publisher's exact `make target-*` recipe,
OCI topology, source-label, and imager timestamp inspection. They require an
already-running, single-node `docker-container` builder in the current Docker
context, Buildx `v0.36.1`, and the workflow's pinned BuildKit image:

```text
moby/buildkit@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8
```

The preflight checks both configured driver options and the actual running
container/image identity. It never creates, starts, pulls, or repairs a builder.
Select the intended existing builder with `BUILDX_BUILDER` if necessary. Use an
anonymous registry configuration for public inputs; local qualification does
not authorize reading registry credentials.

From a clean signed deployment checkout, with an existing `_out` parent:

```sh
./hack/codewire-confidential-storage/publish-runtime-helpers.sh local-build _out/helpers-cached
./hack/codewire-confidential-storage/publish-runtime-helpers.sh local-build --no-cache _out/helpers-cold
./hack/codewire-confidential-storage/publish-runtime-helpers.sh local-compare _out/helpers-cached _out/helpers-cold _out/helpers-comparison.json
```

Each build requires a **new** output directory, created with mode `0700` under
an existing parent. Paths must not traverse symlinks or contain spaces, shell
syntax, or exporter separators. Existing archives and receipts are never
replaced. Outputs inside the repository must be git-ignored and outside all
Docker source inputs. Tracked changes, untracked source, ignored files in Docker-admitted
source directories, and inherited Make/cache/output overrides fail admission.
The source is checked again after both archives are built. `--no-cache` reaches
both full helper targets, not just their final stages; none of the local commands
performs registry inspection, copying, login, or publication.

`local-build.json` binds the exact revision, epoch, version, builder pins, build
mode, and each archive's SHA-256, OCI index digest, and consumed AMD64 platform
manifest. `local-verify` re-inspects both archives, checks them against that
record and the current source identity, and creates a separate inspection
receipt. A mismatched build record or changed archive fails before receipt
creation. These receipts explicitly do **not** claim tests, reproducibility,
runtime qualification, or publication.

`local-compare` requires Python 3 and the cached/no-cache build records from the
same pinned builder and exact checked-out deployment identity. It re-inspects
both helper archives, streams and hashes every OCI blob, verifies descriptor
sizes and the complete image/attestation subject chain, and requires equal
consumed AMD64 platform manifests. It independently validates each SLSA v1
provenance record's source/revision, Dockerfile bytes/frontend, target, version,
source epoch, tool inputs, mode=max metadata, invocation/time fields, and cache
mode. The complete request trees, including nested root requests, and resolved
dependency digests must agree between legs, apart from the validated no-cache
option. Unexpected named build inputs are rejected by this fixed local-context
recipe. Both SPDX SBOMs must have the expected
structure and payload binding. Missing, malformed, tampered, foreign or replayed
evidence fails without creating the final comparison receipt.

The pinned BuildKit uses OCI artifact attestations: its enclosing manifest's
`subject` descriptor and the index annotation bind the payload, while in-toto
statements can legitimately have empty subject arrays. The comparator accepts
that combination only after verifying its complete descriptor/hash chain; any
explicit statement subjects must also match. It preserves both full provenance
and SBOM statements in the comparison receipt, alongside the independent
archive/index/platform/attestation digests. It never removes or rewrites
attestations. Different valid invocation IDs, timestamps, and cache metadata
are expected; neither OCI archive nor index equality is required.

These are unsigned local binding and payload-comparison checks, not authenticated
source-origin claims. BuildKit's local VCS hints are supplied by the build
client. The later signed deployment workflow, registry digest comparison, and
deployment-attestation verification remain mandatory. `cached` means cache was
allowed, not proof of a cache hit: use a populated builder and retain build logs
for the warm leg; the receipt does not claim a cache hit. Do not replace this
proof with `make reproducibility-test-docker-*`, which exports a different
archive format, or with self-consistent receipt metadata. Reuse valid source
policy test evidence; these checks are not an enforcing boot or RAID/SNP gate.

`preflight`, `build`, and `publish` retain their GitHub deployment-push guards.
Do not fake GitHub environment variables to make a local build look like an
authorized publisher. Only the checked-in deployment workflow publishes the
two helpers, and only Infra composes and qualifies the final host installer.

## Package set

The deployment head builds Talos v1.14.7 with its own upstream package set and
tools (`siderolabs/pkgs` `v1.14.0-66-ga1a4897`, `siderolabs/tools`
`v1.14.0-12-g5959f76`). The overlay overrides no package. That set carries
Linux 6.18.65, still with `siderolabs/pkgs` `ec119504d` ("fix: restore ACPI
power button", siderolabs/talos#14657), containerd 2.3.7, runc 1.5.2 and
libpathrs 0.2.6. Hosts using these helpers pair with the official Sidero
extensions from the same Talos v1.14.7 release; DRBD is `9.3.5-v1.14.7`, signed
by this kernel build.

Helpers from earlier deployment heads stay published and immutable:

- `5b15ba08c` and `d92d63d2f` carry the AMS/FRA encrypt-batch kernel overlay and
  remain valid only for the profiles that pin them. A future custom-kernel
  build must re-add that overlay deliberately and pass the artifact, enforcing
  direct-boot and staged-upgrade gates again.
- `1642d7a9e` (Talos v1.14.0 with a runc 1.5.2 pin) is superseded and must not
  be pinned.
- `3e7b1556b` (Talos v1.14.4, Linux 6.18.57) is superseded by this head and must
  not be pinned for new installers: 6.18.57 drops the ACPI fixed-event power
  button, so a power-button or IPMI soft-off no longer shuts Talos down.
- `bb3518b87` (Talos v1.14.5, Linux 6.18.58) stays valid for the installers that
  pin it.
- `faf5926ae` through `ab0020109` (Talos v1.14.6, Linux 6.18.63) stay valid for
  the installers that pin them; new installers are built from this head.

Both helper configurations record their kernel input as
`io.codewire.kernel.package`.

## Carried source patches

Each patch below is carried only because no stable upstream release has the
fix yet. Drop a patch as soon as a stable release contains it. Commit IDs are
those on `downstream/confidential-storage-source-v1.14.7`.

| Commit | Patch | Status |
| --- | --- | --- |
| `013df9b19` | fix(runtime): preserve enforcing extension and pod labels on Talos 1.14 | upstream-ready, carried until upstream has it |
| `1f6fb37f5` | test(selinux): bound the mdadm runtime map directory's policy | neverallow boundaries and fixtures for upstream's `421d1589d` |
| `19de16ee0` | fix(md): deliver live mdadm stderr monitor events | upstream-ready, carried until upstream has it |
| `32a3a85e6` | fix(sbom): bind complete license inputs to module checksums | upstream-ready, carried until upstream has it |
| `19971fd36` | test(storage): cover exact-serial mirror selection and rejoin | tests for the patches above |
| `180111ca4` | test(provision): prove exact-serial mirrored disk cold boots | tests for the patches above |
| `6d58acb40` | test(provision): run the RAID cold-boot proof on isolated Talos 1.14 fixtures | upstream-ready, test-only |
| `b4eb3aaca` | feat: allow the CRI containerd's blockfile snapshotter to be configured | upstream-ready, carried until upstream has it; not opened upstream (Codewire #3718) |
| `93b1f4e4d` | fix(selinux): run the kernel's static usermode helper in its own domain | carried; the domain and its rights were found by an enforcing proof run, not opened upstream |
| `e6bb719ec` | fix(selinux): give the usermode helper the rights a proof run used | as above |
| `dd3051bbc` | fix(selinux): let the usermode helper verify module signatures and reach machined | as above |
| `eabf48bec` | fix(modprobe): load modules without /etc/modprobe.conf, and let machined stat the helper | as above |
| `a79d4b8d5` | docs(selinux): say what guards the usermode helper's unlogged /etc denial | as above |
| `f389c2a78` | fix(machined): refuse to start when SELinux is on and the domain is unreadable | as above; superseded in part by the next |
| `209108f63` | fix(machined): decide the usermode helper gate from the label read's errno | as above |
| `00103e457` | fix(cri): create a missing blockfile scratch image, and start without blockfile when it can't be made | carried with `b4eb3aaca`, not opened upstream |
| `7e5ccd4f1` | fix(cri): drop the image platforms on blockfile from the fallback configuration | as above |
| `c2af31490` | fix(cri): label the blockfile scratch image's root through mkfs, not debugfs | as above |
| `c4e4f12f1` | test(cri): a blockfile scratch creation that fails partway leaves nothing behind, and pin its size | tests for the patches above |
| `2284e09ff` | fix(selinux): keep blockfile mounts' loop worker scheduling denial out of the log | carried with `b4eb3aaca` |
| `26ec1e20a` | fix(cri): refuse a writable directory on the scratch path, and a blockfile default | carried with `b4eb3aaca` |
| `41ed730cd` | fix(selinux): label the usermode helper's device nodes before udev does | carried with `93b1f4e4d`, not opened upstream |
| `c10c5608c` | docs(usermodehelper): keep Names' comment with Names, and say where Label comes from | as above |
| `fd4f4a0cb` | fix(selinux): let Falco list the usermode helper's fd directory | as above |
| `6f4539b7b` | fix(selinux): let Falco open the usermode helper's fd directory too | as above |
| `a978b6054` | fix(lvm): keep the host's LVM off DRBD, loop and LINSTOR crypt devices | carried, not opened upstream |

Upstream's `421d1589d` ("give the mdadm runtime map directory a dedicated SELinux
type"), carried on the v1.14.5 stack as `98a4862c5`, is part of v1.14.6; only its
policy boundaries and fixtures are still carried, as `1f6fb37f5`. Upstream's
`449f82d46` ("wait for inactive MD arrays before volume discovery"), carried on
the v1.14.4 stack, is part of v1.14.5 and is no longer carried.

## Source provenance

Source branches are append-only: later changes are new commits on top, never
a rewrite.

| Deployment head | Talos | Source commit (second parent) | Source branch |
| --- | --- | --- | --- |
| this head | v1.14.7 | `a978b6054` | `downstream/confidential-storage-source-v1.14.7` |
| `ab0020109` | v1.14.6 | `27d8a3553` | `downstream/confidential-storage-source-v1.14.6` |
| `954558567` | v1.14.6 | `09e3aa3e0` | `downstream/confidential-storage-source-v1.14.6` |
| `6dd8c6620` | v1.14.6 | `e6a851e7f` | `downstream/confidential-storage-source-v1.14.6` |
| `2acb9ebd0` | v1.14.6 | `40af88ad9` | `downstream/confidential-storage-source-v1.14.6` |
| `2507839e1` | v1.14.6 | `97a2a03a6` | `downstream/confidential-storage-source-v1.14.6` |
| `faf5926ae` | v1.14.6 | `77399fa6e` | `downstream/confidential-storage-source-v1.14.6` |
| `bb3518b87` | v1.14.5 | `b4af7a731` | `downstream/confidential-storage-source-v1.14.5` |
| `eb09e50a7` | v1.14.5 | `54f4012af` | `downstream/confidential-storage-source-v1.14.5` |
| `3e7b1556b` | v1.14.4 | `f37d59689` | see below |
| `1642d7a9e` and earlier | v1.14.0 | | `downstream/confidential-storage-source` |

`f37d59689` is the source of the published `3e7b1556b` helpers and stays
reachable through the deployment branch as `3e7b1556b`'s second parent. On
2026-10-26 `downstream/confidential-storage-source-v1.14.4` was force-pushed
twice, `f37d59689` to `17b914ade` and then to `90dbd051a`, folding test-only
RAID harness changes into its RAID proof commit (the runtime tree is
unchanged), so that branch's head no longer contains `f37d59689`. Those
rewrites broke the append-only rule; they are recorded here and not repeated.

## CRI blockfile snapshotter

`77399fa6e` leaves `io.containerd.snapshotter.v1.blockfile` out of the CRI
containerd's `disabled_plugins` (the system containerd keeps it disabled).
containerd appends imported `disabled_plugins`, so no `CRICustomizationConfig`
could enable it before. Unconfigured, the plugin creates its empty root
directory and skips itself ("no scratch file generator"); a node uses it only
when its machine configuration gives it a `scratch_file`, as Codewire's
`kata-clh` block root filesystem does.

A configured `scratch_file` that is missing does not skip the plugin: every
plugin behind it fails to load and the whole CRI containerd is down. Since
`dc77eda79` machined creates a missing image before the CRI containerd starts
(a 12 GiB ext4 image whose root carries containerd's state label, under
`/var/lib/` only, through no symlink or writable directory). When it cannot,
the CRI containerd starts without blockfile and without the runtimes and image
platforms on it, so their workloads are refused rather than run on another
snapshotter; a blockfile default runtime or snapshotter is refused instead.
Both outcomes are logged to the kernel log as `blockfile: …`.

## CRI unpack sync

`sync_fs` for the CRI containerd diff service is no longer a fork change. It
is delivered to every bare-metal node as a `CRICustomizationConfig` machine
configuration document, so unpacked image layers reach disk before containerd
commits their snapshots.
