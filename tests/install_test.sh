#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

assert_file() {
  local path="$1"
  [ -f "$path" ] || fail "expected file $path"
}

assert_not_exists() {
  local path="$1"
  [ ! -e "$path" ] || fail "expected $path to be absent"
}

assert_equal() {
  local expected="$1"
  local actual="$2"
  local label="$3"
  [ "$actual" = "$expected" ] || fail "$label: expected '$expected', got '$actual'"
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  else
    fail "sha256sum or shasum is required"
  fi
}

write_helmr_binary() {
  local path="$1"
  local version="${2:-local}"
  printf '#!/usr/bin/env sh\nprintf "%s\\n"\n' "$version" > "$path"
  chmod +x "$path"
}

test_binary_install_copies_only_binary() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/source" "$tmp/home"
  write_helmr_binary "$tmp/source/helmr"

  HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --binary "$tmp/source/helmr" --no-modify-path >/dev/null

  assert_file "$tmp/install/helmr"
  assert_not_exists "$tmp/install/adapter"
}

test_binary_install_removes_existing_sidecar_adapter() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/source" "$tmp/home" "$tmp/install/adapter"
  write_helmr_binary "$tmp/source/helmr"
  printf 'old adapter\n' > "$tmp/install/adapter/main.js"

  HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --binary "$tmp/source/helmr" --no-modify-path >/dev/null

  assert_file "$tmp/install/helmr"
  assert_not_exists "$tmp/install/adapter"
}

test_binary_install_accepts_missing_sidecar_adapter() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/source/adapter" "$tmp/home"
  write_helmr_binary "$tmp/source/helmr"
  printf 'stale sidecar\n' > "$tmp/source/adapter/main.js"

  HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --binary "$tmp/source/helmr" --no-modify-path >/dev/null

  assert_file "$tmp/install/helmr"
  assert_not_exists "$tmp/install/adapter"
}

write_release_fixture() {
  local tmp="$1"
  mkdir -p "$tmp/source"
  write_helmr_binary "$tmp/source/helmr" "v9.8.7"
  tar -C "$tmp/source" -czf "$tmp/helmr-linux-amd64.tar.gz" helmr
  printf '%s  helmr-linux-amd64.tar.gz\n' "$(sha256_file "$tmp/helmr-linux-amd64.tar.gz")" > "$tmp/checksums.txt"
}

write_uname_stub() {
  local tmp="$1"
  cat > "$tmp/stub-bin/uname" <<'SH'
#!/usr/bin/env sh
case "$1" in
  -s) printf 'Linux\n' ;;
  -m) printf 'x86_64\n' ;;
  *) exit 1 ;;
esac
SH
  chmod +x "$tmp/stub-bin/uname"
}

write_release_curl_stub() {
  local tmp="$1"
  local include_releases_api="${2:-false}"
  cat > "$tmp/stub-bin/curl" <<SH
#!/usr/bin/env sh
out=""
url=""
while [ "\$#" -gt 0 ]; do
  case "\$1" in
    -o)
      out="\$2"
      shift 2
      ;;
    -*)
      shift
      ;;
    *)
      url="\$1"
      shift
      ;;
  esac
done
case "\$url" in
  "https://api.github.com/repos/helmrdotdev/helmr/releases?per_page=100")
    if [ "$include_releases_api" = "true" ]; then
      cat "$tmp/releases.json"
    else
      printf 'unexpected url: %s\n' "\$url" >&2
      exit 1
    fi
    ;;
  "https://github.com/helmrdotdev/helmr/releases/download/v9.8.7/helmr-linux-amd64.tar.gz")
    printf '%s\n' "\$url" > "$tmp/download-url"
    cp "$tmp/helmr-linux-amd64.tar.gz" "\$out"
    ;;
  "https://github.com/helmrdotdev/helmr/releases/download/v9.8.7/checksums.txt")
    cp "$tmp/checksums.txt" "\$out"
    ;;
  *)
    printf 'unexpected url: %s\n' "\$url" >&2
    exit 1
    ;;
esac
SH
  chmod +x "$tmp/stub-bin/curl"
}

test_latest_release_skips_non_cli_release() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/stub-bin" "$tmp/home"
  write_release_fixture "$tmp"
  cat > "$tmp/releases.json" <<'JSON'
[
  {
    "tag_name": "boot-artifacts-v0",
    "draft": false,
    "prerelease": false,
    "assets": [
      {
        "name": "guest-vmlinuz"
      },
      {
        "name": "checksums.txt"
      }
    ]
  },
  {
    "tag_name": "v9.8.7",
    "draft": false,
    "prerelease": false,
    "assets": [
      {
        "name": "helmr-linux-amd64.tar.gz"
      },
      {
        "name": "checksums.txt"
      }
    ]
  }
]
JSON
  write_uname_stub "$tmp"
  write_release_curl_stub "$tmp" true

  PATH="$tmp/stub-bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --no-modify-path >/dev/null

  assert_equal "https://github.com/helmrdotdev/helmr/releases/download/v9.8.7/helmr-linux-amd64.tar.gz" "$(cat "$tmp/download-url")" "download url"
  assert_file "$tmp/install/helmr"
  assert_not_exists "$tmp/install/adapter"
}

test_same_version_elsewhere_on_path_does_not_skip_install() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/stub-bin" "$tmp/home"
  write_helmr_binary "$tmp/stub-bin/helmr" "v9.8.7"
  write_release_fixture "$tmp"
  write_uname_stub "$tmp"
  write_release_curl_stub "$tmp"

  PATH="$tmp/stub-bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --version v9.8.7 --no-modify-path >/dev/null

  assert_equal "https://github.com/helmrdotdev/helmr/releases/download/v9.8.7/helmr-linux-amd64.tar.gz" "$(cat "$tmp/download-url")" "download url"
  assert_file "$tmp/install/helmr"
  assert_not_exists "$tmp/install/adapter"
}

test_same_version_installed_path_removes_existing_sidecar_adapter() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/install/adapter" "$tmp/home"
  write_helmr_binary "$tmp/install/helmr" "v9.8.7"
  printf 'old adapter\n' > "$tmp/install/adapter/main.js"

  PATH="$tmp/install:/usr/bin:/bin:/usr/sbin:/sbin" \
    HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --version v9.8.7 --no-modify-path >/dev/null

  assert_file "$tmp/install/helmr"
  assert_not_exists "$tmp/install/adapter"
}

test_path_snippet_quotes_install_dir_and_handles_spaced_home() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/source" "$tmp/home with spaces"
  : > "$tmp/home with spaces/.profile"
  write_helmr_binary "$tmp/source/helmr"

  HELMR_INSTALL_DIR="$tmp/install dir" \
    HOME="$tmp/home with spaces" \
    SHELL=/bin/sh \
    "$repo_root/install" --binary "$tmp/source/helmr" >/dev/null

  assert_equal "export PATH='$tmp/install dir':\$PATH" "$(tail -n 1 "$tmp/home with spaces/.profile")" "shell path snippet"
}

test_binary_install_copies_only_binary
test_binary_install_accepts_missing_sidecar_adapter
test_binary_install_removes_existing_sidecar_adapter
test_latest_release_skips_non_cli_release
test_same_version_elsewhere_on_path_does_not_skip_install
test_same_version_installed_path_removes_existing_sidecar_adapter
test_path_snippet_quotes_install_dir_and_handles_spaced_home
printf 'ok - installer tests\n'
