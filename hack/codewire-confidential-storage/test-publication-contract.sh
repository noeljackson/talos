#!/usr/bin/env bash
# shellcheck disable=SC2016 # Contract literals intentionally contain workflow expressions.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
workflow="${repo_root}/.github/workflows/downstream-confidential-storage.yml"
publisher="${script_dir}/publish-runtime-helpers.sh"
test_root="$(mktemp -d)"

cleanup() {
	find "${test_root}" -xdev -type f -delete 2>/dev/null || true
	rmdir -- "${test_root}" 2>/dev/null || true
}
trap cleanup EXIT

require_line() {
	local file=$1 line=$2 description=$3
	grep -Fqx -- "${line}" "${file}" || {
		printf 'missing %s in %s\n' "${description}" "${file}" >&2
		return 1
	}
}

require_text() {
	local file=$1 value=$2 description=$3
	grep -Fq -- "${value}" "${file}" || {
		printf 'missing %s in %s\n' "${description}" "${file}" >&2
		return 1
	}
}

verify_workflow() {
	local candidate=$1
	[[ "$(grep -Fxc '      - downstream/confidential-storage' "${candidate}")" -eq 2 ]] || {
		printf 'workflow must select the deployment branch once for PRs and once for pushes\n' >&2
		return 1
	}
	! grep -Fq 'downstream/confidential-storage-source' "${candidate}" || {
		printf 'upstreamable source branch must never publish\n' >&2
		return 1
	}
	! grep -Eq 'workflow_dispatch:|^[[:space:]]+release:|^[[:space:]]+tags:' "${candidate}" || {
		printf 'workflow must not expose manual, release, or tag publication\n' >&2
		return 1
	}
	require_line "${candidate}" \
		"    if: github.event_name == 'push' && github.ref == 'refs/heads/downstream/confidential-storage' && github.repository == 'noeljackson/talos'" \
		'exact publication job guard'
	require_line "${candidate}" \
		'          ref: ${{ github.event.pull_request.head.sha }}' \
		'exact pull-request head checkout'
	require_line "${candidate}" \
		'          ref: ${{ github.sha }}' \
		'exact push commit checkout'
	require_line "${candidate}" \
		'        run: ./hack/codewire-confidential-storage/publish-runtime-helpers.sh preflight' \
		'pre-build publication preflight'
	require_text "${candidate}" \
		'build "${OUTPUT_ROOT}"' \
		'archive build step'
	require_text "${candidate}" \
		'publish "${OUTPUT_ROOT}" "${OUTPUT_ROOT}/publication.json"' \
		'separate publication step'
	require_line "${candidate}" '      packages: write' 'registry write permission'
	require_line "${candidate}" '      id-token: write' 'OIDC attestation permission'
	require_line "${candidate}" '      attestations: write' 'attestation permission'
}

verify_publisher() {
	require_line "${publisher}" 'registry_root="ghcr.io/noeljackson"' 'fixed destination owner'
	require_line "${publisher}" 'components=(installer-base imager)' 'complete helper set'
	require_text "${publisher}" 'PLATFORM=linux/amd64' 'single Dev architecture'
	require_text "${publisher}" 'INSTALLER_ARCH=targetarch' 'target-only Talos assets'
	require_text "${publisher}" '"SHA=${revision}"' 'full source revision input'
	require_text "${publisher}" '"TAG=${version}"' 'commit-derived version input'
	require_text "${publisher}" '--provenance=mode=max --sbom=true' 'embedded supply-chain attestations'
	require_text "${publisher}" 'Inspect every local archive and every existing tag before writing either tag.' \
		'build-both publication ordering'
	require_text "${publisher}" 'immutable ${component} tag already exists with a different digest' \
		'immutable collision rejection'
	require_text "${publisher}" 'skopeo copy --all --format oci' 'all-manifest registry copy'
	! grep -Fq 'docker login' "${publisher}" || {
		printf 'publisher must consume workflow-provided registry authentication\n' >&2
		return 1
	}
}

verify_workflow "${workflow}"
verify_publisher

sed 's/downstream\/confidential-storage/main/g' "${workflow}" >"${test_root}/candidate.yml"
if verify_workflow "${test_root}/candidate.yml" >/dev/null 2>&1; then
	printf 'main-only publication fixture unexpectedly satisfied the contract\n' >&2
	exit 1
fi

sed '/      - downstream\/confidential-storage$/a\      - downstream/confidential-storage-source' \
	"${workflow}" >"${test_root}/candidate.yml"
if verify_workflow "${test_root}/candidate.yml" >/dev/null 2>&1; then
	printf 'source-branch publication fixture unexpectedly satisfied the contract\n' >&2
	exit 1
fi

if env \
	GITHUB_ACTIONS=true \
	GITHUB_EVENT_NAME=pull_request \
	GITHUB_REF=refs/heads/downstream/confidential-storage \
	GITHUB_REPOSITORY=noeljackson/talos \
	GITHUB_SHA="$(git -C "${repo_root}" rev-parse HEAD)" \
	"${publisher}" preflight 2>"${test_root}/guard.stderr"; then
	printf 'non-push publication context unexpectedly passed preflight\n' >&2
	exit 1
fi
grep -Fq 'publication requires a push event' "${test_root}/guard.stderr"

printf 'downstream Talos runtime helper publication contract: PASS\n'
