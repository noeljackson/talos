# Codewire Talos runtime helper publication

This deployment-only overlay publishes the two generic Talos helper images
owned by this fork: `imager` and `installer-base`. It deliberately does not
compose or publish a host installer. The reviewed Infra runtime-node recipe
remains the sole owner of combining these helpers with the exact Extensions
artifact and host profile, running the enforcing boot gate, and publishing the
final installer.

The upstreamable `downstream/confidential-storage-source` branch contains no
publication workflow. A pull request into `downstream/confidential-storage`
runs only the source-free publication contract. Landing its exact head starts
one automatic `linux/amd64` build from that event commit and publishes:

```text
ghcr.io/noeljackson/imager:<git-describe-version>
ghcr.io/noeljackson/installer-base:<git-describe-version>
```

The tag is immutable and commit-derived. Both images carry the full deployment
commit in `org.opencontainers.image.revision`, the same derived version in the
Talos and OCI version labels, BuildKit SBOM and provenance attestations, and a
GitHub deployment attestation. The workflow builds and verifies both OCI
archives before it writes either tag. A retry may reuse an identical digest
after a partial registry copy, but it rejects every existing tag whose digest
differs.

Infra consumes only the resulting digest-pinned references. Future source
maintenance follows the two-branch procedure documented in Codewire at
`docs/runbooks/confidential-runtime-fork-maintenance.md`; no third persistent
combination branch or manual helper publication is needed.
