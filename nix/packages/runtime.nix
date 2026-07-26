{
  lib,
  stdenv,
  stdenvNoCC,
  nodejs_24,
  squashfsTools,
  glibc,
  patchelf,
  pax-utils,
  coreutils,
  findutils,
  gnugrep,
  gnused,
  file,
}:

let
  target = {
    architecture = "x86_64";
    loader = "ld-linux-x86-64.so.2";
  };

in
assert lib.assertMsg stdenv.hostPlatform.isx86_64 "managed runtime supports only x86_64-linux";
assert lib.assertMsg (
  nodejs_24.version == "24.16.0"
) "managed runtime requires nodejs_24 24.16.0, got ${nodejs_24.version}";
stdenvNoCC.mkDerivation {
  pname = "helmr-managed-runtime-${target.architecture}";
  version = "0";
  dontUnpack = true;
  dontPatchELF = true;
  dontStrip = true;
  strictDeps = true;

  nativeBuildInputs = [
    patchelf
    pax-utils
    coreutils
    findutils
    gnugrep
    gnused
    file
    squashfsTools
  ];

  buildCommand = ''
    set -euo pipefail

    tree="$TMPDIR/tree"
    install -d -m0755 "$tree/bin" "$tree/helmr" "$tree/lib"
    install -m0755 ${nodejs_24}/bin/node "$tree/bin/node"
    install -m0644 ${../../internal/runtime/entry.mjs} "$tree/helmr/entry.mjs"
    install -m0644 ${../../runtime/typescript/src/preload.mjs} "$tree/helmr/preload.mjs"
    printf '%s' \
      '{"architecture":"${target.architecture}","formatVersion":0,"runtimeApiVersion":"helmr.runtime.v0"}' \
      >"$tree/helmr/runtime.json"
    chmod 0644 "$tree/helmr/runtime.json"

    copy_file() {
      source="$1"
      destination="$2"
      if [ -e "$destination" ]; then
        if ! cmp -s "$source" "$destination"; then
          echo "conflicting runtime file $(basename "$destination")" >&2
          exit 1
        fi
        return
      fi
      install -D -m0644 "$source" "$destination"
    }

    interpreter="$(patchelf --print-interpreter ${nodejs_24}/bin/node)"
    expected_interpreter="${lib.getLib glibc}/lib/${target.loader}"
    if [ "$interpreter" != "$expected_interpreter" ]; then
      echo "Node does not use the exact pinned glibc loader" >&2
      exit 1
    fi
    while IFS= read -r library; do
      [ -n "$library" ] || continue
      [ "$library" != "${nodejs_24}/bin/node" ] || continue
      [ "$library" != "$interpreter" ] || continue
      copy_file "$library" "$tree/lib/$(basename "$library")"
    done < <(lddtree -l ${nodejs_24}/bin/node | LC_ALL=C sort -u)
    copy_file "$interpreter" "$tree/lib/${target.loader}"

    printf '%s' \
      $'passwd: files\ngroup: files\nshadow: files\ngshadow: files\nhosts: files dns\nnetworks: files dns\nprotocols: files\nservices: files\nethers: files\nrpc: files\nnetgroup: files\n' \
      >"$tree/lib/nsswitch.conf"
    chmod 0644 "$tree/lib/nsswitch.conf"

    while IFS= read -r elf; do
      if [ "$(od -An -tx1 -N4 "$elf" | tr -d ' \n')" != "7f454c46" ]; then
        continue
      fi
      if [ "$elf" = "$tree/lib/${target.loader}" ]; then
        continue
      fi
      patchelf --set-rpath /opt/helmr/runtime/lib "$elf"
    done < <(find "$tree/lib" -type f -print | LC_ALL=C sort)
    if [ "$(patchelf --print-soname "$tree/lib/libc.so.6")" != "libc.so.6" ]; then
      echo "libc has unexpected SONAME" >&2
      exit 1
    fi
    patchelf \
      --set-interpreter /opt/helmr/runtime/lib/${target.loader} \
      "$tree/lib/libc.so.6"
    patchelf \
      --set-interpreter /opt/helmr/runtime/lib/${target.loader} \
      --set-rpath /opt/helmr/runtime/lib \
      "$tree/bin/node"

    while IFS= read -r elf; do
      if [ "$(od -An -tx1 -N4 "$elf" | tr -d ' \n')" != "7f454c46" ]; then
        continue
      fi
      while IFS= read -r needed; do
        [ -e "$tree/lib/$needed" ] && continue
        sibling="$(dirname "$elf")/$needed"
        if [ -f "$sibling" ]; then
          ln -s "''${sibling#"$tree/lib/"}" "$tree/lib/$needed"
          continue
        fi
        echo "$(realpath --relative-to="$tree" "$elf") needs missing $needed" >&2
        exit 1
      done < <(patchelf --print-needed "$elf")
      search_path="$(patchelf --print-rpath "$elf")"
      case "$elf" in
        "$tree/lib/${target.loader}")
          if [ -n "$search_path" ]; then
            echo "managed loader has a search path" >&2
            exit 1
          fi
          if [ -n "$(patchelf --print-needed "$elf")" ]; then
            echo "managed loader has dynamic dependencies" >&2
            exit 1
          fi
          ;;
        *)
          if [ "$search_path" != "/opt/helmr/runtime/lib" ]; then
            echo "runtime ELF has unexpected search path $search_path" >&2
            exit 1
          fi
          ;;
      esac
      case "$elf" in
        "$tree/bin/node"|"$tree/lib/libc.so.6")
          if [ "$(patchelf --print-interpreter "$elf")" != \
            "/opt/helmr/runtime/lib/${target.loader}" ]; then
            echo "runtime ELF has unexpected interpreter" >&2
            exit 1
          fi
          ;;
        *)
          if patchelf --print-interpreter "$elf" >/dev/null 2>&1; then
            echo "runtime shared object has an interpreter" >&2
            exit 1
          fi
          ;;
      esac
    done < <(find "$tree/bin/node" "$tree/lib" -type f -print | LC_ALL=C sort)

    if ! cmp -s "$interpreter" "$tree/lib/${target.loader}"; then
      echo "managed loader differs from pinned glibc" >&2
      exit 1
    fi
    find "$tree" -type d -exec chmod 0755 {} +
    while IFS= read -r path; do
      case "$path" in
        "$tree/bin/node"|"$tree/lib/${target.loader}")
          chmod 0755 "$path"
          ;;
        *) chmod 0644 "$path" ;;
      esac
      touch -h -d '@0' "$path"
    done < <(find "$tree" -type f -print | LC_ALL=C sort)
    find "$tree" -type d -exec touch -h -d '@0' {} +
    find "$tree" -type l -exec touch -h -d '@0' {} +

    manifest_line() {
      path="$1"
      relative="''${path#"$tree/"}"
      if [ -d "$path" ]; then
        printf 'd\t0755\t%s\n' "$relative"
      elif [ -L "$path" ]; then
        printf 'l\t0777\t%s\t%s\n' "$relative" "$(readlink "$path")"
      else
        mode=0644
        case "$relative" in
          bin/node|lib/${target.loader}) mode=0755 ;;
        esac
        printf 'f\t%s\t%s\t%s\n' \
          "$mode" "$(sha256sum "$path" | cut -d' ' -f1)" "$relative"
      fi
    }

    install -d "$out"
    while IFS= read -r path; do
      manifest_line "$path"
    done < <(find "$tree" -mindepth 1 -print | LC_ALL=C sort) \
      >"$out/runtime.manifest"

    env -u SOURCE_DATE_EPOCH mksquashfs "$tree" "$out/runtime.squashfs" \
      -noappend \
      -all-root \
      -no-xattrs \
      -no-exports \
      -no-fragments \
      -no-duplicates \
      -no-progress \
      -comp zstd \
      -b 131072 \
      -mkfs-time 0 \
      -all-time 0

    invalid_tree="$TMPDIR/invalid"
    install -d -m0755 "$invalid_tree"
    env -u SOURCE_DATE_EPOCH mksquashfs "$invalid_tree" "$out/verifier-invalid.squashfs" \
      -noappend \
      -all-root \
      -no-xattrs \
      -no-exports \
      -no-fragments \
      -no-duplicates \
      -no-progress \
      -comp zstd \
      -b 131072 \
      -mkfs-time 0 \
      -all-time 0
    if [ "$(stat -c %s "$out/verifier-invalid.squashfs")" -ne 4096 ]; then
      echo "invalid runtime verifier fixture is not exactly 4096 bytes" >&2
      exit 1
    fi

    digest="$(sha256sum "$out/runtime.squashfs" | cut -d' ' -f1)"
    size="$(stat -c %s "$out/runtime.squashfs")"
    printf '%s' \
      '{"architecture":"${target.architecture}","digest":"sha256:'"$digest"'","formatVersion":0,"mediaType":"application/vnd.helmr.runtime.v0+squashfs","runtimeApiVersion":"helmr.runtime.v0","sizeBytes":'"$size"'}' \
      >"$out/runtime.descriptor.json"

    if grep -R -n -E '(^|[[:space:]])(compat|db|hesiod|systemd)([[:space:]]|$)' \
      "$tree/lib/nsswitch.conf"; then
      echo "runtime NSS policy contains an unmanifested service" >&2
      exit 1
    fi
  '';

  passthru = {
    inherit nodejs_24 squashfsTools;
    architecture = target.architecture;
  };

  meta = {
    description = "Helmr managed Node runtime SquashFS";
    platforms = [ "x86_64-linux" ];
  };
}
