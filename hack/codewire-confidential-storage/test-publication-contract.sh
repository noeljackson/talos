#!/usr/bin/env bash
# shellcheck disable=SC2016 # Contract literals intentionally contain workflow expressions.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
workflow="${repo_root}/.github/workflows/downstream-confidential-storage.yml"
publisher="${script_dir}/publish-runtime-helpers.sh"
identity="${script_dir}/runtime-identity.json"
mkdir -p "${repo_root}/_out"
test_root="$(mktemp -d "${repo_root}/_out/publication-contract-test.XXXXXX")"

cleanup() {
	find "${test_root}" -xdev -depth -delete 2>/dev/null || true
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
	require_line "${publisher}" 'identity_file="${script_dir}/runtime-identity.json"' 'pinned identity input'
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
	require_text "${publisher}" 'sourceIdentity:{upstreamRepository:$upstream_repository' \
		'pinned source identity receipt'
	! grep -Fq 'git -C "${repo_root}" describe' "${publisher}" || {
		printf 'publisher must not derive versions from fork-local Git tags\n' >&2
		return 1
	}
	! grep -Fq 'docker login' "${publisher}" || {
		printf 'publisher must consume workflow-provided registry authentication\n' >&2
		return 1
	}
}

verify_identity() {
	jq -e '
		type == "object" and
		keys == ["commitAbbreviationLength", "release", "releaseCommit", "schema", "upstreamRepository"] and
		.schema == "codewire.talos-runtime-source-identity/v1" and
		.upstreamRepository == "https://github.com/siderolabs/talos" and
		.release == "v1.13.10" and
		.releaseCommit == "00ca4c9870786e57690dda23b877025d22256953" and
		.commitAbbreviationLength == 9
	' "${identity}" >/dev/null
}

run_fixture_preflight() {
	local fixture_repo=$1 fixture_head
	fixture_head="$(git -C "${fixture_repo}" rev-parse HEAD)"
	env \
		GITHUB_ACTIONS=true \
		GITHUB_EVENT_NAME=push \
		GITHUB_REF=refs/heads/downstream/confidential-storage \
		GITHUB_REPOSITORY=noeljackson/talos \
		GITHUB_SHA="${fixture_head}" \
		"${fixture_repo}/hack/codewire-confidential-storage/publish-runtime-helpers.sh" preflight
}

verify_tag_independent_version() {
	local fixture_repo fixture_release fixture_head fixture_abbreviation
	local drift_repo drift_head unrelated_repo unrelated_head unrelated_commit empty_tree

	fixture_repo="${test_root}/version-fixture"
	mkdir -p "${fixture_repo}"
	git -C "${fixture_repo}" init -q --initial-branch=main
	git -C "${fixture_repo}" config user.name 'Codewire Contract Test'
	git -C "${fixture_repo}" config user.email 'codewire-contract@example.invalid'
	cat >"${fixture_repo}/go.mod" <<'EOF'
module github.com/siderolabs/talos

go 1.25.0

require github.com/siderolabs/talos/pkg/machinery v1.13.10
EOF
	git -C "${fixture_repo}" add go.mod
	git -C "${fixture_repo}" commit -q -m 'fixture: upstream release'
	fixture_release="$(git -C "${fixture_repo}" rev-parse HEAD)"

	mkdir -p "${fixture_repo}/hack/codewire-confidential-storage"
	cp "${publisher}" "${fixture_repo}/hack/codewire-confidential-storage/publish-runtime-helpers.sh"
	jq -n --arg release_commit "${fixture_release}" '{
		schema: "codewire.talos-runtime-source-identity/v1",
		upstreamRepository: "https://github.com/siderolabs/talos",
		release: "v1.13.10",
		releaseCommit: $release_commit,
		commitAbbreviationLength: 9
	}' >"${fixture_repo}/hack/codewire-confidential-storage/runtime-identity.json"
	git -C "${fixture_repo}" add hack
	git -C "${fixture_repo}" commit -q -m 'fixture: deployment overlay'
	if [[ -n "$(git -C "${fixture_repo}" tag --list)" ]]; then
		printf 'tag-independent fixture unexpectedly contains a Git tag\n' >&2
		exit 1
	fi
	fixture_head="$(git -C "${fixture_repo}" rev-parse HEAD)"
	fixture_abbreviation=${fixture_head:0:9}
	run_fixture_preflight "${fixture_repo}" >"${test_root}/valid-preflight.stdout"
	grep -Fqx \
		"authorized immutable linux/amd64 helper build: version=v1.13.10-1-g${fixture_abbreviation} revision=${fixture_head}" \
		"${test_root}/valid-preflight.stdout"

	drift_repo="${test_root}/release-drift"
	cp -a "${fixture_repo}" "${drift_repo}"
	sed -i 's/v1[.]13[.]10/v1.13.11/' "${drift_repo}/go.mod"
	git -C "${drift_repo}" add go.mod
	git -C "${drift_repo}" commit -q -m 'fixture: drift machinery version'
	drift_head="$(git -C "${drift_repo}" rev-parse HEAD)"
	if run_fixture_preflight "${drift_repo}" >"${test_root}/drift.stdout" 2>"${test_root}/drift.stderr"; then
		printf 'mismatched machinery release unexpectedly passed preflight\n' >&2
		exit 1
	fi
	grep -Fq 'pinned Talos release does not match the machinery module version' \
		"${test_root}/drift.stderr"
	[[ "${drift_head}" != "${fixture_head}" ]]

	unrelated_repo="${test_root}/unrelated-release"
	cp -a "${fixture_repo}" "${unrelated_repo}"
	empty_tree="$(git -C "${unrelated_repo}" mktree </dev/null)"
	unrelated_commit="$(printf '%s\n' 'fixture: unrelated release' | git -C "${unrelated_repo}" commit-tree "${empty_tree}")"
	jq --arg release_commit "${unrelated_commit}" \
		'.releaseCommit = $release_commit' \
		"${unrelated_repo}/hack/codewire-confidential-storage/runtime-identity.json" \
		>"${unrelated_repo}/hack/codewire-confidential-storage/runtime-identity.json.next"
	mv "${unrelated_repo}/hack/codewire-confidential-storage/runtime-identity.json.next" \
		"${unrelated_repo}/hack/codewire-confidential-storage/runtime-identity.json"
	git -C "${unrelated_repo}" add hack/codewire-confidential-storage/runtime-identity.json
	git -C "${unrelated_repo}" commit -q -m 'fixture: select unrelated release commit'
	unrelated_head="$(git -C "${unrelated_repo}" rev-parse HEAD)"
	if run_fixture_preflight "${unrelated_repo}" >"${test_root}/unrelated.stdout" 2>"${test_root}/unrelated.stderr"; then
		printf 'unrelated release commit unexpectedly passed preflight\n' >&2
		exit 1
	fi
	grep -Fq 'pinned Talos release commit is not an ancestor of the deployment revision' \
		"${test_root}/unrelated.stderr"
	[[ "${unrelated_head}" != "${fixture_head}" ]]
}

verify_workflow "${workflow}"
verify_publisher
verify_identity
verify_tag_independent_version

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
