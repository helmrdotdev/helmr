{
  lib, stdenvNoCC, coreutils, jq, nodejs_24, fetchurl, binutils,
}:
let
  api = fetchurl {
    url = "https://registry.npmjs.org/esbuild/-/esbuild-0.28.2.tgz";
    hash = "sha512-HKVLS8dvII+xoKW9kmqxbRKrnWEXfJJr/FZhhJmiqIB0e053QNYFqOBouTMO/k5sID4MvCiUCvv8b9M4h32wIA==";
  };
  binary = fetchurl {
    url = "https://registry.npmjs.org/@esbuild/linux-x64/-/linux-x64-0.28.2.tgz";
    hash = "sha512-4xTZr1FUmSoQW4XIWmit3tzQrUTZM+N3P0XV8xROKYF50XfI7xeO90+1bZvNwxIufQ9hDQVRJH5YhgPVF8A/HQ==";
  };
in stdenvNoCC.mkDerivation {
  pname = "helmr-compiler";
  version = "0";
  dontUnpack = true;
  strictDeps = true;
  nativeBuildInputs = [ coreutils jq nodejs_24 binutils ];
  buildCommand = ''
    set -euo pipefail
    tree="$TMPDIR/tree"
    install -d "$tree/helmr/node_modules/esbuild" "$tree/helmr/node_modules/@esbuild/linux-x64"
    tar -xzf ${api} --strip-components=1 -C "$tree/helmr/node_modules/esbuild"
    tar -xzf ${binary} --strip-components=1 -C "$tree/helmr/node_modules/@esbuild/linux-x64"
    if readelf -l "$tree/helmr/node_modules/@esbuild/linux-x64/bin/esbuild" | grep -q INTERP; then
      echo "esbuild must be a self-contained executable" >&2; exit 1
    fi
    install -m0644 ${../../internal/compiler/program-compiler.mjs} "$tree/helmr/program-compiler.mjs"
    node "$tree/helmr/program-compiler.mjs" --describe >"$TMPDIR/contract.json"
    jq -e '.apiVersion == "helmr.compiler.v0" and .bundler.apiVersion == "helmr.bundle.v0" and .bundler.esbuildVersion == "0.28.2"' "$TMPDIR/contract.json" >/dev/null
    program_digest="$(sha256sum "$tree/helmr/program-compiler.mjs" | cut -d' ' -f1)"
    install -d "$out"
    cp -a "$tree" "$out/tree"
    jq -cSj --arg programDigest "sha256:$program_digest" \
      '{apiVersion:.apiVersion,bundler:.bundler,
        programCompiler:{apiVersion:.apiVersion,digest:$programDigest,entrypoint:"/nix/helmr/program-compiler.mjs"}
      }' "$TMPDIR/contract.json" >"$out/compiler.descriptor.json"
  '';
  meta = {
    description = "Helmr Program bundling and declaration analysis";
    platforms = [ "x86_64-linux" ];
  };
}
