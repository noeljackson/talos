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
	require_line "${candidate}" '          version: v0.36.1' 'pinned Buildx version'
	require_line "${candidate}" \
		'            image=moby/buildkit@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8' \
		'pinned BuildKit image index'
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
	require_text "${publisher}" 'rewrite-timestamp=true' 'repeatable OCI export timestamps'
	require_text "${publisher}" '--provenance=mode=max --sbom=true' 'embedded supply-chain attestations'
	require_text "${publisher}" 'Inspect every local archive and every existing tag before writing either tag.' \
		'build-both publication ordering'
	require_text "${publisher}" 'immutable ${component} tag already exists with a different platform manifest' \
		'immutable payload collision rejection'
	require_text "${publisher}" 'buildIndexDigest:$imager_build_index_digest' \
		'rebuilt index receipt'
	require_text "${publisher}" 'verify_imager_archive_timestamps' \
		'imager layer timestamp verification'
	require_text "${publisher}" 'entry timestamps that do not equal source epoch' \
		'imager timestamp mismatch rejection'
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
		.release == "v1.14.0" and
		.releaseCommit == "9abd05af449ebf9cb1827648298291afce18d714" and
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

write_imager_archive_fixture() {
	local archive=$1 platform=$2 source_epoch=$3 entry_epoch=$4 fixture_name=$5
	local layout="${test_root}/imager-archive-${fixture_name}"
	local rootfs="${layout}/rootfs"
	local layer_digest='sha256:3333333333333333333333333333333333333333333333333333333333333333'
	mkdir -p "${layout}/blobs/sha256" "${rootfs}/etc/modules.d"
	printf 'fixture modules\n' >"${rootfs}/etc/modules.d/10-extra-modules.conf"
	tar --sort=name --format=posix --numeric-owner --owner=0 --group=0 \
		--mtime="@${entry_epoch}" -czf \
		"${layout}/blobs/sha256/${layer_digest#sha256:}" -C "${rootfs}" .
	jq -n \
		--arg layer_digest "${layer_digest}" \
		--arg source_epoch "${source_epoch}" '{
		schemaVersion:2,
		mediaType:"application/vnd.oci.image.manifest.v1+json",
		config:{mediaType:"application/vnd.oci.image.config.v1+json",
		        digest:"sha256:4444444444444444444444444444444444444444444444444444444444444444",
		        size:2},
		layers:[{mediaType:"application/vnd.oci.image.layer.v1.tar+gzip",
		         digest:$layer_digest, size:1,
		         annotations:{"buildkit/rewritten-timestamp":$source_epoch}}]
	}' >"${layout}/blobs/sha256/${platform#sha256:}"
	tar -cf "${archive}" -C "${layout}" blobs
}

verify_tag_independent_version() {
	local fixture_repo fixture_release fixture_head fixture_abbreviation
	local drift_repo drift_head unrelated_repo unrelated_head unrelated_commit empty_tree

	fixture_repo="${test_root}/version-fixture"
	mkdir -p "${fixture_repo}"
	git -C "${fixture_repo}" init -q --initial-branch=main
	git -C "${fixture_repo}" config commit.gpgsign false
	git -C "${fixture_repo}" config user.name 'Codewire Contract Test'
	git -C "${fixture_repo}" config user.email 'codewire-contract@example.invalid'
	cat >"${fixture_repo}/go.mod" <<'EOF'
module github.com/siderolabs/talos

go 1.26.5

require github.com/siderolabs/talos/pkg/machinery v1.14.0
EOF
	git -C "${fixture_repo}" add go.mod
	git -C "${fixture_repo}" commit -q -m 'fixture: upstream release'
	fixture_release="$(git -C "${fixture_repo}" rev-parse HEAD)"

	mkdir -p "${fixture_repo}/hack/codewire-confidential-storage"
	cp "${publisher}" "${fixture_repo}/hack/codewire-confidential-storage/publish-runtime-helpers.sh"
	jq -n --arg release_commit "${fixture_release}" '{
		schema: "codewire.talos-runtime-source-identity/v1",
		upstreamRepository: "https://github.com/siderolabs/talos",
		release: "v1.14.0",
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
		"authorized immutable linux/amd64 helper build: version=v1.14.0-1-g${fixture_abbreviation} revision=${fixture_head}" \
		"${test_root}/valid-preflight.stdout"

	drift_repo="${test_root}/release-drift"
	cp -a "${fixture_repo}" "${drift_repo}"
	sed -i 's/v1[.]14[.]0/v1.14.1/' "${drift_repo}/go.mod"
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

verify_existing_index_retry() {
	local retry_repo retry_bin retry_release retry_head retry_abbreviation retry_version
	local retry_epoch output_dir bad_output receipt copy_log mismatch_log timestamp_log
	local installer_platform imager_platform

	retry_repo="${test_root}/retry-fixture"
	retry_bin="${test_root}/retry-bin"
	output_dir="${retry_repo}/_out/retry"
	receipt="${output_dir}/publication.json"
	copy_log="${test_root}/retry-copy.log"
	mismatch_log="${test_root}/retry-mismatch.stderr"
	installer_platform='sha256:1111111111111111111111111111111111111111111111111111111111111111'
	imager_platform='sha256:2222222222222222222222222222222222222222222222222222222222222222'

	mkdir -p "${retry_repo}" "${retry_bin}"
	git -C "${retry_repo}" init -q --initial-branch=main
	git -C "${retry_repo}" config commit.gpgsign false
	git -C "${retry_repo}" config user.name 'Codewire Retry Contract Test'
	git -C "${retry_repo}" config user.email 'codewire-retry-contract@example.invalid'
	cat >"${retry_repo}/go.mod" <<'EOF'
module github.com/siderolabs/talos

go 1.26.5

require github.com/siderolabs/talos/pkg/machinery v1.14.0
EOF
	git -C "${retry_repo}" add go.mod
	git -C "${retry_repo}" commit -q -m 'fixture: upstream release'
	retry_release="$(git -C "${retry_repo}" rev-parse HEAD)"

	mkdir -p "${retry_repo}/hack/codewire-confidential-storage"
	cp "${publisher}" "${retry_repo}/hack/codewire-confidential-storage/publish-runtime-helpers.sh"
	jq -n --arg release_commit "${retry_release}" '{
		schema: "codewire.talos-runtime-source-identity/v1",
		upstreamRepository: "https://github.com/siderolabs/talos",
		release: "v1.14.0",
		releaseCommit: $release_commit,
		commitAbbreviationLength: 9
	}' >"${retry_repo}/hack/codewire-confidential-storage/runtime-identity.json"
	git -C "${retry_repo}" add hack
	git -C "${retry_repo}" commit -q -m 'fixture: deployment overlay'
	retry_head="$(git -C "${retry_repo}" rev-parse HEAD)"
	retry_epoch="$(git -C "${retry_repo}" show -s --format=%ct HEAD)"
	retry_abbreviation=${retry_head:0:9}
	retry_version="v1.14.0-1-g${retry_abbreviation}"

	mkdir -p "${output_dir}"
	printf 'installer-base archive fixture\n' >"${output_dir}/installer-base.oci.tar"
	write_imager_archive_fixture \
		"${output_dir}/imager.oci.tar" "${imager_platform}" \
		"${retry_epoch}" "${retry_epoch}" valid
	: >"${copy_log}"

	cat >"${retry_bin}/skopeo" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [[ -n "${TEST_SKOPEO_LOG:-}" ]]; then printf '%s\n' "$*" >>"${TEST_SKOPEO_LOG}"; fi

if [[ "$1" == copy ]]; then
	printf '%s\n' "$*" >>"${TEST_COPY_LOG}"
	exit 0
fi

[[ "$1" == inspect ]] || exit 2
source_ref=${!#}
if [[ "${TEST_LOCAL_ONLY:-}" == 1 && "${source_ref}" != oci-archive:* ]]; then exit 95; fi
case "${source_ref}" in
	*installer-base*)
		component=installer-base
		platform='sha256:1111111111111111111111111111111111111111111111111111111111111111'
		local_attestation='sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
		registry_attestation='sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
		;;
	*imager*)
		component=imager
		platform='sha256:2222222222222222222222222222222222222222222222222222222222222222'
		local_attestation='sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc'
		registry_attestation='sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'
		;;
	*) exit 2 ;;
esac

if [[ " $* " == *' --config '* ]]; then
	jq -n \
		--arg revision "${TEST_REVISION}" \
		--arg version "${TEST_VERSION}" \
		'{architecture:"amd64", os:"linux", config:{Labels:{
		  "org.opencontainers.image.source":"https://github.com/noeljackson/talos",
		  "org.opencontainers.image.revision":$revision,
		  "org.opencontainers.image.version":$version,
		  "alpha.talos.dev/version":$version
		}}}'
	exit 0
fi

[[ " $* " == *' --raw '* ]] || exit 2
attestation=${local_attestation}
if [[ "${source_ref}" == docker://* ]]; then
	attestation=${registry_attestation}
	if [[ "${TEST_RETRY_MISMATCH:-}" == "${component}" ]]; then
		platform='sha256:9999999999999999999999999999999999999999999999999999999999999999'
	fi
fi
jq -n --arg platform "${platform}" --arg attestation "${attestation}" '{
	mediaType:"application/vnd.oci.image.index.v1+json",
	manifests:[
	  {mediaType:"application/vnd.oci.image.manifest.v1+json", digest:$platform,
	   platform:{architecture:"amd64", os:"linux"}},
	  {mediaType:"application/vnd.oci.image.manifest.v1+json", digest:$attestation,
	   platform:{architecture:"unknown", os:"unknown"}, annotations:{
	     "vnd.docker.reference.type":"attestation-manifest",
	     "vnd.docker.reference.digest":$platform
	   }}
	]
}'
EOF
	chmod +x "${retry_bin}/skopeo"

	env \
		PATH="${retry_bin}:${PATH}" \
		TEST_COPY_LOG="${copy_log}" \
		TEST_REVISION="${retry_head}" \
		TEST_VERSION="${retry_version}" \
		GITHUB_ACTIONS=true \
		GITHUB_EVENT_NAME=push \
		GITHUB_REF=refs/heads/downstream/confidential-storage \
		GITHUB_REPOSITORY=noeljackson/talos \
		GITHUB_SHA="${retry_head}" \
		GITHUB_OUTPUT="${test_root}/retry-github-output" \
		"${retry_repo}/hack/codewire-confidential-storage/publish-runtime-helpers.sh" \
		publish "${output_dir}" "${receipt}"

	[[ ! -s "${copy_log}" ]]
	jq -e \
		--arg installer_platform "${installer_platform}" \
		--arg imager_platform "${imager_platform}" '
		.schema == "codewire.talos-runtime-helpers.publication/v1" and
		.images["installer-base"].state == "reused" and
		.images.imager.state == "reused" and
		.images["installer-base"].platformManifest == $installer_platform and
		.images.imager.platformManifest == $imager_platform and
		.images["installer-base"].digest != .images["installer-base"].buildIndexDigest and
		.images.imager.digest != .images.imager.buildIndexDigest
	' "${receipt}" >/dev/null

	if env \
		PATH="${retry_bin}:${PATH}" \
		TEST_COPY_LOG="${copy_log}" \
		TEST_RETRY_MISMATCH=installer-base \
		TEST_REVISION="${retry_head}" \
		TEST_VERSION="${retry_version}" \
		GITHUB_ACTIONS=true \
		GITHUB_EVENT_NAME=push \
		GITHUB_REF=refs/heads/downstream/confidential-storage \
		GITHUB_REPOSITORY=noeljackson/talos \
		GITHUB_SHA="${retry_head}" \
		GITHUB_OUTPUT="${test_root}/mismatch-github-output" \
		"${retry_repo}/hack/codewire-confidential-storage/publish-runtime-helpers.sh" \
		publish "${output_dir}" "${output_dir}/mismatch-publication.json" \
		>"${test_root}/retry-mismatch.stdout" 2>"${mismatch_log}"; then
		printf 'mismatched immutable payload unexpectedly passed retry admission\n' >&2
		exit 1
	fi
	grep -Fq 'immutable installer-base tag already exists with a different platform manifest' \
		"${mismatch_log}"
	[[ ! -s "${copy_log}" ]]

	bad_output="${retry_repo}/_out/retry-bad-timestamp"
	timestamp_log="${test_root}/retry-timestamp.stderr"
	mkdir -p "${bad_output}"
	cp "${output_dir}/installer-base.oci.tar" "${bad_output}/installer-base.oci.tar"
	write_imager_archive_fixture \
		"${bad_output}/imager.oci.tar" "${imager_platform}" \
		"${retry_epoch}" "$((retry_epoch - 1))" stale
	: >"${copy_log}"
	if env \
		PATH="${retry_bin}:${PATH}" \
		TEST_COPY_LOG="${copy_log}" \
		TEST_REVISION="${retry_head}" \
		TEST_VERSION="${retry_version}" \
		GITHUB_ACTIONS=true \
		GITHUB_EVENT_NAME=push \
		GITHUB_REF=refs/heads/downstream/confidential-storage \
		GITHUB_REPOSITORY=noeljackson/talos \
		GITHUB_SHA="${retry_head}" \
		GITHUB_OUTPUT="${test_root}/timestamp-github-output" \
		"${retry_repo}/hack/codewire-confidential-storage/publish-runtime-helpers.sh" \
		publish "${bad_output}" "${bad_output}/publication.json" \
		>"${test_root}/retry-timestamp.stdout" 2>"${timestamp_log}"; then
		printf 'stale imager layer timestamp unexpectedly passed retry admission\n' >&2
		exit 1
	fi
	grep -Fq 'entry timestamps that do not equal source epoch' "${timestamp_log}"
	[[ ! -s "${copy_log}" ]]
}

verify_local_archives() {
	local fixture_repo="${test_root}/local-fixture" fixture_bin="${test_root}/local-bin"
	local fixture_archives="${test_root}/local-archives" fixture_release fixture_head fixture_epoch fixture_version
	local fixture_publisher make_log="${test_root}/local-make.log" skopeo_log="${test_root}/local-skopeo.log"
	local cases=0
	mkdir -p "${fixture_repo}/cmd" "${fixture_repo}/selinux" "${fixture_bin}" "${fixture_archives}"
	git -C "${fixture_repo}" init -q --initial-branch=main
	git -C "${fixture_repo}" config commit.gpgsign false
	git -C "${fixture_repo}" config user.name 'Codewire Local Contract Test'
	git -C "${fixture_repo}" config user.email 'codewire-local@example.invalid'
	printf 'module fixture\nrequire github.com/siderolabs/talos/pkg/machinery v1.14.0\n' >"${fixture_repo}/go.mod"
	printf '_out/\n*.ignored\n' >"${fixture_repo}/.gitignore"
	cp "${repo_root}/Makefile" "${fixture_repo}/Makefile"
	cp "${repo_root}/Dockerfile" "${fixture_repo}/Dockerfile"
	git -C "${fixture_repo}" add go.mod .gitignore Makefile Dockerfile
	git -C "${fixture_repo}" commit -qm 'fixture: release'
	fixture_release="$(git -C "${fixture_repo}" rev-parse HEAD)"
	mkdir -p "${fixture_repo}/hack/codewire-confidential-storage" "${fixture_repo}/_out"
	fixture_publisher="${fixture_repo}/hack/codewire-confidential-storage/publish-runtime-helpers.sh"
	cp "${publisher}" "${fixture_publisher}"
	jq --arg release_commit "${fixture_release}" '.releaseCommit = $release_commit' \
		"${identity}" >"${fixture_repo}/hack/codewire-confidential-storage/runtime-identity.json"
	git -C "${fixture_repo}" add hack
	git -C "${fixture_repo}" commit -qm 'fixture: local overlay'
	fixture_head="$(git -C "${fixture_repo}" rev-parse HEAD)"
	fixture_epoch="$(git -C "${fixture_repo}" show -s --format=%ct HEAD)"
	fixture_version="v1.14.0-1-g${fixture_head:0:9}"
	printf 'synthetic installer-base\n' >"${fixture_archives}/installer-base.oci.tar"
	write_imager_archive_fixture "${fixture_archives}/imager.oci.tar" \
		sha256:2222222222222222222222222222222222222222222222222222222222222222 \
		"${fixture_epoch}" "${fixture_epoch}" local
	cp "${test_root}/retry-bin/skopeo" "${fixture_bin}/skopeo"
	cat >"${fixture_bin}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
image=moby/buildkit@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8
case "$*" in
 'buildx version') printf 'github.com/docker/buildx %s fixture\n' "${TEST_BUILDX_VERSION:-v0.36.1}" ;;
 'buildx inspect --timeout 20s')
  printf 'Name: fixture\nDriver: docker-container\n\nNodes:\nName: fixture0\nEndpoint: default\nDriver Options: image="%s"\nStatus: running\n' "${TEST_BUILDKIT_IMAGE:-$image}"
  ;;
 'inspect --type container buildx_buildkit_fixture0')
  jq -n --arg image "$image" --arg id "${TEST_RUNNING_IMAGE_ID:-sha256:fixture}" \
   '[{State:{Running:true}, Config:{Image:$image}, Image:$id}]'
  ;;
 "image inspect $image --format {{.Id}}") printf 'sha256:fixture\n' ;;
 *) printf 'unexpected docker call: %s\n' "$*" >&2; exit 95 ;;
esac
EOF
	cat >"${fixture_bin}/make" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${TEST_MAKE_LOG}"
[[ "$*" == *'PUSH=false CI_ARGS= PLATFORM=linux/amd64 INSTALLER_ARCH=targetarch'* ]]
[[ "$*" == *'BUILD=docker buildx build --builder fixture'* ]]
[[ "$*" == *"SHA=${TEST_REVISION}"* && "$*" == *"TAG=${TEST_VERSION}"* ]]
[[ "$*" == *'ABBREV_TAG=v1.14.0'* ]]
component='' target_args=''
for arg in "$@"; do
 case "$arg" in target-installer-base|target-imager) component=${arg#target-} ;; TARGET_ARGS=*) target_args=${arg#TARGET_ARGS=} ;; esac
done
[[ -n "$component" && "$target_args" == *'--provenance=mode=max --sbom=true'* ]]
if [[ "${TEST_EXPECT_MODE:-cached}" == no-cache ]]; then
 [[ "$target_args" == *' --no-cache' ]]
else
 [[ "$target_args" != *'--no-cache'* ]]
fi
[[ "${TEST_FAIL_BUILD:-}" != 1 ]] || exit 96
archive=${target_args#--output=type=oci,dest=}
archive=${archive%%,rewrite-timestamp=true*}
cp "${TEST_ARCHIVE_FIXTURES}/${component}.oci.tar" "$archive"
if [[ "${TEST_MUTATE_SOURCE:-}" == 1 ]]; then printf '// unexpected source\n' >"${TEST_SOURCE_ROOT}/cmd/stray.go"; fi
EOF
	chmod +x "${fixture_bin}/docker" "${fixture_bin}/make"
	: >"${make_log}"
	: >"${skopeo_log}"
	run_local_fixture() {
		env -u GITHUB_ACTIONS -u GITHUB_EVENT_NAME -u GITHUB_REF -u GITHUB_REPOSITORY -u GITHUB_SHA \
			-u MAKEFLAGS -u MFLAGS -u GNUMAKEFLAGS -u MAKEFILES -u MAKEOVERRIDES \
			-u BUILD -u CI_ARGS -u TARGET_ARGS -u PUSH \
			PATH="${fixture_bin}:${PATH}" TEST_LOCAL_ONLY=1 TEST_SKOPEO_LOG="${skopeo_log}" \
			TEST_MAKE_LOG="${make_log}" TEST_REVISION="${fixture_head}" TEST_VERSION="${fixture_version}" \
			TEST_ARCHIVE_FIXTURES="${fixture_archives}" TEST_SOURCE_ROOT="${fixture_repo}" "$@"
	}
	expect_local_failure() {
		local expected=$1 result
		shift
		if run_local_fixture "$@" >"${test_root}/local-failure.stdout" 2>"${test_root}/local-failure.stderr"; then
			printf 'local negative unexpectedly passed: %s\n' "${expected}" >&2; return 1
		else
			result=$?
		fi
		if [[ -n "${expected}" ]]; then
			grep -Fq "${expected}" "${test_root}/local-failure.stderr"
		else
			[[ "${result}" -eq 96 ]]
		fi
		cases=$((cases + 1))
	}
	run_local_fixture "${fixture_publisher}" local-build _out/cached
	run_local_fixture "${fixture_publisher}" local-verify _out/cached _out/cached/verified.json
	jq -e --arg revision "${fixture_head}" --argjson epoch "${fixture_epoch}" '
		.purpose == "verification" and .sourceRevision == $revision and .sourceEpoch == $epoch and
		.buildMode == "cached" and .claims.archiveInspection and
		([.claims.tests,.claims.reproducibility,.claims.runtime,.claims.publication] | all(. == false)) and
		(.images.imager.indexDigest | startswith("sha256:")) and
		(.images.imager.archiveSha256 | test("^[0-9a-f]{64}$"))
	' "${fixture_repo}/_out/cached/verified.json" >/dev/null
	cases=$((cases + 1))
	run_local_fixture TEST_EXPECT_MODE=no-cache "${fixture_publisher}" local-build --no-cache _out/cold
	jq -e '.buildMode == "no-cache"' "${fixture_repo}/_out/cold/local-build.json" >/dev/null
	[[ "$(wc -l <"${make_log}")" -eq 4 && "$(grep -c -- ' --no-cache' "${make_log}")" -eq 2 ]]
	cases=$((cases + 1))
	expect_local_failure 'new private output directory' "${fixture_publisher}" local-build _out/cached
	expect_local_failure 'refusing to replace existing receipt' "${fixture_publisher}" local-verify _out/cached _out/cached/verified.json
	expect_local_failure 'pinned Buildx' TEST_BUILDX_VERSION=v0.1.0 "${fixture_publisher}" local-build _out/wrong-buildx
	expect_local_failure 'pinned BuildKit image' TEST_BUILDKIT_IMAGE=moby/buildkit:latest "${fixture_publisher}" local-build _out/wrong-buildkit
	expect_local_failure 'running BuildKit container' TEST_RUNNING_IMAGE_ID=sha256:other "${fixture_publisher}" local-build _out/stale-builder
	printf '// stray\n' >"${fixture_repo}/cmd/stray.go"
	expect_local_failure 'clean tracked and untracked source tree' "${fixture_publisher}" local-build _out/untracked
	rm -- "${fixture_repo}/cmd/stray.go"
	printf '// ignored\n' >"${fixture_repo}/cmd/stray.ignored"
	expect_local_failure 'ignored files in Docker source inputs' "${fixture_publisher}" local-build _out/ignored
	rm -- "${fixture_repo}/cmd/stray.ignored"
	printf '; ignored policy\n' >"${fixture_repo}/selinux/stray.ignored"
	expect_local_failure 'ignored files in Docker source inputs' "${fixture_publisher}" local-build _out/ignored-policy
	rm -- "${fixture_repo}/selinux/stray.ignored"
	printf '// dirty\n' >>"${fixture_repo}/go.mod"
	expect_local_failure 'clean tracked and untracked source tree' "${fixture_publisher}" local-build _out/dirty
	git -C "${fixture_repo}" show HEAD:go.mod >"${fixture_repo}/go.mod"
	expect_local_failure 'inherited CI_ARGS' CI_ARGS=--cache-from=registry "${fixture_publisher}" local-build --no-cache _out/cache-override
	expect_local_failure 'inherited TARGET_ARGS' TARGET_ARGS=--output=type=registry "${fixture_publisher}" local-build _out/output-override
	expect_local_failure 'inherited PUSH' PUSH=true "${fixture_publisher}" local-build _out/push-override
	expect_local_failure 'inherited MAKEFLAGS' MAKEFLAGS=-e "${fixture_publisher}" local-build _out/make-override
	expect_local_failure 'inherited PROGRESS' PROGRESS='plain --output=type=registry' "${fixture_publisher}" local-build _out/progress-override
	expect_local_failure 'inherited TOOLS' TOOLS=unreviewed "${fixture_publisher}" local-build _out/tools-override
	expect_local_failure 'inherited BUILDX_GIT_INFO' BUILDX_GIT_INFO=0 "${fixture_publisher}" local-build _out/git-info-override
	expect_local_failure 'inherited http_proxy' http_proxy='https://proxy.invalid --output=type=registry' "${fixture_publisher}" local-build _out/proxy-override
	expect_local_failure 'local-build requires' "${fixture_publisher}" local-build --no-cache-filter=imager _out/filter-override
	expect_local_failure 'outside Docker source inputs' "${fixture_publisher}" local-build cmd/output.ignored
	expect_local_failure 'must be git-ignored' "${fixture_publisher}" local-build unrelated-output
	expect_local_failure 'outside Docker source inputs' "${fixture_publisher}" local-verify _out/cached cmd/receipt.ignored
	[[ "$(wc -l <"${make_log}")" -eq 4 ]]
	cp "${fixture_repo}/_out/cached/local-build.json" "${test_root}/local-build.original.json"
	jq '.builder.buildkitImage = "moby/buildkit:latest"' "${test_root}/local-build.original.json" >"${fixture_repo}/_out/cached/local-build.json"
	expect_local_failure 'local build record identity or builder mismatch' "${fixture_publisher}" local-verify _out/cached _out/wrong-record.json
	cp "${test_root}/local-build.original.json" "${fixture_repo}/_out/cached/local-build.json"
	jq '.sourceEpoch += 1' "${test_root}/local-build.original.json" >"${fixture_repo}/_out/cached/local-build.json"
	expect_local_failure 'local build record identity or builder mismatch' "${fixture_publisher}" local-verify _out/cached _out/wrong-epoch.json
	cp "${test_root}/local-build.original.json" "${fixture_repo}/_out/cached/local-build.json"
	printf 'tampered\n' >>"${fixture_repo}/_out/cached/installer-base.oci.tar"
	expect_local_failure 'archive differs from the local build record' "${fixture_publisher}" local-verify _out/cached _out/tampered.json
	cp "${fixture_archives}/installer-base.oci.tar" "${fixture_repo}/_out/cached/installer-base.oci.tar"
	expect_local_failure 'clean tracked and untracked source tree' TEST_MUTATE_SOURCE=1 "${fixture_publisher}" local-build _out/mutated
	[[ ! -e "${fixture_repo}/_out/mutated/local-build.json" ]]
	rm -- "${fixture_repo}/cmd/stray.go"
	expect_local_failure '' TEST_FAIL_BUILD=1 "${fixture_publisher}" local-build _out/failed
	[[ ! -e "${fixture_repo}/_out/failed/local-build.json" ]]
	expect_local_failure 'publication requires GitHub Actions' "${fixture_publisher}" build _out/not-github
	ln -s cached "${fixture_repo}/_out/linked"
	expect_local_failure 'must not traverse symlinks' "${fixture_publisher}" local-build _out/linked/new
	if grep -Eq '^copy |docker://' "${skopeo_log}"; then
		printf 'local path attempted a registry operation\n' >&2; return 1
	fi
	[[ ! -e "${fixture_repo}/_out/wrong-record.json" && ! -e "${fixture_repo}/_out/tampered.json" ]]
	printf 'local helper archive contract: PASS (%s scenarios, synthetic tools only)\n' "${cases}"
	verify_local_comparison
}

verify_local_comparison() {
	local comparison_root="${fixture_repo}/_out/comparison" fault receipt count=0
	# The comparator reads real content-addressed OCI bytes. Only the preliminary
	# skopeo interface is stubbed, by returning the archive's actual JSON objects.
	cat >"${fixture_bin}/skopeo" <<'PY'
#!/usr/bin/env python3
import json, sys, tarfile
args = sys.argv[1:]
assert args[0] == "inspect" and "--no-creds" in args and args[-1].startswith("oci-archive:")
with tarfile.open(args[-1].removeprefix("oci-archive:")) as archive:
    def blob(descriptor):
        return archive.extractfile("blobs/sha256/" + descriptor["digest"].split(":")[1]).read()
    raw = archive.extractfile("index.json").read()
    index = json.loads(raw)
    if len(index["manifests"]) == 1:
        raw = blob(index["manifests"][0])
        index = json.loads(raw)
    if "--config" in args:
        payload = next(item for item in index["manifests"] if item["platform"]["os"] == "linux")
        raw = blob(json.loads(blob(payload))["config"])
    else:
        assert "--raw" in args
    sys.stdout.buffer.write(raw)
PY
	write_comparison_fixture() {
		python3 - "${fixture_repo}" "${comparison_root}" "$1" <<'PY'
import base64, copy, datetime, gzip, hashlib, io, json, re, sys, tarfile
from pathlib import Path
repo, root, fault = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
template = json.loads((repo / "_out/cached/local-build.json").read_text())
epoch = template["sourceEpoch"]
MANIFEST = "application/vnd.oci.image.manifest.v1+json"
INDEX = "application/vnd.oci.image.index.v1+json"
CONFIG = "application/vnd.oci.image.config.v1+json"
SLSA, SPDX = "https://slsa.dev/provenance/v1", "https://spdx.dev/Document"
SOURCE = "https://github.com/noeljackson/talos"
dockerfile, makefile = (repo / "Dockerfile").read_bytes(), (repo / "Makefile").read_text()
def encoded(value):
    return json.dumps(value, separators=(",", ":")).encode()
def stamp(offset):
    return datetime.datetime.fromtimestamp(epoch + offset, datetime.timezone.utc).isoformat().replace("+00:00", "Z")
for mode in ("cached", "no-cache"):
    folder = root / fault / mode
    folder.mkdir(parents=True)
    record = copy.deepcopy(template)
    record["buildMode"] = mode
    for component in ("installer-base", "imager"):
        bad = fault if mode == "no-cache" and component == "imager" else ""
        blobs = {}
        def put(value, media):
            data = value if isinstance(value, bytes) else encoded(value)
            digest = "sha256:" + hashlib.sha256(data).hexdigest()
            blobs["blobs/sha256/" + digest[7:]] = data
            return {"mediaType": media, "digest": digest, "size": len(data)}
        config = put({"architecture":"amd64", "os":"linux", "config":{"Labels":{
            "org.opencontainers.image.source":SOURCE, "org.opencontainers.image.revision":record["sourceRevision"],
            "org.opencontainers.image.version":record["sourceVersion"], "alpha.talos.dev/version":record["sourceVersion"]}}}, CONFIG)
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w", format=tarfile.USTAR_FORMAT) as tar:
            data = (component + ("changed" if bad == "payload" else "") + "\n").encode()
            member = tarfile.TarInfo("payload")
            member.size, member.mtime = len(data), epoch
            tar.addfile(member, io.BytesIO(data))
        layer = put(gzip.compress(stream.getvalue(), mtime=0), "application/vnd.oci.image.layer.v1.tar+gzip")
        layer["annotations"] = {"buildkit/rewritten-timestamp":str(epoch)}
        payload = put({"schemaVersion":2, "mediaType":MANIFEST, "config":config, "layers":[layer]}, MANIFEST)
        args = {"target":component, "build-arg:SHA":record["sourceRevision"], "build-arg:TAG":record["sourceVersion"],
                "build-arg:ABBREV_TAG":record["release"], "build-arg:SOURCE_DATE_EPOCH":str(epoch),
                "build-arg:INSTALLER_ARCH":"targetarch", "source":re.fullmatch(r"#\s*syntax\s*=\s*(\S+)", dockerfile.splitlines()[0].decode())[1]}
        for variable in ("TOOLS", "PKGS", "TOOLS_PREFIX", "PKGS_PREFIX"):
            args["build-arg:" + variable] = re.search(r"^" + variable + r" \?= (\S+)$", makefile, re.M)[1]
        if mode == "no-cache": args["no-cache"] = ""
        meta = {"invocationId":mode + "-" + component, "startedOn":stamp(10 if mode == "cached" else 30),
                "finishedOn":stamp(20 if mode == "cached" else 40), "buildkit_completeness":{"request":True, "resolvedDependencies":False},
                "buildkit_metadata":{"vcs":{"source":SOURCE, "revision":record["sourceRevision"]},
                                      "source":{"infos":[{"filename":"Dockerfile", "data":base64.b64encode(dockerfile).decode()}]}}}
        dependencies = [{"uri":"pkg:docker/synthetic@1?platform=linux%2Famd64", "digest":{"sha256":"f"*64}}]
        provenance = {"_type":"https://in-toto.io/Statement/v1", "predicateType":SLSA, "subject":[], "predicate":{
            "buildDefinition":{"buildType":"https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-definitions.md",
                               "externalParameters":{"configSource":{"path":"Dockerfile"}, "request":{"frontend":"gateway.v0", "args":args,
                                                     "locals":[{"name":"context"}, {"name":"dockerfile"}]}},
                               "internalParameters":{"buildConfig":{"llbDefinition":[{"id":"step0"}]}}, "resolvedDependencies":dependencies},
            "runDetails":{"builder":{"id":""}, "metadata":meta}}}
        request = provenance["predicate"]["buildDefinition"]["externalParameters"]["request"]
        request["root"] = {"configSource":{"path":"Dockerfile"}, "request":{
            "frontend":"dockerfile.v0", "args":copy.deepcopy(args), "locals":[{"name":"context"}, {"name":"dockerfile"}]}}
        sbom = {"_type":"https://in-toto.io/Statement/v1", "predicateType":SPDX, "subject":[], "predicate":{
            "spdxVersion":"SPDX-2.3", "SPDXID":"SPDXRef-DOCUMENT", "name":"sbom", "documentNamespace":"https://fixture.invalid/" + mode,
            "packages":[{"SPDXID":"SPDXRef-Package", "name":component}], "creationInfo":{"created":stamp(15), "creators":["Tool: synthetic"]}}}
        if fault == "explicit-subjects":
            for document in (provenance, sbom): document["subject"] = [{"name":component, "digest":{"sha256":payload["digest"][7:]}}]
        if bad == "vcs-source": meta["buildkit_metadata"]["vcs"]["source"] = "https://foreign.invalid/repo"
        if bad == "vcs-revision": meta["buildkit_metadata"]["vcs"]["revision"] = "0"*40
        if bad == "epoch": args["build-arg:SOURCE_DATE_EPOCH"] = str(epoch+1)
        if bad == "version": args["build-arg:TAG"] = "v9.9.9"
        if bad == "target": args["target"] = "talos"
        if bad == "tools": args["build-arg:TOOLS"] = "unreviewed"
        if bad == "no-cache": del args["no-cache"]
        if bad == "no-cache-filter": args["no-cache"] = "imager"
        if bad == "mode-min": meta["buildkit_completeness"]["request"] = False
        if bad == "llb-string": provenance["predicate"]["buildDefinition"]["internalParameters"]["buildConfig"]["llbDefinition"] = "not-an-array"
        if bad == "llb-bool": provenance["predicate"]["buildDefinition"]["internalParameters"]["buildConfig"]["llbDefinition"] = True
        if bad == "root-parameters": request["root"]["request"]["args"]["build-arg:GOAMD64"] = "v3"
        if bad == "root-inputs": request["root"]["request"]["inputs"] = {"foreign":{"configSource":{"uri":"https://foreign.invalid"}}}
        if bad == "root-config": request["root"]["configSource"]["path"] = "Foreignfile"
        if bad == "root-frontend": request["root"]["request"]["frontend"] = "foreign.v0"
        if bad == "root-locals": request["root"]["request"]["locals"] = [{"name":"foreign"}]
        if bad == "dockerfile": meta["buildkit_metadata"]["source"]["infos"][0]["data"] = base64.b64encode(b"FROM foreign").decode()
        if bad == "time": meta["finishedOn"] = "not-a-time"
        if bad == "replayed-invocation": meta["invocationId"] = "cached-" + component
        if bad == "dependencies": dependencies[0]["digest"]["sha256"] = "a"*64
        if bad == "parameters": args["build-arg:GOAMD64"] = "v3"
        if bad == "statement-subject": provenance["subject"] = [{"name":component, "digest":{"sha256":"0"*64}}]
        if bad == "sbom-subject": sbom["subject"] = [{"name":component, "digest":{"sha256":"0"*64}}]
        if bad == "sbom-empty": sbom["predicate"]["packages"] = []
        if bad == "sbom-packages-type": sbom["predicate"]["packages"] = "not-an-array"
        if bad == "sbom-creators-type": sbom["predicate"]["creationInfo"]["creators"] = True
        if bad == "sbom-package-entry": sbom["predicate"]["packages"] = [{"name":"missing-id"}]
        layers = []
        for predicate, document in ((SLSA, provenance), (SPDX, sbom)):
            item = put(b"{" if bad == "malformed" and predicate == SLSA else document, "application/vnd.in-toto+json")
            item["annotations"] = {"in-toto.io/predicate-type":predicate}
            layers.append(item)
        if bad == "tampered-blob": blobs["blobs/sha256/" + layers[0]["digest"][7:]] += b" "
        if bad == "missing-provenance": layers = layers[1:]
        if bad == "duplicate-provenance": layers = [layers[0], layers[0]]
        subject = copy.deepcopy(payload)
        if bad == "enclosing-subject": subject["digest"] = "sha256:" + "0"*64
        att = put({"schemaVersion":2, "mediaType":MANIFEST, "artifactType":"application/vnd.docker.attestation.manifest.v1+json",
                   "config":put({}, "application/vnd.oci.empty.v1+json"), "subject":subject, "layers":layers}, MANIFEST)
        att.update(platform={"architecture":"unknown", "os":"unknown"}, annotations={
            "vnd.docker.reference.type":"attestation-manifest", "vnd.docker.reference.digest":payload["digest"]})
        if bad == "index-subject": att["annotations"]["vnd.docker.reference.digest"] = "sha256:" + "0"*64
        payload["platform"] = {"architecture":"amd64", "os":"linux"}
        index = {"schemaVersion":2, "mediaType":INDEX, "manifests":[payload, att]}
        index_desc = put(index, INDEX)
        # Exercise both valid OCI layout encodings without inventing a network.
        blobs["index.json"] = encoded(index if fault == "explicit-subjects" and mode == "cached"
                                      else {"schemaVersion":2, "manifests":[index_desc]})
        blobs["oci-layout"] = encoded({"imageLayoutVersion":"1.0.0"})
        path = folder / (component + ".oci.tar")
        with tarfile.open(path, "w") as archive:
            for name, data in blobs.items():
                member = tarfile.TarInfo(name)
                member.size = len(data)
                archive.addfile(member, io.BytesIO(data))
        record["images"][component] = {"indexDigest":index_desc["digest"], "platformManifest":payload["digest"],
                                       "archiveSha256":hashlib.sha256(path.read_bytes()).hexdigest()}
    if mode == "no-cache" and fault == "record-head": record["sourceRevision"] = "0"*40
    if mode == "no-cache" and fault == "record-mode": record["buildMode"] = "cached"
    (folder / "local-build.json").write_text(json.dumps(record))
PY
	}
	for fault in valid explicit-subjects payload missing-provenance malformed vcs-source vcs-revision epoch version target tools no-cache no-cache-filter \
		mode-min llb-string llb-bool root-parameters root-inputs root-config root-frontend root-locals \
		dockerfile time replayed-invocation dependencies parameters statement-subject sbom-subject sbom-empty \
		sbom-packages-type sbom-creators-type sbom-package-entry \
		tampered-blob duplicate-provenance enclosing-subject index-subject record-head record-mode; do
		write_comparison_fixture "${fault}"
		receipt="${comparison_root}/${fault}/comparison.json"
		if [[ "${fault}" == valid || "${fault}" == explicit-subjects ]]; then
			# The superseded byte-equality rule rejects this legitimate pair.
			if cmp -s "${comparison_root}/${fault}/cached/imager.oci.tar" "${comparison_root}/${fault}/no-cache/imager.oci.tar"; then
				printf 'distinct-invocation archives unexpectedly match\n' >&2; return 1
			fi
			run_local_fixture "${fixture_publisher}" local-compare "${comparison_root}/${fault}/cached" "${comparison_root}/${fault}/no-cache" "${receipt}"
			jq -e '.claims.payloadManifestsEqual and .claims.provenanceAndSBOMBindingsVerified and (.claims.signedOrigin | not) and
			  (.builds.cached.images.imager.indexDigest != .builds["no-cache"].images.imager.indexDigest) and
			  (.builds.cached.attestations.imager.provenance != .builds["no-cache"].attestations.imager.provenance) and
			  (.builds.cached.images.imager.platformManifest == .builds["no-cache"].images.imager.platformManifest)' "${receipt}" >/dev/null
		else
			if run_local_fixture "${fixture_publisher}" local-compare "${comparison_root}/${fault}/cached" "${comparison_root}/${fault}/no-cache" "${receipt}" \
				>"${test_root}/compare-${fault}.stdout" 2>"${test_root}/compare-${fault}.stderr"; then
				printf 'comparison negative unexpectedly passed: %s\n' "${fault}" >&2; return 1
			fi
			[[ ! -e "${receipt}" ]] || { printf 'failed comparison emitted receipt: %s\n' "${fault}" >&2; return 1; }
		fi
		count=$((count + 1))
	done
	expect_local_failure 'two distinct build directories' "${fixture_publisher}" local-compare \
		"${comparison_root}/valid/cached" "${comparison_root}/valid/cached" "${comparison_root}/duplicate.json"
	[[ ! -e "${comparison_root}/duplicate.json" ]]
	local existing_hash
	existing_hash="$(sha256sum "${comparison_root}/valid/comparison.json")"
	expect_local_failure 'refusing to replace existing comparison receipt' "${fixture_publisher}" local-compare \
		"${comparison_root}/valid/cached" "${comparison_root}/valid/no-cache" "${comparison_root}/valid/comparison.json"
	[[ "$(sha256sum "${comparison_root}/valid/comparison.json")" == "${existing_hash}" ]]
	count=$((count + 2))
	printf 'local payload/provenance comparison contract: PASS (%s scenarios, real OCI fixtures; no builds)\n' "${count}"
}

verify_workflow "${workflow}"
verify_publisher
verify_identity
verify_tag_independent_version
verify_existing_index_retry
verify_local_archives

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
