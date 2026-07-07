#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
	printf 'not ok - %s\n' "$1" >&2
	exit 1
}

assert_contains() {
	local file="$1"
	local needle="$2"
	local label="$3"
	grep -Fq "$needle" "$file" || fail "$label: expected '$needle' in $file"
}

assert_not_contains() {
	local file="$1"
	local needle="$2"
	local label="$3"
	! grep -Fq "$needle" "$file" || fail "$label: did not expect '$needle' in $file"
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fake_root="$tmp/repo"
mkdir -p "$fake_root/dev/aws" "$fake_root/infra/aws/stacks/dev" "$fake_root/bin"
cp "$repo_root/dev/aws/run-surface-attestation.sh" "$fake_root/dev/aws/run-surface-attestation.sh"
chmod +x "$fake_root/dev/aws/run-surface-attestation.sh"

cat >"$fake_root/dev/aws/db-query.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1" >"${CAPTURED_SQL:?CAPTURED_SQL is required}"
printf 'fake db query\n'
EOF
chmod +x "$fake_root/dev/aws/db-query.sh"

cat >"$fake_root/bin/tofu" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
output_name="${@: -1}"
case "$output_name" in
	control_cluster_name) printf 'helmr-dev-control\n' ;;
	control_service_name) printf 'control\n' ;;
	dispatcher_service_name) printf 'dispatcher\n' ;;
	*) exit 1 ;;
esac
EOF
chmod +x "$fake_root/bin/tofu"

cat >"$fake_root/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "$1" != "ecs" ]; then
	exit 1
fi
case "$2" in
	describe-services)
		service=""
		while [ "$#" -gt 0 ]; do
			case "$1" in
				--services)
					service="$2"
					shift 2
					;;
				*)
					shift
					;;
			esac
		done
		case "$service" in
			control)
				cat <<'JSON'
{"services":[{"serviceName":"control","desiredCount":1,"runningCount":1,"pendingCount":0,"taskDefinition":"arn:aws:ecs:us-east-1:123456789012:task-definition/helmr-dev-control:12","deployments":[{"status":"PRIMARY","rolloutState":"COMPLETED"}]}]}
JSON
				;;
			dispatcher)
				cat <<'JSON'
{"services":[{"serviceName":"dispatcher","desiredCount":1,"runningCount":1,"pendingCount":0,"taskDefinition":"arn:aws:ecs:us-east-1:123456789012:task-definition/helmr-dev-dispatcher:34","deployments":[{"status":"PRIMARY","rolloutState":"COMPLETED"}]}]}
JSON
				;;
			*)
				exit 1
				;;
		esac
		;;
	describe-task-definition)
		task_definition=""
		while [ "$#" -gt 0 ]; do
			case "$1" in
				--task-definition)
					task_definition="$2"
					shift 2
					;;
				*)
					shift
					;;
			esac
		done
		case "$task_definition" in
			*helmr-dev-control:12)
				printf '123456789012.dkr.ecr.us-east-1.amazonaws.com/helmr/control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'
				;;
			*helmr-dev-dispatcher:34)
				printf '123456789012.dkr.ecr.us-east-1.amazonaws.com/helmr/control@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n'
				;;
			*)
				exit 1
				;;
		esac
		;;
	*)
		exit 1
		;;
esac
EOF
chmod +x "$fake_root/bin/aws"

sql_file="$tmp/sql.sql"
stdout="$tmp/stdout"
stderr="$tmp/stderr"

if PATH="$fake_root/bin:$PATH" CAPTURED_SQL="$sql_file" \
	"$fake_root/dev/aws/run-surface-attestation.sh" invalid/label >"$stdout" 2>"$stderr"; then
	fail "invalid label should fail"
fi
assert_contains "$stderr" "LABEL must contain only letters" "invalid label error"

if PATH="$fake_root/bin:$PATH" CAPTURED_SQL="$sql_file" \
	"$fake_root/dev/aws/run-surface-attestation.sh" measurement >"$stdout" 2>"$stderr"; then
	fail "surface attestation should require ECS observer opt-in"
fi
assert_contains "$stderr" "surface attestation requires" "observer opt-in error"

PATH="$fake_root/bin:$PATH" \
	CAPTURED_SQL="$sql_file" \
	HELMR_SURFACE_ATTESTATION_ALLOW_ECS_TASK=1 \
	"$fake_root/dev/aws/run-surface-attestation.sh" measurement >"$stdout" 2>"$stderr"

assert_contains "$stdout" "local_checkout" "local checkout section"
assert_contains "$stdout" "aws_ecs_service	control	control	1	1	0	helmr-dev-control:12	sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa	COMPLETED" "control ECS attestation"
assert_contains "$stdout" "aws_ecs_service	dispatcher	dispatcher	1	1	0	helmr-dev-dispatcher:34	sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb	COMPLETED" "dispatcher ECS attestation"
assert_not_contains "$stdout" "123456789012" "account id should not be printed"
assert_not_contains "$stdout" "dkr.ecr" "raw image URI should not be printed"
assert_contains "$stdout" "fake db query" "DB query should run"

assert_contains "$sql_file" "surface_setup" "setup SQL section"
assert_contains "$sql_file" "current_deployment" "deployment SQL section"
assert_contains "$sql_file" "deployment_sandbox" "sandbox SQL section"
assert_contains "$sql_file" "deployment_task" "task SQL section"
assert_contains "$sql_file" "worker_runtime_identity" "runtime SQL section"
assert_contains "$sql_file" "worker_instance" "worker SQL section"
assert_not_contains "$sql_file" "resource_id" "worker cloud resource id should not be queried"
assert_not_contains "$sql_file" "deployment_tasks.requested_execution_slots" "deployment task attestation should not query nonexistent execution slots"

printf 'ok - surface attestation tests\n'
