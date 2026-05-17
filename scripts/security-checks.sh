#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

fail=0

if ! command -v rg >/dev/null 2>&1; then
	echo "ripgrep (rg) is required for security checks" >&2
	exit 1
fi

if ! command -v zizmor >/dev/null 2>&1; then
	echo "zizmor is required for security checks" >&2
	exit 1
fi

zizmor --format plain --min-severity low .github/workflows .github/actions

if rg -n "pull_request_target" .github/workflows .github/actions; then
	echo "pull_request_target is not allowed without a focused security review" >&2
	fail=1
fi

while IFS= read -r line; do
	target=${line#*uses:}
	target=${target%%#*}
	target=${target#"${target%%[![:space:]]*}"}
	target=${target%"${target##*[![:space:]]}"}
	target=${target#\"}
	target=${target%\"}
	target=${target#\'}
	target=${target%\'}

	case "$target" in
		"" | ./* | ../*)
			continue
			;;
		docker://*)
			ref=${target##*@}
			if [[ $ref == "$target" || ! $ref =~ ^sha256:[0-9a-f]{64}$ ]]; then
				printf 'external Docker Action must be pinned to a sha256 digest: %s\n' "$line" >&2
				fail=1
			fi
			continue
			;;
	esac

	ref=${target##*@}
	if [[ $ref == "$target" || ! $ref =~ ^[0-9a-f]{40}$ ]]; then
		printf 'external GitHub Action must be pinned to a full commit SHA: %s\n' "$line" >&2
		fail=1
	fi
done < <(rg -n 'uses:[[:space:]]*[^[:space:]]+' .github/workflows .github/actions || true)

if rg -n 'toJSON\([[:space:]]*secrets[[:space:]]*\)|\$\{\{[[:space:]]*secrets[[:space:]]*\}\}' .github/workflows .github/actions; then
	echo "dumping the full GitHub secrets context is not allowed" >&2
	fail=1
fi

if rg -n 'bun[[:space:]]+install([^#\n]*)' scripts .github/workflows .github/actions infra/aws/modules | rg -v -- '--ignore-scripts'; then
	echo "automation and provisioning bun installs must use --ignore-scripts to avoid dependency lifecycle execution" >&2
	fail=1
fi

if rg -n '(curl|wget)[^|;&]*\|[[:space:]]*(sh|bash)' .github/workflows .github/actions; then
	echo "piping downloaded scripts into a shell is not allowed in workflows" >&2
	fail=1
fi

exit "$fail"
