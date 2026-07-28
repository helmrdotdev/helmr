{
  lib,
  stdenv,
  stdenvNoCC,
  buildEnv,
  closureInfo,
  bash,
  coreutils,
  gcc,
  gnumake,
  gnutar,
  pkg-config,
  python3,
}:

let
  architecture = "x86_64";
  roots = [
    bash
    coreutils
    gcc
    gnumake
    pkg-config
    python3
  ];
  environment = buildEnv {
    name = "helmr-toolchain-base-env";
    paths = roots;
    pathsToLink = [ "/bin" ];
    ignoreCollisions = false;
  };
  closure = closureInfo {
    rootPaths = [ environment ];
  };
in
assert lib.assertMsg stdenv.hostPlatform.isx86_64 "toolchain base supports only x86_64-linux";
stdenvNoCC.mkDerivation {
  pname = "helmr-toolchain-base-${architecture}";
  version = "0";
  dontUnpack = true;
  dontPatchELF = true;
  dontStrip = true;
  strictDeps = true;

  nativeBuildInputs = [
    coreutils
    gnutar
  ];

  buildCommand = ''
    set -euo pipefail

    tree="$TMPDIR/tree"
    install -d -m0755 "$tree/store"
    while IFS= read -r source; do
      cp -a "$source" "$tree/store/$(basename "$source")"
    done <${closure}/store-paths
    ln -s "store/$(basename ${environment})/bin" "$tree/bin"

    find "$tree" -type d -exec touch -h -d '@0' {} +
    find "$tree" -type f -exec touch -h -d '@0' {} +
    find "$tree" -type l -exec touch -h -d '@0' {} +

    install -d "$out"
    LC_ALL=C tar \
      --create \
      --file "$out/base.tar" \
      --format=posix \
      --sort=name \
      --owner=0 \
      --group=0 \
      --numeric-owner \
      --mtime='@0' \
      --pax-option=delete=atime,delete=ctime \
      --directory "$tree" \
      .
    digest="$(sha256sum "$out/base.tar" | cut -d' ' -f1)"
    size="$(stat -c %s "$out/base.tar")"
    printf '%s' \
      '{"digest":"sha256:'"$digest"'","mediaType":"application/vnd.helmr.platform-tree.v0+tar","sizeBytes":'"$size"'}' \
      >"$out/base.descriptor.json"
  '';

  meta = {
    description = "Node-independent Helmr native build toolchain input";
    platforms = [ "x86_64-linux" ];
  };
}
