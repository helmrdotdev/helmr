#!/bin/sh
set -eu

archive=$1
image_id_file=$2
rebuild=${BOOT_TOOLS_REBUILD:-0}
arch=${ARCH:-x86_64}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
tools_root="$repo_root/images/tools"

if [ "$arch" != "x86_64" ]; then
	printf 'unsupported ARCH: %s\n' "$arch" >&2
	exit 1
fi

mkdir -p "$(dirname "$archive")" "$(dirname "$image_id_file")"
archive_dir=$(CDPATH='' cd -- "$(dirname -- "$archive")" && pwd)
archive="$archive_dir/$(basename "$archive")"
image_id_dir=$(CDPATH='' cd -- "$(dirname -- "$image_id_file")" && pwd)
image_id_file="$image_id_dir/$(basename "$image_id_file")"
case "$archive" in
	"$tools_root"/*) archive_rel=${archive#"$tools_root"/} ;;
	*)
		printf 'boot tools archive must stay within the repository: %s\n' "$archive" >&2
		exit 1
		;;
esac
if [ "$rebuild" = "1" ] || [ ! -f "$archive" ]; then
	tmp_archive="${archive}.tmp"
	tmp_archive_rel="${archive_rel}.tmp"
	trap 'rm -f "$tmp_archive"' EXIT
	(
		cd "$tools_root"
		apko build \
			apko.yaml \
			helmr-boot-tools:local \
			"$tmp_archive_rel" \
			--arch x86_64 \
			--lockfile apko.x86_64.lock.json \
			--build-date 1970-01-01T00:00:00Z \
			--sbom=false \
			--vcs=false
	)
	mv "$tmp_archive" "$archive"
	trap - EXIT
fi

manifest=$(tar -xOf "$archive" manifest.json)
repo_tag=$(printf '%s\n' "$manifest" | jq -er '
	if length == 1 and (.[0].RepoTags | length) == 1
	then .[0].RepoTags[0]
	else error("expected one boot tools image tag")
	end
')
config_digest=$(printf '%s\n' "$manifest" | jq -er '
	if length == 1 and (.[0].Config | test("^sha256:[0-9a-f]{64}$"))
	then .[0].Config
	else error("invalid boot tools image config digest")
	end
')
tar -tf "$archive" | grep -Fx "$config_digest" >/dev/null
actual_config_digest="sha256:$(tar -xOf "$archive" "$config_digest" | sha256sum | awk '{print $1}')"
if [ "$actual_config_digest" != "$config_digest" ]; then
	printf 'boot tools config digest = %s, filename = %s\n' "$actual_config_digest" "$config_digest" >&2
	exit 1
fi
docker load --input "$archive" >/dev/null
image_id=$(docker image inspect --format '{{.Id}}' "$repo_tag")
case "$image_id" in
	sha256:[0-9a-f][0-9a-f]*) ;;
	*)
		printf 'invalid boot tools image ID: %s\n' "$image_id" >&2
		exit 1
		;;
esac
if [ "${#image_id}" -ne 71 ]; then
	printf 'invalid boot tools image ID length: %s\n' "$image_id" >&2
	exit 1
fi
docker run --rm --platform linux/amd64 --entrypoint /bin/sh "$image_id" -ceu '
	for command in cpio depmod gzip jq sort tar unsquashfs; do
		command -v "$command" >/dev/null
	done
'

tmp_id="${image_id_file}.tmp"
trap 'rm -f "$tmp_id"' EXIT
printf '%s\n' "$image_id" >"$tmp_id"
mv "$tmp_id" "$image_id_file"
trap - EXIT
