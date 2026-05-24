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

write_adapter_files() {
  local dir="$1"
  mkdir -p "$dir/adapter"
  printf 'main\n' > "$dir/adapter/main.js"
  printf 'register\n' > "$dir/adapter/register.mjs"
  printf 'loader\n' > "$dir/adapter/loader.mjs"
}

test_binary_install_copies_adapter() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/source" "$tmp/home"
  printf '#!/usr/bin/env sh\nprintf "local\\n"\n' > "$tmp/source/helmr"
  chmod +x "$tmp/source/helmr"
  write_adapter_files "$tmp/source"

  HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --binary "$tmp/source/helmr" --no-modify-path >/dev/null

  assert_file "$tmp/install/helmr"
  assert_equal "main" "$(cat "$tmp/install/adapter/main.js")" "adapter main"
  assert_equal "register" "$(cat "$tmp/install/adapter/register.mjs")" "adapter register"
  assert_equal "loader" "$(cat "$tmp/install/adapter/loader.mjs")" "adapter loader"
}

test_binary_install_rejects_incomplete_adapter() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/source/adapter" "$tmp/home"
  printf '#!/usr/bin/env sh\nprintf "local\\n"\n' > "$tmp/source/helmr"
  chmod +x "$tmp/source/helmr"
  printf 'main\n' > "$tmp/source/adapter/main.js"

  if HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --binary "$tmp/source/helmr" --no-modify-path >"$tmp/stdout" 2>"$tmp/stderr"; then
    fail "expected incomplete adapter install to fail"
  fi
  grep -F "adapter/register.mjs was not found" "$tmp/stdout" "$tmp/stderr" >/dev/null ||
    fail "expected missing register error"
}

test_latest_release_skips_non_cli_release() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/stub-bin" "$tmp/source" "$tmp/home"
  printf '#!/usr/bin/env sh\nprintf "v9.8.7\\n"\n' > "$tmp/source/helmr"
  chmod +x "$tmp/source/helmr"
  write_adapter_files "$tmp/source"
  tar -C "$tmp/source" -czf "$tmp/helmr-linux-amd64.tar.gz" helmr adapter
  printf '%s  helmr-linux-amd64.tar.gz\n' "$(sha256_file "$tmp/helmr-linux-amd64.tar.gz")" > "$tmp/checksums.txt"

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

  cat > "$tmp/stub-bin/uname" <<'SH'
#!/usr/bin/env sh
case "$1" in
  -s) printf 'Linux\n' ;;
  -m) printf 'x86_64\n' ;;
  *) exit 1 ;;
esac
SH
  chmod +x "$tmp/stub-bin/uname"

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
    cat "$tmp/releases.json"
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

  PATH="$tmp/stub-bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --no-modify-path >/dev/null

  assert_equal "https://github.com/helmrdotdev/helmr/releases/download/v9.8.7/helmr-linux-amd64.tar.gz" "$(cat "$tmp/download-url")" "download url"
  assert_equal "main" "$(cat "$tmp/install/adapter/main.js")" "adapter main"
}

test_same_version_elsewhere_on_path_does_not_skip_install() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/stub-bin" "$tmp/source" "$tmp/home"
  printf '#!/usr/bin/env sh\nprintf "v9.8.7\\n"\n' > "$tmp/stub-bin/helmr"
  chmod +x "$tmp/stub-bin/helmr"
  printf '#!/usr/bin/env sh\nprintf "v9.8.7\\n"\n' > "$tmp/source/helmr"
  chmod +x "$tmp/source/helmr"
  write_adapter_files "$tmp/source"
  tar -C "$tmp/source" -czf "$tmp/helmr-linux-amd64.tar.gz" helmr adapter
  printf '%s  helmr-linux-amd64.tar.gz\n' "$(sha256_file "$tmp/helmr-linux-amd64.tar.gz")" > "$tmp/checksums.txt"

  cat > "$tmp/stub-bin/uname" <<'SH'
#!/usr/bin/env sh
case "$1" in
  -s) printf 'Linux\n' ;;
  -m) printf 'x86_64\n' ;;
  *) exit 1 ;;
esac
SH
  chmod +x "$tmp/stub-bin/uname"

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

  PATH="$tmp/stub-bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --version v9.8.7 --no-modify-path >/dev/null

  assert_equal "https://github.com/helmrdotdev/helmr/releases/download/v9.8.7/helmr-linux-amd64.tar.gz" "$(cat "$tmp/download-url")" "download url"
  assert_file "$tmp/install/helmr"
  assert_equal "main" "$(cat "$tmp/install/adapter/main.js")" "adapter main"
}

test_path_snippet_quotes_install_dir_and_handles_spaced_home() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/source" "$tmp/home with spaces"
  : > "$tmp/home with spaces/.profile"
  printf '#!/usr/bin/env sh\nprintf "local\\n"\n' > "$tmp/source/helmr"
  chmod +x "$tmp/source/helmr"
  write_adapter_files "$tmp/source"

  HELMR_INSTALL_DIR="$tmp/install dir" \
    HOME="$tmp/home with spaces" \
    SHELL=/bin/sh \
    "$repo_root/install" --binary "$tmp/source/helmr" >/dev/null

  assert_equal "export PATH='$tmp/install dir':\$PATH" "$(tail -n 1 "$tmp/home with spaces/.profile")" "shell path snippet"
}

test_binary_install_copies_adapter
test_binary_install_rejects_incomplete_adapter
test_latest_release_skips_non_cli_release
test_same_version_elsewhere_on_path_does_not_skip_install
test_path_snippet_quotes_install_dir_and_handles_spaced_home
printf 'ok - installer tests\n'
