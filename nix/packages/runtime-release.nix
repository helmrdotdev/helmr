{
  lib,
  moduleExecution,
  stdenv,
  stdenvNoCC,
  debianRuntimeImage,
  nodeVersion,
  typescriptVersion,
  nodeRelease,
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
  loader = "ld-linux-x86-64.so.2";
  # Every glibc component an addon or Workspace library can name must already
  # be loaded from the Runtime, so Node depends on the ones it does not link itself.
  nodeLibraries = [
    "libdl.so.2"
    "libstdc++.so.6"
    "libm.so.6"
    "libgcc_s.so.1"
    "libpthread.so.0"
    "libc.so.6"
  ];
  preloadedLibraries = [
    "libmvec.so.1"
    "librt.so.1"
    "libutil.so.1"
    "libanl.so.1"
    "libresolv.so.2"
  ];
  # libc resolves these plugins itself through its Runtime RUNPATH.
  nssLibraries = [
    "libnss_compat.so.2"
    "libnss_dns.so.2"
    "libnss_files.so.2"
    "libnss_hesiod.so.2"
  ];
  runtimeLibraries = nodeLibraries ++ preloadedLibraries ++ nssLibraries;
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

    install -m0644 ${../../internal/runtime/module-preload.mjs} "$tree/helmr/module-preload.mjs"
    cp -a ${moduleExecution}/moduleexecution "$tree/moduleexecution"
    cp -a ${moduleExecution}/share/licenses/typescript "$tree/share/licenses/typescript"
    adapter_digest="sha256:$(sha256sum "$tree/moduleexecution/loader.mjs" | cut -d' ' -f1)"
    typescript_digest="sha256:$(sha256sum "$tree/moduleexecution/typescript.cjs" | cut -d' ' -f1)"

    debian="$TMPDIR/debian"
    mkdir -p "$TMPDIR/image" "$debian" "$tree/share/licenses/debian"
    tar -xf ${debianRuntimeImage} --directory "$TMPDIR/image"
    jq -r '.[0].Layers[]' "$TMPDIR/image/manifest.json" | while read -r layer; do
      tar -xf "$TMPDIR/image/$layer" --directory "$debian" \
        --no-same-owner --no-same-permissions --exclude='dev/*' \
        --wildcards 'usr/lib/x86_64-linux-gnu/*' 'usr/share/doc/*'
    done
    debian_lib="$debian/usr/lib/x86_64-linux-gnu"
    for name in ${loader} ${lib.concatStringsSep " " runtimeLibraries}; do
      if [ ! -e "$debian_lib/$name" ]; then
        echo "missing Debian Runtime library $name" >&2
        exit 1
      fi
      cp -L "$debian_lib/$name" "$tree/lib/$name"
      chmod 0644 "$tree/lib/$name"
    done
    chmod 0755 "$tree/lib/${loader}"
    for package in libc6 libgcc-s1 libstdc++6; do
      install -m0644 "$(realpath "$debian/usr/share/doc/$package/copyright")" \
        "$tree/share/licenses/debian/$package"
    done

    # One edit per invocation; the resulting dynamic sections are asserted below.
    for name in ${lib.concatStringsSep " " preloadedLibraries}; do
      patchelf --add-needed "$name" "$tree/bin/node"
    done
    patchelf --set-interpreter /opt/helmr/runtime/lib/${loader} "$tree/bin/node"
    patchelf --set-rpath /opt/helmr/runtime/lib "$tree/bin/node"
    for name in ${lib.concatStringsSep " " runtimeLibraries}; do
      patchelf --set-rpath /opt/helmr/runtime/lib "$tree/lib/$name"
    done
    patchelf --set-interpreter /opt/helmr/runtime/lib/${loader} "$tree/lib/libc.so.6"

    # A wrong search path or dependency set would silently mix the Workspace
    # image's C runtime into Node, so the exact dynamic sections are required.
    require_dynamic() {
      if [ "$2" != "$3" ]; then
        echo "Runtime $1 = '$2', want '$3'" >&2
        exit 1
      fi
    }
    require_dynamic "Node interpreter" \
      "$(patchelf --print-interpreter "$tree/bin/node")" /opt/helmr/runtime/lib/${loader}
    require_dynamic "Node dependencies" \
      "$(patchelf --print-needed "$tree/bin/node" | sort | tr '\n' ' ')" \
      "$(printf '%s\n' ${lib.concatStringsSep " " ([ loader ] ++ nodeLibraries ++ preloadedLibraries)} | sort | tr '\n' ' ')"
    for file in bin/node ${lib.concatMapStringsSep " " (name: "lib/${name}") runtimeLibraries}; do
      require_dynamic "$file RUNPATH" "$(patchelf --print-rpath "$tree/$file")" /opt/helmr/runtime/lib
    done
    require_dynamic "loader RUNPATH" "$(patchelf --print-rpath "$tree/lib/${loader}")" ""
    require_dynamic "loader dependencies" "$(patchelf --print-needed "$tree/lib/${loader}")" ""

    jq -cSj -n \
      --arg architecture "${architecture}" \
      --arg nodeVersion "${nodeVersion}" \
      --arg typescriptVersion "${typescriptVersion}" \
      --arg adapterDigest "$adapter_digest" \
      --arg typescriptDigest "$typescript_digest" \
      --arg runtimeContract "helmr.runtime.v0" \
      '{
        architecture:$architecture,
        formatVersion:0,
        nodeVersion:$nodeVersion,
        programNodeFlags:["--no-strip-types","--no-global-search-paths","--enable-source-maps","--import=file:///opt/helmr/runtime/helmr/module-preload.mjs"],
        language:{apiVersion:"helmr.module-execution.v0",adapterDigest:$adapterDigest,typescriptDigest:$typescriptDigest,typescriptVersion:$typescriptVersion},
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
