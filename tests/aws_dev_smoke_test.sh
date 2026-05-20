#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="$repo_root/scripts/aws-dev-smoke.sh"

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

assert_equal() {
  local expected="$1"
  local actual="$2"
  local label="$3"
  [ "$actual" = "$expected" ] || fail "$label: expected '$expected', got '$actual'"
}

assert_tfvar_count() {
  local file="$1"
  local key="$2"
  local expected="$3"
  local label="$4"
  local actual
  actual="$(grep -Ec "^[[:space:]]*${key}[[:space:]]*=" "$file" || true)"
  [ "$actual" = "$expected" ] || fail "$label: expected $expected assignments for $key, got $actual"
}

write_tfvars() {
  local file="$1"
  local public_url="$2"
  local certificate_arn="$3"
  local enable_cloudfront="$4"
  local cloudfront_origin="$5"

  cat >"$file" <<EOF
public_url = $public_url
certificate_arn = $certificate_arn
enable_cloudfront = $enable_cloudfront
cloudfront_origin_domain_name = $cloudfront_origin
EOF
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

tfvars="$tmp/dev.tfvars"
stdout="$tmp/stdout"
stderr="$tmp/stderr"

write_tfvars "$tfvars" '"http://localhost"' null false null
if WORKER_AMI_ID=ami-0123456789abcdef0 DEV_TFVARS="$tfvars" "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"; then
  fail "dev-worker-tfvars should require certificate_arn before enabling workers"
fi
assert_contains "$stderr" "requires DEV_CERTIFICATE_ARN or an existing certificate_arn tfvar" "missing certificate guard"

write_tfvars "$tfvars" '"http://localhost"' '"arn:aws:acm:us-east-1:123456789012:certificate/example"' false null
if WORKER_AMI_ID=ami-0123456789abcdef0 DEV_TFVARS="$tfvars" "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"; then
  fail "dev-worker-tfvars should reject loopback public_url"
fi
assert_contains "$stderr" "requires public_url to use a non-loopback hostname" "loopback public_url guard"

write_tfvars "$tfvars" '"https://viewer.example.com"' '"arn:aws:acm:us-east-1:123456789012:certificate/example"' true '"localhost"'
if WORKER_AMI_ID=ami-0123456789abcdef0 DEV_TFVARS="$tfvars" "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"; then
  fail "dev-worker-tfvars should reject loopback CloudFront origin"
fi
assert_contains "$stderr" "requires cloudfront_origin_domain_name to use a non-loopback hostname" "loopback cloudfront origin guard"

write_tfvars "$tfvars" '"https://viewer.example.com"' '"arn:aws:acm:us-east-1:123456789012:certificate/example"' true '"https://origin.example.com:443/path"'
if WORKER_AMI_ID=ami-0123456789abcdef0 DEV_TFVARS="$tfvars" "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"; then
  fail "dev-worker-tfvars should reject URL-shaped CloudFront origin"
fi
assert_contains "$stderr" "requires cloudfront_origin_domain_name to be a DNS hostname without scheme, path, or port" "URL-shaped cloudfront origin guard"

write_tfvars "$tfvars" '"http://localhost"' null false null
WORKER_AMI_ID=ami-0123456789abcdef0 \
  DEV_TFVARS="$tfvars" \
  DEV_PUBLIC_URL=https://control.example.com \
  DEV_CERTIFICATE_ARN=arn:aws:acm:us-east-1:123456789012:certificate/example \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'public_url = "https://control.example.com"' "public URL override"
assert_contains "$tfvars" 'certificate_arn = "arn:aws:acm:us-east-1:123456789012:certificate/example"' "certificate override"
assert_contains "$tfvars" 'create_worker = true' "worker enabled"

cat >"$tfvars" <<'EOF'
public_url="http://localhost"
certificate_arn=null
enable_cloudfront=false
cloudfront_origin_domain_name=null
EOF
WORKER_AMI_ID=ami-0123456789abcdef0 \
  DEV_TFVARS="$tfvars" \
  DEV_PUBLIC_URL=https://compact.example.com \
  DEV_CERTIFICATE_ARN=arn:aws:acm:us-east-1:123456789012:certificate/example \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'public_url = "https://compact.example.com"' "compact public URL replacement"
assert_contains "$tfvars" 'certificate_arn = "arn:aws:acm:us-east-1:123456789012:certificate/example"' "compact certificate replacement"
assert_tfvar_count "$tfvars" public_url 1 "compact public URL replacement should not duplicate"
assert_tfvar_count "$tfvars" certificate_arn 1 "compact certificate replacement should not duplicate"

cat >"$tfvars" <<'EOF'
aws_region="us-west-2"
name="compact-smoke"
public_url="https://old.example.com"
control_url="https://stale.example.com"
worker_control_url="https://worker-stale.example.com"
enable_private_control_dns=true
create_worker=false
EOF
DEV_TFVARS="$tfvars" \
  AWS_REGION=us-west-2 \
  DEV_CONTROL_IMAGE=123456789012.dkr.ecr.us-west-2.amazonaws.com/helmr-control:test \
  DEV_PUBLIC_URL=https://replacement.example.com \
  DEV_GITHUB_APP_ID=12345 \
  DEV_GITHUB_APP_SLUG=helmr-smoke \
  DEV_GITHUB_APP_CLIENT_ID=Iv1.example \
  DEV_BOOTSTRAP_OWNER_EMAIL=owner@example.com \
  "$script" dev-control-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'public_url = "https://replacement.example.com"' "compact tfvar replacement"
assert_tfvar_count "$tfvars" public_url 1 "compact tfvar replacement should not duplicate"
assert_tfvar_count "$tfvars" control_url 0 "compact control_url removal"
assert_tfvar_count "$tfvars" worker_control_url 0 "compact worker_control_url removal"
assert_tfvar_count "$tfvars" enable_private_control_dns 0 "compact enable_private_control_dns removal"
assert_not_contains "$tfvars" "https://old.example.com" "compact old value removal"

mkdir -p "$tmp/bin"
cat >"$tmp/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

cat "$MOCK_IMAGE_JSON"
EOF
chmod +x "$tmp/bin/aws"

state_dir="$tmp/state"
mkdir -p "$state_dir"
MOCK_IMAGE_JSON="$tmp/image-missing-region.json"
cat >"$MOCK_IMAGE_JSON" <<'JSON'
{
  "image": {
    "state": {
      "status": "AVAILABLE"
    },
    "outputResources": {
      "amis": [
        {
          "region": "us-east-1",
          "image": "ami-0aaaaaaaaaaaaaaaa"
        }
      ]
    }
  }
}
JSON

if AWS_REGION=us-west-2 STATE_DIR="$state_dir" MOCK_IMAGE_JSON="$MOCK_IMAGE_JSON" PATH="$tmp/bin:$PATH" "$script" worker-image-wait arn:aws:imagebuilder:us-west-2:123456789012:image/example/1.0.0/1 >"$stdout" 2>"$stderr"; then
  fail "worker-image-wait should fail when AWS_REGION is absent from Image Builder AMIs"
fi
assert_contains "$stderr" "does not include an AMI for AWS_REGION=us-west-2" "missing AMI region guard"

MOCK_IMAGE_JSON="$tmp/image-current-region.json"
cat >"$MOCK_IMAGE_JSON" <<'JSON'
{
  "image": {
    "state": {
      "status": "AVAILABLE"
    },
    "outputResources": {
      "amis": [
        {
          "region": "us-east-1",
          "image": "ami-0aaaaaaaaaaaaaaaa"
        },
        {
          "region": "us-west-2",
          "image": "ami-0bbbbbbbbbbbbbbbb"
        }
      ]
    }
  }
}
JSON

AWS_REGION=us-west-2 STATE_DIR="$state_dir" MOCK_IMAGE_JSON="$MOCK_IMAGE_JSON" PATH="$tmp/bin:$PATH" "$script" worker-image-wait arn:aws:imagebuilder:us-west-2:123456789012:image/example/1.0.0/1 >"$stdout" 2>"$stderr"
assert_equal "ami-0bbbbbbbbbbbbbbbb" "$(cat "$stdout")" "worker-image-wait current region AMI"
assert_equal "ami-0bbbbbbbbbbbbbbbb" "$(cat "$state_dir/worker-ami-id")" "recorded worker AMI"

printf 'ok - aws dev smoke tests\n'
