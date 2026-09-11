#!/usr/bin/env bash

set -euo pipefail
umask 077

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
identity_file="${script_dir}/runtime-identity.json"
registry_root="ghcr.io/noeljackson"
components=(installer-base imager)
temporary_dir=""
buildx_version=v0.36.1
buildkit_image=moby/buildkit@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8
build_mode=cached
local_builder=""
source_input_dirs=(api selinux cmd docs hack internal pkg website tools _out/uki-certs)

usage() {
	cat <<'EOF'
Usage: publish-runtime-helpers.sh preflight
       publish-runtime-helpers.sh build OUTPUT_DIR
       publish-runtime-helpers.sh publish OUTPUT_DIR RECEIPT
       publish-runtime-helpers.sh local-build [--no-cache] OUTPUT_DIR
       publish-runtime-helpers.sh local-verify OUTPUT_DIR RECEIPT
       publish-runtime-helpers.sh local-compare CACHED_DIR NO_CACHE_DIR RECEIPT

Builds both exact linux/amd64 helper archives before publishing immutable tags.
This wrapper is usable only by a push of the exact checked-out
downstream/confidential-storage commit in noeljackson/talos GitHub Actions.
The local commands create or inspect archives only; they never publish.
local-build requires a new output directory under an existing parent, a clean
source tree, and an already-running single-node pinned docker-container builder.
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

require_clean_source() {
	[[ -z "$(git -C "${repo_root}" status --porcelain --untracked-files=all)" ]] \
		|| die "local qualification requires a clean tracked and untracked source tree"
	# These directories are admitted by .dockerignore; ignored source files can
	# still enter COPY even though git status omits them. _out is not a source
	# directory, except for the explicitly admitted UKI certificate directory.
	[[ -z "$(git -C "${repo_root}" ls-files --others --ignored --exclude-standard -- \
		"${source_input_dirs[@]}")" ]] \
		|| die "ignored files in Docker source inputs are not exact-commit material"
}

require_local_environment() {
	local name
	# Make command-line/environment hooks can replace the command, add another
	# exporter, or turn a supposed no-cache build back into a cache-dependent one.
	for name in MAKEFLAGS MFLAGS GNUMAKEFLAGS MAKEFILES MAKEOVERRIDES BUILD CI_ARGS TARGET_ARGS PUSH \
		BUILDKIT_SYNTAX BUILDKIT_DOCKERFILE_CHECK BUILDX_BAKE_FILE BUILDX_GIT_INFO; do
		[[ -z "${!name:-}" ]] || die "local archive builds reject inherited ${name} overrides"
	done
	# The upstream Makefile also forwards environment-overridable package,
	# compiler, and progress values to Buildx. Use its actual assignment names
	# rather than a second incomplete list of payload inputs.
	[[ -f "${repo_root}/Makefile" ]] || die "checked-in Makefile is required"
	while IFS= read -r name; do
		[[ -z "${!name:-}" ]] || die "local archive builds reject inherited ${name} overrides"
	done < <(awk '
		/^[A-Za-z_][A-Za-z0-9_]*[[:space:]]*[:?+]?=/ {
			name=$0; sub(/[[:space:]]*[:?+]?=.*/, "", name); print name
		}
		/^COMMON_ARGS[[:space:]]/ {
			line=$0
			while (match(line, /\$\([A-Za-z_][A-Za-z0-9_]*\)/)) {
				print substr(line, RSTART + 2, RLENGTH - 3)
				line=substr(line, RSTART + RLENGTH)
			}
		}' "${repo_root}/Makefile")
}

require_local_builder() {
	local actual_version inspection node_name container image_id
	actual_version="$(docker buildx version)" || die "could not inspect Buildx version"
	[[ "$(awk '{print $2}' <<<"${actual_version}")" == "${buildx_version}" ]] \
		|| die "local builds require pinned Buildx ${buildx_version}"
	inspection="$(docker buildx inspect --timeout 20s)" || die "could not inspect local builder"
	[[ "$(awk '/^Driver:/ {print $2}' <<<"${inspection}")" == docker-container ]] \
		|| die "local builds require the pinned docker-container builder"
	[[ "$(grep -c '^Name:' <<<"${inspection}")" -eq 2 && \
		"$(grep -c '^Status:[[:space:]]*running$' <<<"${inspection}")" -eq 1 ]] \
		|| die "local builds require exactly one already-running builder node"
	! grep -q '^Error:' <<<"${inspection}" || die "local builder reports an error"
	local_builder="$(awk '/^Name:/ {print $2; exit}' <<<"${inspection}")"
	node_name="$(awk '/^Name:/ {name=$2} END {print name}' <<<"${inspection}")"
	[[ "${local_builder}" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ && \
		"${node_name}" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || die "invalid builder name"
	grep -Fq "image=\"${buildkit_image}\"" <<<"${inspection}" \
		|| die "local builder does not select the pinned BuildKit image"
	# Check the running container, not merely configured driver options (which
	# may have changed since the container was started). No bootstrap or pull.
	container="$(docker inspect --type container "buildx_buildkit_${node_name}")" \
		|| die "could not inspect the running local BuildKit container"
	image_id="$(docker image inspect "${buildkit_image}" --format '{{.Id}}')" \
		|| die "pinned BuildKit image is not available locally"
	jq -e --arg image "${buildkit_image}" --arg image_id "${image_id}" '
		length == 1 and .[0].State.Running == true and
		.[0].Config.Image == $image and .[0].Image == $image_id
	' <<<"${container}" >/dev/null || die "running BuildKit container does not match its pinned image"
}

resolve_local_path() {
	local path=$1
	[[ "${path}" =~ ^[a-zA-Z0-9_./-]+$ ]] || die "local artifact paths must not contain spaces or shell/exporter syntax"
	[[ "${path}" == /* ]] || path="${repo_root}/${path}"
	[[ "$(realpath -m -s -- "${path}")" == "$(realpath -m -- "${path}")" ]] \
		|| die "local artifact paths must not traverse symlinks"
	realpath -m -- "${path}"
}

require_local_output_path() {
	local path=$1 source_dir
	for source_dir in "${source_input_dirs[@]}"; do
		[[ "${path}" != "${repo_root}/${source_dir}" && "${path}" != "${repo_root}/${source_dir}/"* ]] \
			|| die "local outputs must be outside Docker source inputs"
	done
	if [[ "${path}" == "${repo_root}/"* ]]; then
		git -C "${repo_root}" check-ignore --quiet -- "${path}" \
			|| die "local outputs inside the repository must be git-ignored"
	fi
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
	skopeo inspect --no-creds --raw "oci-archive:${archive}" >"${raw_file}" \
		|| die "could not inspect local ${component} OCI archive"
	digest="sha256:$(sha256sum "${raw_file}" | awk '{print $1}')"
	platform_manifest="$(index_platform_manifest "${raw_file}")" \
		|| die "${component} archive lacks the exact linux/amd64 image and attestation topology"
	skopeo inspect --no-creds --config --override-os linux --override-arch amd64 \
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
		if [[ "${build_mode}" == no-cache ]]; then target_args+=" --no-cache"; fi
		make -C "${repo_root}" "target-${component}" \
			"BUILD=docker buildx build${local_builder:+ --builder ${local_builder}}" \
			PUSH=false CI_ARGS= \
			PLATFORM=linux/amd64 \
			INSTALLER_ARCH=targetarch \
			REGISTRY=ghcr.io \
			USERNAME=noeljackson \
			"SHA=${revision}" \
			"TAG=${version}" \
			"ABBREV_TAG=${release}" \
			"SOURCE_DATE_EPOCH=${source_epoch}" \
			"TARGET_ARGS=${target_args}"
		[[ -s "${archive}" ]] || die "${component} build produced no OCI archive"
		done
}

inspect_local_archives() {
	local output_dir=$1 receipt=$2 purpose=$3 component archive result
	local work_dir record="${output_dir}/local-build.json"
	local -A indexes manifests archives
	[[ ! -e "${receipt}" && ! -L "${receipt}" ]] || die "refusing to replace existing receipt: ${receipt}"
	[[ -d "$(dirname "${receipt}")" ]] || die "receipt parent directory must already exist"
	[[ -n "${temporary_dir}" ]] || temporary_dir="$(mktemp -d)"
	work_dir="$(mktemp -d "${temporary_dir}/inspection.XXXXXX")"
	for component in "${components[@]}"; do
		archive="$(archive_path "${output_dir}" "${component}")"
		[[ -f "${archive}" && ! -L "${archive}" && -s "${archive}" ]] || die "missing regular ${component} archive"
		result="$(inspect_archive "${component}" "${archive}" "${work_dir}")" \
			|| die "local ${component} archive inspection failed"
		IFS=$'\t' read -r indexes["${component}"] manifests["${component}"] <<<"${result}"
		archives["${component}"]="$(sha256sum "${archive}" | awk '{print $1}')"
		if [[ "${purpose}" == verification ]]; then
			jq -e --arg component "${component}" --arg index "${indexes[${component}]}" \
				--arg manifest "${manifests[${component}]}" --arg archive "${archives[${component}]}" '
				.images[$component].indexDigest == $index and
				.images[$component].platformManifest == $manifest and
				.images[$component].archiveSha256 == $archive
			' "${record}" >/dev/null || die "${component} archive differs from the local build record"
		fi
	done
	require_clean_source
	# This is artifact inspection, not a test/reproducibility/runtime receipt.
	# In particular, matching self-consistent metadata cannot prove a rebuild.
	jq -n --arg purpose "${purpose}" --arg revision "${revision}" \
		--arg source_epoch "${source_epoch}" --arg version "${version}" \
		--arg release "${release}" --arg release_commit "${release_commit}" \
		--arg build_mode "${build_mode}" --arg buildx "${buildx_version}" \
		--arg buildkit "${buildkit_image}" --arg builder "${local_builder}" \
		--arg imager_index "${indexes[imager]}" --arg imager_manifest "${manifests[imager]}" \
		--arg imager_archive "${archives[imager]}" \
		--arg installer_index "${indexes[installer-base]}" --arg installer_manifest "${manifests[installer-base]}" \
		--arg installer_archive "${archives[installer-base]}" '
		{schema:"codewire.talos-runtime-helpers.local-archives/v1", purpose:$purpose,
		 sourceRevision:$revision, sourceEpoch:($source_epoch | tonumber), sourceVersion:$version,
		 release:$release, releaseCommit:$release_commit, platform:"linux/amd64", buildMode:$build_mode,
		 builder:{name:$builder, buildxVersion:$buildx, buildkitImage:$buildkit},
		 claims:{archiveInspection:true, tests:false, reproducibility:false, runtime:false, publication:false},
		 images:{imager:{indexDigest:$imager_index, platformManifest:$imager_manifest, archiveSha256:$imager_archive},
		         "installer-base":{indexDigest:$installer_index, platformManifest:$installer_manifest, archiveSha256:$installer_archive}}}
	' >"${work_dir}/local-receipt.json"
	(set -o noclobber; cat "${work_dir}/local-receipt.json" >"${receipt}") \
		|| die "could not create a new local receipt: ${receipt}"
	printf 'local archive %s complete (no publication): %s\n' "${purpose}" "${receipt}"
}

load_local_build_record() {
	local record=$1 resolved
	[[ -f "${record}" && ! -L "${record}" ]] || die "local-build.json is required for local verification"
	resolved="$(jq -er --arg revision "${revision}" --arg source_epoch "${source_epoch}" \
		--arg version "${version}" --arg release "${release}" --arg release_commit "${release_commit}" \
		--arg buildx "${buildx_version}" --arg buildkit "${buildkit_image}" '
		if .schema == "codewire.talos-runtime-helpers.local-archives/v1" and .purpose == "build" and
		   .sourceRevision == $revision and .sourceEpoch == ($source_epoch | tonumber) and
		   .sourceVersion == $version and .release == $release and .releaseCommit == $release_commit and
		   .platform == "linux/amd64" and (.buildMode == "cached" or .buildMode == "no-cache") and
		   .builder.buildxVersion == $buildx and .builder.buildkitImage == $buildkit and
		   (.builder.name | type == "string" and test("^[a-zA-Z0-9][a-zA-Z0-9_.-]*$")) and
		   .claims == {archiveInspection:true, tests:false, reproducibility:false, runtime:false, publication:false}
		then [.buildMode, .builder.name] | @tsv
		else error("local build record identity or builder mismatch") end
	' "${record}")" || die "local build record identity or builder mismatch"
	IFS=$'\t' read -r build_mode local_builder <<<"${resolved}"
}

compare_local_archives() {
	local cached=$1 cold=$2 receipt=$3 directory leg builder=""
	[[ "${cached}" != "${cold}" ]] || die "comparison requires two distinct build directories"
	[[ ! -e "${receipt}" && ! -L "${receipt}" ]] || die "refusing to replace existing comparison receipt"
	[[ -d "$(dirname "${receipt}")" ]] || die "comparison receipt parent must exist"
	temporary_dir="$(mktemp -d)"
	for leg in cached no-cache; do
		directory=${cached}
		if [[ "${leg}" == no-cache ]]; then directory=${cold}; fi
		load_local_build_record "${directory}/local-build.json"
		[[ "${build_mode}" == "${leg}" ]] || die "comparison requires cached then true no-cache build records"
		[[ -z "${builder}" || "${builder}" == "${local_builder}" ]] || die "comparison builder names differ"
		builder=${local_builder}
		inspect_local_archives "${directory}" "${temporary_dir}/${leg}.json" verification
	done
	# Stream and hash the actual OCI blobs; never extract archive paths to disk.
	# Local VCS hints and unsigned statements are checked bindings, not proof of
	# authenticated workflow origin. Keep every original attestation unchanged.
	python3 - "${repo_root}" "${cached}" "${cold}" \
		"${temporary_dir}/cached.json" "${temporary_dir}/no-cache.json" \
		>"${temporary_dir}/comparison.json" <<'PY'
import base64
import copy
import datetime
import hashlib
import json
from pathlib import Path
import re
import sys
import tarfile

INDEX = "application/vnd.oci.image.index.v1+json"
MANIFEST = "application/vnd.oci.image.manifest.v1+json"
CONFIG = "application/vnd.oci.image.config.v1+json"
EMPTY = "application/vnd.oci.empty.v1+json"
ATTESTATION = "application/vnd.docker.attestation.manifest.v1+json"
SLSA = "https://slsa.dev/provenance/v1"
SPDX = "https://spdx.dev/Document"
SOURCE = "https://github.com/noeljackson/talos"
MAX_JSON = 32 * 1024 * 1024

def require(condition, message):
    if not condition:
        raise ValueError(message)

def pairs(items):
    result = {}
    for key, value in items:
        require(key not in result, "duplicate JSON key")
        result[key] = value
    return result

def decode(data):
    return json.loads(data, object_pairs_hook=pairs)

def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()

def file_hash(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()

def timestamp(value):
    require(isinstance(value, str) and re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z", value),
            "invalid attestation timestamp")
    return datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))

class Archive:
    def __init__(self, path):
        self.blobs, self.documents, self.names = {}, {}, set()
        with tarfile.open(path, "r|*") as archive:
            for member in archive:
                name = member.name.removeprefix("./")
                require(name not in self.names, "duplicate OCI archive member")
                self.names.add(name)
                if member.isdir():
                    require(name in (".", "blobs", "blobs/sha256"), "unexpected OCI directory")
                    continue
                require(member.isfile(), "non-regular OCI archive member")
                is_blob = re.fullmatch(r"blobs/sha256/[0-9a-f]{64}", name)
                require(is_blob or name in ("index.json", "oci-layout"), "unexpected OCI archive member")
                stream, value, saved = archive.extractfile(member), hashlib.sha256(), bytearray()
                for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                    value.update(chunk)
                    if member.size <= MAX_JSON:
                        saved.extend(chunk)
                if is_blob:
                    key = "sha256:" + value.hexdigest()
                    require(key == "sha256:" + name.rsplit("/", 1)[1], "OCI blob digest mismatch")
                    self.blobs[key] = member.size
                    if saved.lstrip().startswith(b"{"):
                        self.documents[key] = decode(saved)
                else:
                    require(member.size <= MAX_JSON, "oversized OCI metadata")
                    self.documents[name] = decode(saved)
                    self.blobs[name] = "sha256:" + value.hexdigest()
        require(self.documents["oci-layout"] == {"imageLayoutVersion": "1.0.0"}, "invalid OCI layout")

    def descriptor(self, item, media_type=None):
        key, size = item["digest"], item["size"]
        require(isinstance(key, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", key), "invalid OCI digest")
        require(type(size) is int and size >= 0, "invalid OCI size")
        if media_type:
            require(item["mediaType"] == media_type, "unexpected OCI media type")
        if "data" in item:
            inline = base64.b64decode(item["data"], validate=True)
            require(digest(inline) == key and len(inline) == size, "invalid inline OCI descriptor")
            if key not in self.blobs:
                self.blobs[key], self.documents[key] = size, decode(inline)
        require(self.blobs.get(key) == size, "missing OCI blob or descriptor size mismatch")
        return self.documents.get(key)

    def index(self, expected):
        outer = self.documents["index.json"]
        require(outer["schemaVersion"] == 2, "invalid OCI index")
        if self.blobs["index.json"] == expected:
            return outer
        require(len(outer["manifests"]) == 1 and outer["manifests"][0]["digest"] == expected,
                "OCI archive index does not select the recorded index")
        return self.descriptor(outer["manifests"][0], INDEX)

def statement(archive, descriptor, predicate, subject):
    result = archive.descriptor(descriptor, "application/vnd.in-toto+json")
    require(descriptor["annotations"]["in-toto.io/predicate-type"] == predicate,
            "attestation predicate annotation mismatch")
    require(result["_type"] == "https://in-toto.io/Statement/v1" and result["predicateType"] == predicate,
            "unexpected attestation statement")
    require(isinstance(result["subject"], list), "missing statement subjects")
    # Pinned BuildKit's OCI artifact format uses the enclosing subject and
    # legitimately leaves this array empty. Any explicit subject must agree.
    for item in result["subject"]:
        require(item["digest"] == {"sha256": subject.removeprefix("sha256:")} and item["name"],
                "foreign attestation statement subject")
    return result

def comparable_request(request):
    result = copy.deepcopy(request)
    result["args"].pop("no-cache", None)
    if "root" in result:
        result["root"]["request"] = comparable_request(result["root"]["request"])
    return result

def verify(path, record, component, repo):
    expected = record["images"][component]
    archive = Archive(path)
    require(file_hash(path) == expected["archiveSha256"], "archive changed since build-record inspection")
    root = archive.index(expected["indexDigest"])
    require(root["mediaType"] == INDEX and root["schemaVersion"] == 2 and len(root["manifests"]) == 2,
            "unexpected image/attestation topology")
    payloads = [item for item in root["manifests"] if item.get("platform") == {"architecture": "amd64", "os": "linux"}]
    require(len(payloads) == 1, "missing exact AMD64 platform")
    payload = payloads[0]
    require(payload["digest"] == expected["platformManifest"], "recorded payload manifest differs")
    manifest = archive.descriptor(payload, MANIFEST)
    require(manifest["schemaVersion"] == 2 and manifest["mediaType"] == MANIFEST, "invalid payload manifest")
    config = archive.descriptor(manifest["config"], CONFIG)
    require(config["architecture"] == "amd64" and config["os"] == "linux", "foreign image platform")
    labels = config["config"]["Labels"]
    require(labels["org.opencontainers.image.source"] == SOURCE and
            labels["org.opencontainers.image.revision"] == record["sourceRevision"] and
            labels["org.opencontainers.image.version"] == record["sourceVersion"] and
            labels["alpha.talos.dev/version"] == record["sourceVersion"], "foreign image source labels")
    require(len(manifest["layers"]) > 0, "empty image payload")
    for layer in manifest["layers"]:
        archive.descriptor(layer)
    attached = [item for item in root["manifests"] if item is not payload][0]
    require(attached["platform"] == {"architecture": "unknown", "os": "unknown"} and
            attached["annotations"]["vnd.docker.reference.type"] == "attestation-manifest" and
            attached["annotations"]["vnd.docker.reference.digest"] == payload["digest"], "foreign attestation index binding")
    att = archive.descriptor(attached, MANIFEST)
    require(att["schemaVersion"] == 2 and att["mediaType"] == MANIFEST and att["artifactType"] == ATTESTATION,
            "unexpected attestation artifact format")
    require(all(att["subject"][key] == payload[key] for key in ("digest", "size", "mediaType")),
            "foreign enclosing attestation subject")
    require(archive.descriptor(att["config"], EMPTY) == {}, "nonempty attestation config")
    layers = att["layers"]
    require(len(layers) == 2, "provenance and SBOM must each occur exactly once")
    predicates = {item["annotations"]["in-toto.io/predicate-type"]: item for item in layers}
    require(set(predicates) == {SLSA, SPDX}, "missing or duplicated provenance/SBOM")
    provenance = statement(archive, predicates[SLSA], SLSA, payload["digest"])
    sbom = statement(archive, predicates[SPDX], SPDX, payload["digest"])
    pred = provenance["predicate"]
    definition, meta = pred["buildDefinition"], pred["runDetails"]["metadata"]
    require(definition["buildType"] == "https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-definitions.md",
            "foreign provenance build type")
    external = definition["externalParameters"]
    require(external["configSource"] == {"path": "Dockerfile"}, "foreign provenance config source")
    request, vcs = external["request"], meta["buildkit_metadata"]["vcs"]
    require(vcs["source"] in (SOURCE, SOURCE + ".git", "git@github.com:noeljackson/talos.git", "ssh://git@github.com/noeljackson/talos.git") and
            vcs["revision"] == record["sourceRevision"], "foreign provenance VCS source/revision")
    require(request["frontend"] == "gateway.v0", "unexpected provenance frontend")
    require(sorted(item["name"] for item in request["locals"]) == ["context", "dockerfile"],
            "unexpected local build inputs")
    steps = definition["internalParameters"]["buildConfig"]["llbDefinition"]
    require(meta["buildkit_completeness"]["request"] is True and isinstance(steps, list) and
            steps and all(isinstance(step, dict) and isinstance(step.get("id"), str) and step["id"] for step in steps),
            "provenance is not structured mode=max")
    expected_args = {"target": component, "build-arg:SHA": record["sourceRevision"],
                     "build-arg:TAG": record["sourceVersion"], "build-arg:ABBREV_TAG": record["release"],
                     "build-arg:SOURCE_DATE_EPOCH": str(record["sourceEpoch"]), "build-arg:INSTALLER_ARCH": "targetarch"}
    makefile = (repo / "Makefile").read_text()
    for variable in ("TOOLS", "PKGS", "TOOLS_PREFIX", "PKGS_PREFIX"):
        expected_args["build-arg:" + variable] = re.search(r"^" + variable + r" \?= (\S+)$", makefile, re.M)[1]
    def validate_request(candidate):
        require(isinstance(candidate, dict), "malformed provenance request")
        require(candidate.get("frontend", "dockerfile.v0") in ("gateway.v0", "dockerfile.v0"), "foreign nested frontend")
        if "locals" in candidate:
            require(sorted(item["name"] for item in candidate["locals"]) == ["context", "dockerfile"], "foreign nested local inputs")
        # This fixed recipe supplies only local context/dockerfile, not named
        # --build-context inputs or a second nested build owner.
        require("inputs" not in candidate or candidate["inputs"] == {}, "unexpected named provenance inputs")
        args = candidate["args"]
        require(all(args.get(key) == value for key, value in expected_args.items()), "foreign provenance build identity")
        require((args.get("no-cache") == "" and "no-cache" in args) if record["buildMode"] == "no-cache"
                else "no-cache" not in args, "provenance cache mode differs from build record")
        require(not candidate.get("secrets") and not candidate.get("ssh"), "unexpected build secret/SSH inputs")
        if "root" in candidate:
            root_request = candidate["root"]
            require(set(root_request) == {"configSource", "request"} and
                    root_request["configSource"] == {"path": "Dockerfile"}, "foreign root config source")
            validate_request(root_request["request"])
    validate_request(request)
    dockerfile = (repo / "Dockerfile").read_bytes()
    syntax = re.fullmatch(r"#\s*syntax\s*=\s*(\S+)", dockerfile.splitlines()[0].decode())
    require(syntax and request["args"]["source"] == syntax[1],
            "foreign Dockerfile frontend source")
    infos = [info for info in meta["buildkit_metadata"]["source"]["infos"] if info["filename"] == "Dockerfile"]
    require(infos and all(base64.b64decode(info["data"], validate=True) == dockerfile for info in infos),
            "provenance Dockerfile differs from checked-out source")
    require(isinstance(meta["invocationId"], str) and meta["invocationId"], "missing build invocation")
    require(timestamp(meta["startedOn"]).timestamp() >= record["sourceEpoch"] and
            timestamp(meta["finishedOn"]) >= timestamp(meta["startedOn"]), "invalid build time ordering")
    dependencies = definition["resolvedDependencies"]
    require(dependencies and all(item["uri"] and item["digest"] and
            all(algorithm in ("sha1", "sha256") and re.fullmatch(r"[0-9a-f]{" + str(40 if algorithm == "sha1" else 64) + r"}", value)
                for algorithm, value in item["digest"].items()) for item in dependencies), "missing or invalid build dependencies")
    doc = sbom["predicate"]
    packages, creators = doc["packages"], doc["creationInfo"]["creators"]
    require(doc["spdxVersion"] == "SPDX-2.3" and doc["SPDXID"] == "SPDXRef-DOCUMENT" and
            isinstance(doc["documentNamespace"], str) and doc["documentNamespace"] and
            isinstance(doc["name"], str) and doc["name"] and isinstance(packages, list) and packages and
            all(isinstance(package, dict) and isinstance(package.get("SPDXID"), str) and package["SPDXID"].startswith("SPDXRef-") and
                isinstance(package.get("name"), str) and package["name"] for package in packages) and
            isinstance(creators, list) and creators and all(isinstance(creator, str) and creator for creator in creators),
            "malformed SPDX SBOM")
    timestamp(doc["creationInfo"]["created"])
    return {"manifestDigest": attached["digest"], "provenanceDigest": predicates[SLSA]["digest"],
            "sbomDigest": predicates[SPDX]["digest"], "provenance": provenance, "sbom": sbom}

try:
    repo, cached, cold, cached_record, cold_record = map(Path, sys.argv[1:])
    records = [decode(cached_record.read_bytes()), decode(cold_record.read_bytes())]
    result = {"schema": "codewire.talos-runtime-helpers.local-comparison/v1", "builds": {}}
    for directory, record in zip((cached, cold), records):
        record["attestations"] = {component: verify(directory / (component + ".oci.tar"), record, component, repo)
                                  for component in ("installer-base", "imager")}
        result["builds"][record["buildMode"]] = record
    for component in ("installer-base", "imager"):
        require(records[0]["images"][component]["platformManifest"] == records[1]["images"][component]["platformManifest"],
                "consumed payload manifests differ")
        proofs = [record["attestations"][component]["provenance"]["predicate"] for record in records]
        require(proofs[0]["runDetails"]["metadata"]["invocationId"] != proofs[1]["runDetails"]["metadata"]["invocationId"],
                "comparison requires independent build invocations")
        definitions = [proof["buildDefinition"] for proof in proofs]
        requests = [comparable_request(definition["externalParameters"]["request"]) for definition in definitions]
        require(requests[0] == requests[1], "warm/cold build request trees differ")
        dependencies = [sorted(definition["resolvedDependencies"], key=lambda item: json.dumps(item, sort_keys=True)) for definition in definitions]
        require(dependencies[0] == dependencies[1], "warm/cold dependency digests differ")
    result["claims"] = {"payloadManifestsEqual": True, "provenanceAndSBOMBindingsVerified": True,
                        "signedOrigin": False, "warmCacheHit": False, "tests": False, "runtime": False, "publication": False}
    print(json.dumps(result, indent=2))
except (ValueError, KeyError, TypeError, IndexError, OSError, tarfile.TarError) as error:
    print("error: local comparison failed: " + str(error), file=sys.stderr)
    sys.exit(1)
PY
	require_clean_source
	(set -o noclobber; cat "${temporary_dir}/comparison.json" >"${receipt}") \
		|| die "could not create new comparison receipt"
	printf 'local payload comparison and provenance/SBOM inspection complete (unsigned): %s\n' "${receipt}"
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
	local-build)
		shift
		if [[ "${1:-}" == --no-cache ]]; then build_mode=no-cache; shift; fi
		[[ $# -eq 1 ]] || die "local-build requires [--no-cache] OUTPUT_DIR"
		for tool in awk date docker git grep gzip jq make realpath sha256sum skopeo tar; do require_command "${tool}"; done
		require_local_environment
		require_clean_source
		load_identity
		require_local_builder
		output_dir="$(resolve_local_path "$1")"
		require_local_output_path "${output_dir}"
		[[ ! -e "${output_dir}" && ! -L "${output_dir}" ]] || die "local-build requires a new private output directory"
		mkdir -m 0700 -- "${output_dir}" || die "output parent must exist and output directory must be new"
		build_archives "${output_dir}"
		inspect_local_archives "${output_dir}" "${output_dir}/local-build.json" build
		;;
	local-verify)
		[[ $# -eq 3 ]] || die "local-verify requires OUTPUT_DIR RECEIPT"
		for tool in awk date git gzip jq realpath sha256sum skopeo tar; do require_command "${tool}"; done
		require_local_environment
		require_clean_source
		load_identity
		output_dir="$(resolve_local_path "$2")"
		receipt="$(resolve_local_path "$3")"
		require_local_output_path "${receipt}"
		load_local_build_record "${output_dir}/local-build.json"
		inspect_local_archives "${output_dir}" "${receipt}" verification
		;;
	local-compare)
		[[ $# -eq 4 ]] || die "local-compare requires CACHED_DIR NO_CACHE_DIR RECEIPT"
		for tool in awk date git gzip jq python3 realpath sha256sum skopeo tar; do require_command "${tool}"; done
		require_local_environment
		require_clean_source
		load_identity
		cached_dir="$(resolve_local_path "$2")"
		cold_dir="$(resolve_local_path "$3")"
		receipt="$(resolve_local_path "$4")"
		require_local_output_path "${receipt}"
		compare_local_archives "${cached_dir}" "${cold_dir}" "${receipt}"
		;;
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
