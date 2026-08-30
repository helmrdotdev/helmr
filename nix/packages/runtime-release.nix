{
  lib,
  stdenv,
  stdenvNoCC,
  fetchurl,
  glibc,
  coreutils,
  findutils,
  gnutar,
  jq,
  patchelf,
  xz,
  squashfsTools,
}:

let
  architecture = "x86_64";
  nodeVersion = "24.20.0";
  loader = "ld-linux-x86-64.so.2";
  glibcLib = lib.getLib glibc;
  compilerLib = lib.getLib stdenv.cc.cc;
  nodeRelease = fetchurl {
    url = "https://nodejs.org/dist/v${nodeVersion}/node-v${nodeVersion}-linux-x64.tar.xz";
    hash = "sha256-LywNoWIxjw3kdmVBDHyMLtPTbI8xBd5LvGEXbHCny/I=";
  };
in
assert lib.assertMsg stdenv.hostPlatform.isx86_64 "Runtime release supports only x86_64-linux";
stdenvNoCC.mkDerivation {
  pname = "helmr-runtime-release-${architecture}";
  version = nodeVersion;
  dontUnpack = true;
  dontPatchELF = true;
  dontStrip = true;
  strictDeps = true;

  nativeBuildInputs = [
    coreutils
    findutils
    gnutar
    jq
    patchelf
    squashfsTools
    xz
  ];

  buildCommand = ''
    set -euo pipefail

    upstream="$TMPDIR/upstream"
    tree="$TMPDIR/tree"
    mkdir -p "$upstream" "$tree/bin" "$tree/helmr" "$tree/lib" "$tree/share/licenses/node"
    tar -xJf ${nodeRelease} --strip-components=1 --directory "$upstream"
    install -m0755 "$upstream/bin/node" "$tree/bin/node"
    install -m0644 "$upstream/LICENSE" "$tree/share/licenses/node/LICENSE"
    install -m0644 ${../../internal/runtime/entry.mjs} "$tree/helmr/entry.mjs"

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
      echo "missing Runtime library $name" >&2
      exit 1
    }

    copy_library ${loader} ${glibcLib}/lib
    copy_library libc.so.6 ${glibcLib}/lib
    copy_library libdl.so.2 ${glibcLib}/lib
    copy_library libm.so.6 ${glibcLib}/lib
    copy_library libpthread.so.0 ${glibcLib}/lib
    copy_library libresolv.so.2 ${glibcLib}/lib
    copy_library libgcc_s.so.1 ${compilerLib}/lib ${glibcLib}/lib
    copy_library libstdc++.so.6 ${compilerLib}/lib
    chmod 0755 "$tree/lib/${loader}"

    patchelf \
      --set-interpreter /opt/helmr/runtime/lib/${loader} \
      --set-rpath /opt/helmr/runtime/lib \
      "$tree/bin/node"

    for library in \
      libc.so.6 \
      libdl.so.2 \
      libm.so.6 \
      libpthread.so.0 \
      libresolv.so.2 \
      libgcc_s.so.1 \
      libstdc++.so.6; do
      patchelf --set-rpath /opt/helmr/runtime/lib "$tree/lib/$library"
    done
    patchelf \
      --set-interpreter /opt/helmr/runtime/lib/${loader} \
      "$tree/lib/libc.so.6"
    patchelf --remove-rpath "$tree/lib/${loader}"

    jq -cSj -n \
      --arg architecture "${architecture}" \
      --arg nodeVersion "${nodeVersion}" \
      --arg runtimeContract "helmr.runtime.v0" \
      '{
        architecture:$architecture,
        formatVersion:0,
        nodeVersion:$nodeVersion,
        programNodeFlags:["--no-strip-types","--enable-source-maps"],
        runtimeContract:$runtimeContract
      }' >"$tree/helmr/runtime.json"

    "$tree/lib/${loader}" \
      --library-path "$tree/lib" \
      "$tree/bin/node" --version | grep -Fx "v${nodeVersion}"
    "$tree/lib/${loader}" \
      --library-path "$tree/lib" \
      "$tree/bin/node" \
      --no-strip-types \
      --enable-source-maps \
      -e 'process.exit(process.arch === "x64" ? 0 : 1)'
    "$tree/lib/${loader}" \
      --library-path "$tree/lib" \
      "$tree/bin/node" \
      --check "$tree/helmr/entry.mjs"

    find "$tree" -type d -exec chmod 0755 {} +
    find "$tree" -type f ! -path "$tree/bin/node" ! -path "$tree/lib/${loader}" -exec chmod 0644 {} +
    find "$tree" -type f -exec touch -d '@0' {} +
    find "$tree" -type d -exec touch -d '@0' {} +

    tar \
      --create \
      --file "$TMPDIR/runtime.tar" \
      --format=ustar \
      --sort=name \
      --owner=0 \
      --group=0 \
      --numeric-owner \
      --mtime='@0' \
      --directory "$tree" \
      .

    mkdir -p "$out"
    unset SOURCE_DATE_EPOCH
    mksquashfs \
      - "$out/runtime.squashfs" \
      -tar \
      -noappend \
      -all-root \
      -no-xattrs \
      -no-exports \
      -no-fragments \
      -no-tailends \
      -no-duplicates \
      -no-hardlinks \
      -no-progress \
      -exit-on-error \
      -processors 2 \
      -mem 1024M \
      -comp zstd \
      -b 131072 \
      -root-mode 0755 \
      -mkfs-time 0 \
      -all-time 0 \
      <"$TMPDIR/runtime.tar"

    runtime_digest="sha256:$(sha256sum "$out/runtime.squashfs" | cut -d' ' -f1)"
    runtime_size="$(stat -c %s "$out/runtime.squashfs")"
    jq -cSj -n \
      --arg architecture "${architecture}" \
      --arg digest "$runtime_digest" \
      --arg mediaType "application/vnd.helmr.runtime.v0+squashfs" \
      --arg runtimeContract "helmr.runtime.v0" \
      --argjson sizeBytes "$runtime_size" \
      '{
        architecture:$architecture,
        digest:$digest,
        formatVersion:0,
        mediaType:$mediaType,
        runtimeContract:$runtimeContract,
        sizeBytes:$sizeBytes
      }' >"$out/runtime.descriptor.json"
    cp "$tree/helmr/runtime.json" "$out/runtime.metadata.json"
    cp -a "$tree" "$out/tree"
  '';

  passthru = {
    inherit nodeVersion;
  };

  meta = {
    description = "Final Product-owned Helmr Runtime object";
    license = lib.licenses.asl20;
    platforms = [ "x86_64-linux" ];
  };
}
