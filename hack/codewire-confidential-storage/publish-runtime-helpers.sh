#!/usr/bin/env bash

set -euo pipefail
umask 077

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
registry_root="ghcr.io/noeljackson"
components=(installer-base imager)
temporary_dir=""

usage() {
	cat <<'EOF'
Usage: publish-runtime-helpers.sh preflight
       publish-runtime-helpers.sh build OUTPUT_DIR
       publish-runtime-helpers.sh publish OUTPUT_DIR RECEIPT

Builds both exact linux/amd64 helper archives before publishing immutable tags.
This wrapper is usable only by a push of the exact checked-out
downstream/confidential-storage commit in noeljackson/talos GitHub Actions.
EOF
}

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command is missing: $1"
}

cleanup() {
	if [[ -n "${temporary_dir}" && -d "${temporary_dir}" ]]; then
		find "${temporary_dir}" -xdev -mindepth 1 -delete 2>/dev/null || true
		rmdir -- "${temporary_dir}" 2>/dev/null || true
	fi
}
trap cleanup EXIT INT TERM

require_publication_context() {
	[[ "${GITHUB_ACTIONS:-}" == "true" ]] || die "publication requires GitHub Actions"
	[[ "${GITHUB_EVENT_NAME:-}" == "push" ]] || die "publication requires a push event"
	[[ "${GITHUB_REF:-}" == "refs/heads/downstream/confidential-storage" ]] \
		|| die "publication requires the downstream/confidential-storage ref"
	[[ "${GITHUB_REPOSITORY:-}" == "noeljackson/talos" ]] \
		|| die "publication requires the noeljackson/talos repository"
	[[ "${GITHUB_SHA:-}" =~ ^[0-9a-f]{40}$ ]] \
		|| die "publication requires a full lowercase GitHub event commit"
	[[ "$(git -C "${repo_root}" rev-parse HEAD)" == "${GITHUB_SHA}" ]] \
		|| die "checkout HEAD does not match the GitHub event commit"
	[[ -z "$(git -C "${repo_root}" status --porcelain --untracked-files=no)" ]] \
		|| die "tracked checkout changed after the event commit"
}

load_identity() {
	revision="$(git -C "${repo_root}" rev-parse HEAD)"
	version="$(git -C "${repo_root}" describe --tag --always --match 'v[0-9]*')"
	source_epoch="$(git -C "${repo_root}" show -s --format=%ct HEAD)"
	[[ "${revision}" =~ ^[0-9a-f]{40}$ ]] || die "source revision is not a full commit"
	[[ "${version}" =~ ^v[0-9]+[.][0-9]+[.][0-9]+-[0-9]+-g[0-9a-f]{7,40}$ ]] \
		|| die "deployment version is not an immutable git-describe version"
	[[ "${source_epoch}" =~ ^[0-9]+$ ]] || die "source epoch is invalid"
}

archive_path() {
	local output_dir=$1 component=$2
	printf '%s/%s.oci.tar\n' "${output_dir}" "${component}"
}

reference_for() {
	local component=$1
	printf '%s/%s:%s\n' "${registry_root}" "${component}" "${version}"
}

inspect_archive() {
	local component=$1 archive=$2 work_dir=$3
	local raw_file config_file digest platform_manifest attestation_count
	raw_file="${work_dir}/${component}.archive.raw.json"
	config_file="${work_dir}/${component}.archive.config.json"
	skopeo inspect --raw "oci-archive:${archive}" >"${raw_file}" \
		|| die "could not inspect local ${component} OCI archive"
	jq -e '.mediaType == "application/vnd.oci.image.index.v1+json"' \
		"${raw_file}" >/dev/null || die "${component} archive root is not an OCI index"
	digest="sha256:$(sha256sum "${raw_file}" | awk '{print $1}')"
	platform_manifest="$(jq -er '
		[.manifests[] |
		 select(.platform.os == "linux" and
		        .platform.architecture == "amd64" and
		        (.annotations["vnd.docker.reference.type"] // "") != "attestation-manifest") |
		 .digest] as $images |
		 if ($images | length) == 1 then $images[0] else error("platform count") end
	' "${raw_file}")" || die "${component} archive does not contain exactly one linux/amd64 image"
	attestation_count="$(jq -er --arg subject "${platform_manifest}" '
		[.manifests[] |
		 select((.annotations["vnd.docker.reference.type"] // "") == "attestation-manifest" and
		        (.annotations["vnd.docker.reference.digest"] // "") == $subject)] |
		length
	' "${raw_file}")" || die "could not inspect ${component} archive attestations"
	[[ "${attestation_count}" =~ ^[0-9]+$ && "${attestation_count}" -ge 1 ]] \
		|| die "${component} archive has no attestation bound to its linux/amd64 image"
	skopeo inspect --config --override-os linux --override-arch amd64 \
		"oci-archive:${archive}" >"${config_file}" \
		|| die "could not inspect local ${component} image configuration"
	jq -e \
		--arg revision "${revision}" \
		--arg version "${version}" '
		.architecture == "amd64" and
		.os == "linux" and
		.config.Labels["org.opencontainers.image.source"] == "https://github.com/noeljackson/talos" and
		.config.Labels["org.opencontainers.image.revision"] == $revision and
		.config.Labels["org.opencontainers.image.version"] == $version and
		.config.Labels["alpha.talos.dev/version"] == $version
	' "${config_file}" >/dev/null || die "${component} archive platform or source labels drifted"
	printf '%s\t%s\n' "${digest}" "${platform_manifest}"
}

verify_registry_copy() {
	local component=$1 reference=$2 expected_digest=$3 work_dir=$4
	local raw_file config_file actual_digest
	raw_file="${work_dir}/${component}.published.raw.json"
	config_file="${work_dir}/${component}.published.config.json"
	skopeo inspect --raw "docker://${reference}" >"${raw_file}" \
		|| die "could not inspect published ${component} image"
	actual_digest="sha256:$(sha256sum "${raw_file}" | awk '{print $1}')"
	[[ "${actual_digest}" == "${expected_digest}" ]] \
		|| die "published ${component} index differs from the local archive"
	skopeo inspect --config --override-os linux --override-arch amd64 \
		"docker://${reference}" >"${config_file}" \
		|| die "could not inspect published ${component} configuration"
	jq -e \
		--arg revision "${revision}" \
		--arg version "${version}" '
		.architecture == "amd64" and
		.os == "linux" and
		.config.Labels["org.opencontainers.image.source"] == "https://github.com/noeljackson/talos" and
		.config.Labels["org.opencontainers.image.revision"] == $revision and
		.config.Labels["org.opencontainers.image.version"] == $version and
		.config.Labels["alpha.talos.dev/version"] == $version
	' "${config_file}" >/dev/null || die "published ${component} platform or labels drifted"
}

build_archives() {
	local output_dir=$1 component archive target_args
	[[ "${output_dir}" == /* ]] || output_dir="${repo_root}/${output_dir}"
	mkdir -p "${output_dir}"
	for component in "${components[@]}"; do
		archive="$(archive_path "${output_dir}" "${component}")"
		[[ ! -e "${archive}" ]] || die "refusing to replace existing output: ${archive}"
		target_args="--output=type=oci,dest=${archive} --provenance=mode=max --sbom=true"
		make -C "${repo_root}" "target-${component}" \
			PLATFORM=linux/amd64 \
			INSTALLER_ARCH=targetarch \
			REGISTRY=ghcr.io \
			USERNAME=noeljackson \
			"SHA=${revision}" \
			"TAG=${version}" \
			"SOURCE_DATE_EPOCH=${source_epoch}" \
			"TARGET_ARGS=${target_args}"
		[[ -s "${archive}" ]] || die "${component} build produced no OCI archive"
	done
}

publish_archives() {
	local output_dir=$1 receipt=$2 component archive reference existing_digest
	local work_dir raw_file error_file state_file
	declare -A digests manifests states archive_sha256
	[[ "${output_dir}" == /* ]] || output_dir="${repo_root}/${output_dir}"
	[[ "${receipt}" == /* ]] || receipt="${repo_root}/${receipt}"
	temporary_dir="$(mktemp -d)"
	work_dir="${temporary_dir}"

	# Inspect every local archive and every existing tag before writing either tag.
	for component in "${components[@]}"; do
		archive="$(archive_path "${output_dir}" "${component}")"
		[[ -s "${archive}" ]] || die "missing ${component} OCI archive"
		IFS=$'\t' read -r digests["${component}"] manifests["${component}"] \
			< <(inspect_archive "${component}" "${archive}" "${work_dir}")
		archive_sha256["${component}"]="$(sha256sum "${archive}" | awk '{print $1}')"
		reference="$(reference_for "${component}")"
		raw_file="${work_dir}/${component}.existing.raw.json"
		error_file="${work_dir}/${component}.existing.stderr"
		if skopeo inspect --raw "docker://${reference}" >"${raw_file}" 2>"${error_file}"; then
			existing_digest="sha256:$(sha256sum "${raw_file}" | awk '{print $1}')"
			[[ "${existing_digest}" == "${digests[${component}]}" ]] \
				|| die "immutable ${component} tag already exists with a different digest"
			states["${component}"]=reused
		else
			grep -Eqi 'manifest unknown|manifest_unknown|name unknown|name_unknown' "${error_file}" \
				|| die "could not prove that the immutable ${component} tag is absent"
			states["${component}"]=missing
		fi
	done

	for component in "${components[@]}"; do
		archive="$(archive_path "${output_dir}" "${component}")"
		reference="$(reference_for "${component}")"
		if [[ "${states[${component}]}" == "missing" ]]; then
			skopeo copy --all --format oci "oci-archive:${archive}" "docker://${reference}"
			states["${component}"]=published
		fi
		verify_registry_copy \
			"${component}" "${reference}" "${digests[${component}]}" "${work_dir}"
	done

	mkdir -p "$(dirname "${receipt}")"
	state_file="${work_dir}/receipt.json"
	jq -n \
		--arg revision "${revision}" \
		--arg version "${version}" \
		--arg deployment_revision "${GITHUB_SHA}" \
		--arg imager_reference "$(reference_for imager)" \
		--arg imager_digest "${digests[imager]}" \
		--arg imager_manifest "${manifests[imager]}" \
		--arg imager_archive_sha256 "${archive_sha256[imager]}" \
		--arg imager_state "${states[imager]}" \
		--arg installer_base_reference "$(reference_for installer-base)" \
		--arg installer_base_digest "${digests[installer-base]}" \
		--arg installer_base_manifest "${manifests[installer-base]}" \
		--arg installer_base_archive_sha256 "${archive_sha256[installer-base]}" \
		--arg installer_base_state "${states[installer-base]}" '
		{schema:"codewire.talos-runtime-helpers.publication/v1",
		 platform:"linux/amd64", sourceRevision:$revision,
		 sourceVersion:$version, deploymentRevision:$deployment_revision,
		 images:{
		   imager:{reference:$imager_reference, digest:$imager_digest,
		           platformManifest:$imager_manifest,
		           archiveSha256:$imager_archive_sha256, state:$imager_state},
		   "installer-base":{reference:$installer_base_reference,
		                    digest:$installer_base_digest,
		                    platformManifest:$installer_base_manifest,
		                    archiveSha256:$installer_base_archive_sha256,
		                    state:$installer_base_state}
		 }}
	' >"${state_file}"
	install -m 0600 "${state_file}" "${receipt}"

	[[ -n "${GITHUB_OUTPUT:-}" ]] || die "GitHub output file is unavailable"
	printf 'imager_image=%s/imager\nimager_digest=%s\ninstaller_base_image=%s/installer-base\ninstaller_base_digest=%s\n' \
		"${registry_root}" "${digests[imager]}" \
		"${registry_root}" "${digests[installer-base]}" >>"${GITHUB_OUTPUT}"
	printf 'immutable helper publication complete: version=%s imager=%s installer-base=%s\n' \
		"${version}" "${digests[imager]}" "${digests[installer-base]}"
}

command=${1:-}
case "${command}" in
	preflight)
		[[ $# -eq 1 ]] || die "preflight takes no arguments"
		require_command git
		require_publication_context
		load_identity
		printf 'authorized immutable linux/amd64 helper build: version=%s revision=%s\n' \
			"${version}" "${revision}"
		;;
	build)
		[[ $# -eq 2 ]] || die "build requires OUTPUT_DIR"
		for tool in git jq make sha256sum skopeo; do require_command "${tool}"; done
		require_publication_context
		load_identity
		output_dir=$2
		[[ "${output_dir}" == /* ]] || output_dir="${repo_root}/${output_dir}"
		build_archives "${output_dir}"
		temporary_dir="$(mktemp -d)"
		work_dir="${temporary_dir}"
		for component in "${components[@]}"; do
			inspect_archive "${component}" \
				"$(archive_path "${output_dir}" "${component}")" "${work_dir}"
		done
		;;
	publish)
		[[ $# -eq 3 ]] || die "publish requires OUTPUT_DIR RECEIPT"
		for tool in git jq sha256sum skopeo; do require_command "${tool}"; done
		require_publication_context
		load_identity
		publish_archives "$2" "$3"
		;;
	-h|--help|help)
		usage
		;;
	*)
		usage >&2
		exit 2
		;;
esac
