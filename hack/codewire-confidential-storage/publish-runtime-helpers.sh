#!/usr/bin/env bash

set -euo pipefail
umask 077

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
identity_file="${script_dir}/runtime-identity.json"
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
	local identity machinery_version resolved_revision
	local -a machinery_versions

	revision="$(git -C "${repo_root}" rev-parse HEAD)"
	source_epoch="$(git -C "${repo_root}" show -s --format=%ct HEAD)"
	[[ "${revision}" =~ ^[0-9a-f]{40}$ ]] || die "source revision is not a full commit"
	[[ -f "${identity_file}" ]] || die "pinned runtime identity is missing"
	identity="$(jq -er '
		if type == "object" and
		   keys == ["commitAbbreviationLength", "release", "releaseCommit", "schema", "upstreamRepository"] and
		   .schema == "codewire.talos-runtime-source-identity/v1" and
		   .upstreamRepository == "https://github.com/siderolabs/talos" and
		   (.release | type == "string") and
		   (.releaseCommit | type == "string") and
		   (.commitAbbreviationLength | type == "number" and floor == .)
		then [.upstreamRepository, .release, .releaseCommit, (.commitAbbreviationLength | tostring)] | @tsv
		else error("invalid runtime identity")
		end
	' "${identity_file}")" || die "pinned runtime identity is invalid"
	IFS=$'\t' read -r upstream_repository release release_commit abbreviation_length <<<"${identity}"
	[[ "${release}" =~ ^v[0-9]+[.][0-9]+[.][0-9]+$ ]] \
		|| die "pinned Talos release is invalid"
	[[ "${release_commit}" =~ ^[0-9a-f]{40}$ ]] \
		|| die "pinned Talos release commit is invalid"
	[[ "${abbreviation_length}" =~ ^[0-9]+$ && "${abbreviation_length}" -ge 9 && "${abbreviation_length}" -le 40 ]] \
		|| die "pinned commit abbreviation length is invalid"
	git -C "${repo_root}" cat-file -e "${release_commit}^{commit}" 2>/dev/null \
		|| die "pinned Talos release commit is unavailable"
	git -C "${repo_root}" merge-base --is-ancestor "${release_commit}" "${revision}" \
		|| die "pinned Talos release commit is not an ancestor of the deployment revision"

	mapfile -t machinery_versions < <(
		awk '
			$1 == "github.com/siderolabs/talos/pkg/machinery" && $2 ~ /^v[0-9]+[.][0-9]+[.][0-9]+$/ { print $2 }
			$1 == "require" && $2 == "github.com/siderolabs/talos/pkg/machinery" && $3 ~ /^v[0-9]+[.][0-9]+[.][0-9]+$/ { print $3 }
		' \
			"${repo_root}/go.mod"
	)
	[[ "${#machinery_versions[@]}" -eq 1 ]] \
		|| die "go.mod does not contain exactly one Talos machinery release"
	machinery_version=${machinery_versions[0]}
	[[ "${machinery_version}" == "${release}" ]] \
		|| die "pinned Talos release does not match the machinery module version"

	revision_distance="$(git -C "${repo_root}" rev-list --count "${release_commit}..${revision}")"
	[[ "${revision_distance}" =~ ^[0-9]+$ ]] || die "deployment revision distance is invalid"
	revision_abbreviation=${revision:0:abbreviation_length}
	resolved_revision="$(git -C "${repo_root}" rev-parse --verify "${revision_abbreviation}^{commit}" 2>/dev/null)" \
		|| die "deployment revision abbreviation is ambiguous"
	[[ "${resolved_revision}" == "${revision}" ]] \
		|| die "deployment revision abbreviation resolves to a different commit"
	version="${release}-${revision_distance}-g${revision_abbreviation}"
	[[ "${version}" =~ ^v[0-9]+[.][0-9]+[.][0-9]+-[0-9]+-g[0-9a-f]{7,40}$ ]] \
		|| die "deployment version is not an immutable pinned-release version"
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
	local raw_file config_file digest platform_manifest
	raw_file="${work_dir}/${component}.archive.raw.json"
	config_file="${work_dir}/${component}.archive.config.json"
	skopeo inspect --raw "oci-archive:${archive}" >"${raw_file}" \
		|| die "could not inspect local ${component} OCI archive"
	digest="sha256:$(sha256sum "${raw_file}" | awk '{print $1}')"
	platform_manifest="$(index_platform_manifest "${raw_file}")" \
		|| die "${component} archive lacks the exact linux/amd64 image and attestation topology"
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
	if [[ "${component}" == "imager" ]]; then
		verify_imager_archive_timestamps "${archive}" "${platform_manifest}" "${work_dir}"
	fi
	printf '%s\t%s\n' "${digest}" "${platform_manifest}"
}

index_platform_manifest() {
	local raw_file=$1
	jq -er '
		if .mediaType != "application/vnd.oci.image.index.v1+json" then
			error("not an OCI index")
		else
			[.manifests[] |
			 select((.annotations["vnd.docker.reference.type"] // "") != "attestation-manifest")
			] as $images |
			[.manifests[] |
			 select((.annotations["vnd.docker.reference.type"] // "") == "attestation-manifest")
			] as $attestations |
			if (.manifests | length) == 2 and
			   ($images | length) == 1 and
			   ($attestations | length) == 1 and
			   $images[0].platform.os == "linux" and
			   $images[0].platform.architecture == "amd64" and
			   ($images[0].digest | test("^sha256:[0-9a-f]{64}$")) and
			   $attestations[0].annotations["vnd.docker.reference.digest"] == $images[0].digest
			then $images[0].digest
			else error("unexpected image or attestation topology")
			end
		end
	' "${raw_file}"
}

verify_imager_archive_timestamps() {
	local archive=$1 platform_manifest=$2 work_dir=$3
	local platform_file layer_digest expected_date expected_time counts
	local entry_count mismatch_count
	platform_file="${work_dir}/imager.archive.platform.json"
	[[ "${platform_manifest}" =~ ^sha256:[0-9a-f]{64}$ ]] \
		|| die "imager platform manifest digest is invalid"
	tar -xOf "${archive}" "blobs/sha256/${platform_manifest#sha256:}" >"${platform_file}" \
		|| die "could not extract the imager platform manifest"
	layer_digest="$(jq -er --arg source_epoch "${source_epoch}" '
		if .schemaVersion == 2 and
		   .mediaType == "application/vnd.oci.image.manifest.v1+json" and
		   (.layers | length) == 1 and
		   .layers[0].mediaType == "application/vnd.oci.image.layer.v1.tar+gzip" and
		   .layers[0].annotations["buildkit/rewritten-timestamp"] == $source_epoch and
		   (.layers[0].digest | test("^sha256:[0-9a-f]{64}$"))
		then .layers[0].digest
		else error("unexpected imager layer topology or source epoch")
		end
	' "${platform_file}")" || die "imager archive layer contract is invalid"
	read -r expected_date expected_time \
		<<<"$(date --utc --date="@${source_epoch}" '+%F %T')"
	counts="$(
		tar -xOf "${archive}" "blobs/sha256/${layer_digest#sha256:}" \
			| gzip -dc \
			| LC_ALL=C tar --utc --full-time --numeric-owner -tvf - \
			| LC_ALL=C awk \
				-v expected_date="${expected_date}" \
				-v expected_time="${expected_time}" '
					{ total++ }
					$4 != expected_date || $5 != expected_time { mismatches++ }
					END { printf "%d\t%d\n", total, mismatches + 0 }
				'
	)" || die "could not inspect imager layer timestamps"
	IFS=$'\t' read -r entry_count mismatch_count <<<"${counts}"
	[[ "${entry_count}" =~ ^[0-9]+$ && "${entry_count}" -gt 0 ]] \
		|| die "imager archive contains no layer entries"
	[[ "${mismatch_count}" == "0" ]] \
		|| die "imager archive contains ${mismatch_count} entry timestamps that do not equal source epoch ${source_epoch}"
	printf 'verified imager layer timestamps: entries=%s source_epoch=%s\n' \
		"${entry_count}" "${source_epoch}" >&2
}

verify_registry_copy() {
	local component=$1 reference=$2 expected_digest=$3 expected_platform=$4 work_dir=$5
	local raw_file config_file actual_digest actual_platform
	raw_file="${work_dir}/${component}.published.raw.json"
	config_file="${work_dir}/${component}.published.config.json"
	skopeo inspect --raw "docker://${reference}" >"${raw_file}" \
		|| die "could not inspect published ${component} image"
	actual_digest="sha256:$(sha256sum "${raw_file}" | awk '{print $1}')"
	[[ "${actual_digest}" == "${expected_digest}" ]] \
		|| die "published ${component} index differs from the selected immutable digest"
	actual_platform="$(index_platform_manifest "${raw_file}")" \
		|| die "published ${component} lacks the exact linux/amd64 image and attestation topology"
	[[ "${actual_platform}" == "${expected_platform}" ]] \
		|| die "published ${component} platform manifest differs from the exact build"
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
		target_args="--output=type=oci,dest=${archive},rewrite-timestamp=true --provenance=mode=max --sbom=true"
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
	local output_dir=$1 receipt=$2 component archive reference existing_digest existing_platform
	local work_dir raw_file error_file state_file
	declare -A local_digests registry_digests manifests states archive_sha256
	[[ "${output_dir}" == /* ]] || output_dir="${repo_root}/${output_dir}"
	[[ "${receipt}" == /* ]] || receipt="${repo_root}/${receipt}"
	temporary_dir="$(mktemp -d)"
	work_dir="${temporary_dir}"

	# Inspect every local archive and every existing tag before writing either tag.
	for component in "${components[@]}"; do
		archive="$(archive_path "${output_dir}" "${component}")"
		[[ -s "${archive}" ]] || die "missing ${component} OCI archive"
		IFS=$'\t' read -r local_digests["${component}"] manifests["${component}"] \
			< <(inspect_archive "${component}" "${archive}" "${work_dir}")
		archive_sha256["${component}"]="$(sha256sum "${archive}" | awk '{print $1}')"
		reference="$(reference_for "${component}")"
		raw_file="${work_dir}/${component}.existing.raw.json"
		error_file="${work_dir}/${component}.existing.stderr"
		if skopeo inspect --raw "docker://${reference}" >"${raw_file}" 2>"${error_file}"; then
			existing_digest="sha256:$(sha256sum "${raw_file}" | awk '{print $1}')"
			existing_platform="$(index_platform_manifest "${raw_file}")" \
				|| die "immutable ${component} tag has unexpected image or attestation topology"
			[[ "${existing_platform}" == "${manifests[${component}]}" ]] \
				|| die "immutable ${component} tag already exists with a different platform manifest"
			registry_digests["${component}"]="${existing_digest}"
			states["${component}"]=reused
			verify_registry_copy \
				"${component}" "${reference}" "${registry_digests[${component}]}" \
				"${manifests[${component}]}" "${work_dir}"
		else
			grep -Eqi 'manifest unknown|manifest_unknown|name unknown|name_unknown' "${error_file}" \
				|| die "could not prove that the immutable ${component} tag is absent"
			registry_digests["${component}"]="${local_digests[${component}]}"
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
			"${component}" "${reference}" "${registry_digests[${component}]}" \
			"${manifests[${component}]}" "${work_dir}"
	done

	mkdir -p "$(dirname "${receipt}")"
	state_file="${work_dir}/receipt.json"
	jq -n \
		--arg revision "${revision}" \
		--arg version "${version}" \
		--arg deployment_revision "${GITHUB_SHA}" \
		--arg upstream_repository "${upstream_repository}" \
		--arg release "${release}" \
		--arg release_commit "${release_commit}" \
		--arg revision_distance "${revision_distance}" \
		--arg revision_abbreviation "${revision_abbreviation}" \
		--arg imager_reference "$(reference_for imager)" \
		--arg imager_digest "${registry_digests[imager]}" \
		--arg imager_build_index_digest "${local_digests[imager]}" \
		--arg imager_manifest "${manifests[imager]}" \
		--arg imager_archive_sha256 "${archive_sha256[imager]}" \
		--arg imager_state "${states[imager]}" \
		--arg installer_base_reference "$(reference_for installer-base)" \
		--arg installer_base_digest "${registry_digests[installer-base]}" \
		--arg installer_base_build_index_digest "${local_digests[installer-base]}" \
		--arg installer_base_manifest "${manifests[installer-base]}" \
		--arg installer_base_archive_sha256 "${archive_sha256[installer-base]}" \
		--arg installer_base_state "${states[installer-base]}" '
		{schema:"codewire.talos-runtime-helpers.publication/v1",
		 platform:"linux/amd64", sourceRevision:$revision,
		 sourceVersion:$version, deploymentRevision:$deployment_revision,
		 sourceIdentity:{upstreamRepository:$upstream_repository,
		                 release:$release, releaseCommit:$release_commit,
		                 revisionDistance:($revision_distance | tonumber),
		                 revisionAbbreviation:$revision_abbreviation},
		 images:{
		   imager:{reference:$imager_reference, digest:$imager_digest,
		           buildIndexDigest:$imager_build_index_digest,
		           platformManifest:$imager_manifest,
		           archiveSha256:$imager_archive_sha256, state:$imager_state},
		   "installer-base":{reference:$installer_base_reference,
		                    digest:$installer_base_digest,
		                    buildIndexDigest:$installer_base_build_index_digest,
		                    platformManifest:$installer_base_manifest,
		                    archiveSha256:$installer_base_archive_sha256,
		                    state:$installer_base_state}
		 }}
	' >"${state_file}"
	install -m 0600 "${state_file}" "${receipt}"

	[[ -n "${GITHUB_OUTPUT:-}" ]] || die "GitHub output file is unavailable"
	printf 'imager_image=%s/imager\nimager_digest=%s\ninstaller_base_image=%s/installer-base\ninstaller_base_digest=%s\n' \
		"${registry_root}" "${registry_digests[imager]}" \
		"${registry_root}" "${registry_digests[installer-base]}" >>"${GITHUB_OUTPUT}"
	printf 'immutable helper publication complete: version=%s imager=%s installer-base=%s\n' \
		"${version}" "${registry_digests[imager]}" "${registry_digests[installer-base]}"
}

command=${1:-}
case "${command}" in
	preflight)
		[[ $# -eq 1 ]] || die "preflight takes no arguments"
		for tool in awk git jq; do require_command "${tool}"; done
		require_publication_context
		load_identity
		printf 'authorized immutable linux/amd64 helper build: version=%s revision=%s\n' \
			"${version}" "${revision}"
		;;
	build)
		[[ $# -eq 2 ]] || die "build requires OUTPUT_DIR"
		for tool in awk date git gzip jq make sha256sum skopeo tar; do require_command "${tool}"; done
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
		for tool in awk date git gzip jq sha256sum skopeo tar; do require_command "${tool}"; done
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
