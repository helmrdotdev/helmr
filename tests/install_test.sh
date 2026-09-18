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
}

write_release_fixture() {
  local tmp="$1"
  mkdir -p "$tmp/source"
  write_helmr_binary "$tmp/source/helmr" "v9.8.7 (0123456789abcdef0123456789abcdef01234567)"
  tar -C "$tmp/source" -czf "$tmp/helmr-linux-amd64.tar.gz" helmr
  PYTHONPATH="$repo_root/scripts/release" python3 - "$tmp" <<'PYCODE'
import json,sys,shutil
from pathlib import Path
from contract import ASSETS,PLATFORMS,canonical,cli_checksums,descriptor
root=Path(sys.argv[1])
for p in PLATFORMS:
    if p!='linux-amd64':shutil.copyfile(root/'helmr-linux-amd64.tar.gz',root/f'helmr-{p}.tar.gz')
(root/'checksums.txt').write_bytes(cli_checksums(root))
(root/'release-index.json').write_bytes(canonical(dict(schema='helmr.release.v0',version='v9.8.7',assets={'checksums.txt':descriptor(root/'checksums.txt')})))
# Discovery inventory comes from the producer contract, not a parallel list.
(root/'releases.json').write_text(json.dumps([dict(tag_name='v9.9.0-preview.gabcdef01.b2',draft=False,prerelease=True,assets=[]),dict(tag_name='v9.8.7',draft=False,prerelease=False,assets=[dict(name=n) for n in sorted(ASSETS|{'release-index.json','release-index.sigstore.json'})])],indent=2))
PYCODE
}

test_canonical_version_at_target_skips_install() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/install" "$tmp/home" "$tmp/stub-bin"
  write_helmr_binary "$tmp/install/helmr" "v9.8.7 (0123456789abcdef0123456789abcdef01234567)"
  write_uname_stub "$tmp"

  PATH="$tmp/install:$tmp/stub-bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --version v9.8.7 --no-modify-path >/dev/null
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
headers=""
while [ "\$#" -gt 0 ]; do
  case "\$1" in
    -D)
      headers="\$2"
      shift 2
      ;;
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
  "https://api.github.com/repos/helmrdotdev/helmr/releases?per_page=100&page="*)
    if [ "$include_releases_api" = "true" ]; then
      page="\${url##*page=}"
      printf '%s\n' "\$page" >>"$tmp/api-requests"
      [ ! -e "$tmp/fail-page-\$page" ] || exit 22
      : >"\$headers"
      if [ -f "$tmp/headers-page-\$page" ]; then cat "$tmp/headers-page-\$page" >"\$headers"; fi
      if [ -f "$tmp/releases-page-\$page.json" ]; then cat "$tmp/releases-page-\$page.json"
      elif [ "\$page" = 1 ]; then cat "$tmp/releases.json"
      else printf '[]\n'; fi
    else
      printf 'unexpected url: %s\n' "\$url" >&2
      exit 1
    fi
    ;;
  "https://github.com/helmrdotdev/helmr/releases/download/v9.8.7/helmr-linux-amd64.tar.gz")
    printf '%s\n' "\$url" > "$tmp/download-url"
    cp "$tmp/helmr-linux-amd64.tar.gz" "\$out"
    ;;
  "https://github.com/helmrdotdev/helmr/releases/download/v9.8.7/release-index.json")
    cp "$tmp/release-index.json" "\$out"
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
  write_uname_stub "$tmp"
  write_release_curl_stub "$tmp" true

  PATH="$tmp/stub-bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --no-modify-path >/dev/null

  assert_equal "https://github.com/helmrdotdev/helmr/releases/download/v9.8.7/helmr-linux-amd64.tar.gz" "$(cat "$tmp/download-url")" "download url"
  assert_file "$tmp/install/helmr"
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

test_indexed_checksum_and_archive_tampering() {
  local tmp fault
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/stub-bin" "$tmp/home"
  write_uname_stub "$tmp"
  write_release_curl_stub "$tmp"
  for fault in checksum archive; do
    write_release_fixture "$tmp"
    if [ "$fault" = checksum ]; then printf corrupt >>"$tmp/checksums.txt"; else printf corrupt >>"$tmp/helmr-linux-amd64.tar.gz"; fi
    if PATH="$tmp/stub-bin:/usr/bin:/bin:/usr/sbin:/sbin" HELMR_INSTALL_DIR="$tmp/install" HOME="$tmp/home" SHELL=/bin/sh \
      "$repo_root/install" --version v9.8.7 --no-modify-path >"$tmp/out" 2>&1; then fail "$fault tamper accepted"; fi
    [ ! -e "$tmp/install/helmr" ] || fail 'tampered archive installed'
  done
}

test_latest_release_pagination() {
  local tmp scenario
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/stub-bin" "$tmp/home"
  write_release_fixture "$tmp"
  write_uname_stub "$tmp"
  write_release_curl_stub "$tmp" true
  python3 - "$tmp" <<'PYCODE'
import json,sys
from pathlib import Path
root=Path(sys.argv[1]);stable=json.loads((root/'releases.json').read_text())[-1]
previews=[dict(stable,tag_name=f'v9.9.0-preview.gabcdef01.b{i}',prerelease=True) for i in range(1,101)]
(root/'releases-page-1.json').write_text(json.dumps(previews,indent=2))
(root/'stable.json').write_text(json.dumps([stable],indent=2))
PYCODE
  printf '%s\n' 'Link: <https://api.github.com/repos/helmrdotdev/helmr/releases?per_page=100&page=2>; rel="next"' >"$tmp/headers-page-1"
  for scenario in stable exhausted transport-error; do
    rm -rf "$tmp/install"
    rm -f "$tmp/api-requests" "$tmp/download-url" "$tmp/fail-page-2"
    if [ "$scenario" = stable ]; then cp "$tmp/stable.json" "$tmp/releases-page-2.json"
    else printf '[]\n' >"$tmp/releases-page-2.json"; fi
    if [ "$scenario" = transport-error ]; then : >"$tmp/fail-page-2"; fi
    if PATH="$tmp/stub-bin:/usr/bin:/bin:/usr/sbin:/sbin" HELMR_INSTALL_DIR="$tmp/install" HOME="$tmp/home" SHELL=/bin/sh \
      "$repo_root/install" --no-modify-path >"$tmp/$scenario.out" 2>&1; then
      [ "$scenario" = stable ] || fail "$scenario must fail without a stable release"
      assert_file "$tmp/install/helmr"
    else
      [ "$scenario" != stable ] || fail 'stable release on page two was missed'
      [ ! -e "$tmp/install/helmr" ] && [ ! -e "$tmp/download-url" ] || fail 'unsuccessful listing downloaded a release'
      if [ "$scenario" = exhausted ]; then
        grep -F 'Failed to resolve latest' "$tmp/$scenario.out" >/dev/null || fail 'exhausted listing did not terminate normally'
      else
        if grep -F 'Failed to resolve latest' "$tmp/$scenario.out" >/dev/null; then fail 'transport failure was treated as exhaustion'; fi
      fi
    fi
    assert_equal "$(printf '1\n2')" "$(cat "$tmp/api-requests")" "$scenario native pages"
  done
}

write_preview_fixture() {
  local tmp="$1"
  mkdir -p "$tmp/source"
  write_helmr_binary "$tmp/source/helmr" "v0.1.0-preview.gabcdef01.b2 (0123456789abcdef0123456789abcdef01234567)"
  tar -C "$tmp/source" -czf "$tmp/helmr-linux-amd64.tar.gz" helmr
  PYTHONPATH="$repo_root/scripts/release" python3 - "$tmp" <<'PYCODE'
import sys, shutil
from pathlib import Path
from contract import PLATFORMS, canonical, cli_checksums, descriptor
root=Path(sys.argv[1])
for p in PLATFORMS:
    if p != 'linux-amd64':
        shutil.copyfile(root/'helmr-linux-amd64.tar.gz', root/f'helmr-{p}.tar.gz')
(root/'checksums.txt').write_bytes(cli_checksums(root))
(root/'release-index.json').write_bytes(canonical(dict(
    schema='helmr.release.v0', version='v0.1.0-preview.gabcdef01.b2',
    assets={'checksums.txt': descriptor(root/'checksums.txt')})))
PYCODE
}

write_preview_curl_stub() {
  local tmp="$1"
  local forbid="${2:-false}"
  cat > "$tmp/stub-bin/curl" <<SH
#!/usr/bin/env sh
out=""
url=""
while [ "\$#" -gt 0 ]; do
  case "\$1" in
    -o) out="\$2"; shift 2 ;;
    -w) shift 2 ;;
    --proto|--max-redirs) shift 2 ;;
    -*) shift ;;
    *) url="\$1"; shift ;;
  esac
done
code=200
case "\$url" in
  *"/release-index.json")
    printf 'release-index\n' >>"$tmp/order"
    cp "$tmp/release-index.json" "\$out"
    ;;
  *"/checksums.txt")
    if [ "$forbid" = true ]; then code=403; : >"\$out"; else
      printf 'checksums\n' >>"$tmp/order"
      cp "$tmp/checksums.txt" "\$out"
    fi
    ;;
  *"/helmr-linux-amd64.tar.gz")
    printf 'archive\n' >>"$tmp/order"
    cp "$tmp/helmr-linux-amd64.tar.gz" "\$out"
    ;;
  *)
    printf 'unexpected url: %s\n' "\$url" >&2
    exit 1
    ;;
esac
printf '%s' "\$code"
SH
  chmod +x "$tmp/stub-bin/curl"
}

test_preview_install_validates_index_before_archive() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/stub-bin" "$tmp/home"
  : >"$tmp/order"
  write_preview_fixture "$tmp"
  write_uname_stub "$tmp"
  write_preview_curl_stub "$tmp" false
  PATH="$tmp/stub-bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    HELMR_INSTALL_DIR="$tmp/install" \
    HOME="$tmp/home" \
    SHELL=/bin/sh \
    "$repo_root/install" --version v0.1.0-preview.gabcdef01.b2 --no-modify-path >/dev/null
  assert_equal "$(printf 'checksums\nrelease-index\narchive')" "$(cat "$tmp/order")" "preview fetch order"
  assert_file "$tmp/install/helmr"
}

test_preview_install_reports_http_status_without_set_e_trap() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/stub-bin" "$tmp/home"
  write_preview_fixture "$tmp"
  write_uname_stub "$tmp"
  write_preview_curl_stub "$tmp" true
  if PATH="$tmp/stub-bin:/usr/bin:/bin:/usr/sbin:/sbin" HELMR_INSTALL_DIR="$tmp/install" HOME="$tmp/home" SHELL=/bin/sh \
    "$repo_root/install" --version v0.1.0-preview.gabcdef01.b2 --no-modify-path >"$tmp/out" 2>&1; then
    fail 'preview HTTP 403 should fail install'
  fi
  grep -F 'Preview download failed: HTTP 403' "$tmp/out" >/dev/null || fail 'friendly preview HTTP error missing'
}

test_binary_install_copies_only_binary
test_canonical_version_at_target_skips_install
test_latest_release_skips_non_cli_release
test_same_version_elsewhere_on_path_does_not_skip_install
test_path_snippet_quotes_install_dir_and_handles_spaced_home
test_indexed_checksum_and_archive_tampering
test_latest_release_pagination
test_preview_install_validates_index_before_archive
test_preview_install_reports_http_status_without_set_e_trap
printf 'ok - installer tests\n'
