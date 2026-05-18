#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

fail=0

check_id_token_permissions() {
	awk '
		function fail_at(file, line_no, message) {
			printf "%s:%d: %s\n", file, line_no, message > "/dev/stderr"
			failed = 1
		}

		function detects_id_token_grant(line) {
			detected_grant = ""
			if (line ~ /^[[:space:]]*#/) {
				return 0
			}
			if (line ~ /(^|[[:space:],{])["\047]?id-token["\047]?[[:space:]]*:[[:space:]]*["\047]?write["\047]?([[:space:]#,}]|$)/) {
				detected_grant = "id-token: write"
				return 1
			}
			if (line ~ /(^|[[:space:]])["\047]?permissions["\047]?[[:space:]]*:[[:space:]]*["\047]?write-all["\047]?([[:space:]#]|$)/) {
				detected_grant = "permissions: write-all"
				return 1
			}
			return 0
		}

		function finalize_job(i) {
			if (current_job != "" && id_token_count > 0 && !job_environment_release) {
				for (i = 1; i <= id_token_count; i++) {
					fail_at(id_token_file[i], id_token_line[i], id_token_grant[i] " in job \"" current_job "\" must be protected by environment: release-production")
				}
			}
			current_job = ""
			job_environment_release = 0
			id_token_count = 0
			in_environment_block = 0
		}

		FNR == 1 {
			if (NR > 1) {
				finalize_job()
			}
			in_jobs = 0
			current_job = ""
			job_environment_release = 0
			id_token_count = 0
			in_environment_block = 0
		}

		{
			line = $0
			sub(/\r$/, "", line)

			if (line ~ /^jobs:[[:space:]]*(#.*)?$/) {
				finalize_job()
				in_jobs = 1
				next
			}

			if (in_jobs && line ~ /^[^[:space:]#][^:]*:/) {
				finalize_job()
				in_jobs = 0
			}

			if (in_jobs && line ~ /^  ["\047]?[A-Za-z0-9_][A-Za-z0-9_-]*["\047]?[[:space:]]*:[[:space:]]*(#.*)?$/) {
				finalize_job()
				current_job = line
				sub(/^  /, "", current_job)
				sub(/^["\047]/, "", current_job)
				sub(/:.*/, "", current_job)
				sub(/["\047][[:space:]]*$/, "", current_job)
			}

			if (current_job != "") {
				if (line ~ /^    ["\047]?environment["\047]?[[:space:]]*:[[:space:]]*["\047]?release-production["\047]?([[:space:]#]|$)/) {
					job_environment_release = 1
					in_environment_block = 0
				} else if (line ~ /^    ["\047]?environment["\047]?[[:space:]]*:[[:space:]]*(#.*)?$/) {
					in_environment_block = 1
				} else if (in_environment_block && line ~ /^      ["\047]?name["\047]?[[:space:]]*:[[:space:]]*["\047]?release-production["\047]?([[:space:]#]|$)/) {
					job_environment_release = 1
				} else if (in_environment_block && line !~ /^      / && line !~ /^[[:space:]]*$/) {
					in_environment_block = 0
				}
			}

			if (detects_id_token_grant(line)) {
				if (line !~ /security-check:[[:space:]]*allow-id-token/) {
					fail_at(FILENAME, FNR, detected_grant " must be explicitly marked with security-check: allow-id-token")
				}
				if (current_job == "") {
					fail_at(FILENAME, FNR, detected_grant " must be inside a workflow job protected by environment: release-production")
				} else {
					id_token_count++
					id_token_file[id_token_count] = FILENAME
					id_token_line[id_token_count] = FNR
					id_token_grant[id_token_count] = detected_grant
				}
			}
		}

		END {
			finalize_job()
			exit failed ? 1 : 0
		}
	' "$@"
}

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

if rg -n 'actions/cache' .github/workflows/release.yaml .github/workflows/boot-artifacts.yaml; then
	echo "release workflows must not use GitHub Actions cache" >&2
	fail=1
fi

if rg -n 'CACHIX_AUTH_TOKEN' .github/workflows/release.yaml .github/workflows/boot-artifacts.yaml; then
	echo "release workflows must not receive the Cachix write/auth token" >&2
	fail=1
fi

mapfile -t github_yaml_files < <(rg --files .github/workflows .github/actions -g '*.yaml' -g '*.yml')
if ((${#github_yaml_files[@]} > 0)) && ! check_id_token_permissions "${github_yaml_files[@]}"; then
	echo "id-token: write must be explicitly marked and protected by release-production" >&2
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
