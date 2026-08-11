{
  lib,
  stdenv,
  glibc,
  patchelf,
  coreutils,
  findutils,
  gnutar,
}:

let
  architecture = "x86_64";
  loader = "ld-linux-x86-64.so.2";
  glibcLib = lib.getLib glibc;
  compilerLib = lib.getLib stdenv.cc.cc;
in
assert lib.assertMsg stdenv.hostPlatform.isx86_64 "Runtime harness supports only x86_64-linux";
stdenv.mkDerivation {
  pname = "helmr-runtime-harness-${architecture}";
  version = "0";
  dontUnpack = true;
  dontPatchELF = true;
  dontStrip = true;
  strictDeps = true;

  nativeBuildInputs = [
    coreutils
    findutils
    gnutar
    patchelf
  ];

  buildCommand = ''
    set -euo pipefail

    tree="$TMPDIR/tree"
    install -d -m0755 "$tree/bin" "$tree/helmr" "$tree/lib"
    install -m0644 ${../../internal/runtime/entry.mjs} "$tree/helmr/entry.mjs"
    install -m0644 ${../../internal/runtime/loader_env.cjs} "$tree/helmr/loader_env.cjs"
    "$CC" -Os -fPIE -pie -fno-ident -Wl,--build-id=none \
      -o "$tree/bin/node" ${../../internal/runtime/node_launcher.c}
    copy_library() {
      name="$1"
      shift
      for directory in "$@"; do
        candidate="$directory/$name"
        if [ -e "$candidate" ]; then
          cp -L "$candidate" "$tree/lib/$name"
          chmod 0644 "$tree/lib/$name"
          return
        fi
      done
      echo "missing Runtime harness library $name" >&2
      exit 1
    }

    copy_library ${loader} ${glibcLib}/lib
    install -m0755 "$tree/lib/${loader}" "$tree/ld.so"
    copy_library libc.so.6 ${glibcLib}/lib
    copy_library libdl.so.2 ${glibcLib}/lib
    copy_library libm.so.6 ${glibcLib}/lib
    copy_library libpthread.so.0 ${glibcLib}/lib
    copy_library libresolv.so.2 ${glibcLib}/lib
    copy_library libgcc_s.so.1 ${compilerLib}/lib ${glibcLib}/lib
    copy_library libstdc++.so.6 ${compilerLib}/lib

    chmod 0755 "$tree/lib/${loader}"
    patchelf \
      --set-interpreter /opt/helmr/runtime/ld.so \
      --set-rpath /opt/helmr/runtime/lib \
      "$tree/bin/node"
    while IFS= read -r elf; do
      [ "$elf" != "$tree/lib/${loader}" ] || continue
      if [ "$elf" = "$tree/lib/libc.so.6" ]; then
        patchelf \
          --set-interpreter /opt/helmr/runtime/ld.so \
          --set-rpath /opt/helmr/runtime/lib \
          "$elf"
      else
        patchelf --set-rpath /opt/helmr/runtime/lib "$elf"
      fi
    done < <(find "$tree/lib" -type f -print | LC_ALL=C sort)

    find "$tree" -type d -exec chmod 0755 {} +
    find "$tree" -type f -exec touch -d '@0' {} +
    find "$tree" -type d -exec touch -d '@0' {} +

    install -d "$out"
    LC_ALL=C tar \
      --create \
      --file "$out/harness.tar" \
      --format=ustar \
      --sort=name \
      --owner=0 \
      --group=0 \
      --numeric-owner \
      --mtime='@0' \
      --directory "$tree" \
      .
    harness_digest="$(sha256sum "$out/harness.tar" | cut -d' ' -f1)"
    harness_size="$(stat -c %s "$out/harness.tar")"
    printf '%s' \
      '{"digest":"sha256:'"$harness_digest"'","mediaType":"application/vnd.helmr.platform-tree.v0+tar","sizeBytes":'"$harness_size"'}' \
      >"$out/harness.descriptor.json"
  '';

  meta = {
    description = "Node-independent Helmr Managed Runtime harness input";
    platforms = [ "x86_64-linux" ];
  };
}
