{
  lib,
  stdenvNoCC,
  coreutils,
  jq,
  nodejs_24,
  moduleLoader,
  typescriptVersion,
}:
stdenvNoCC.mkDerivation {
  pname = "helmr-compiler";
  version = "0";
  dontUnpack = true;
  strictDeps = true;
  nativeBuildInputs = [
    coreutils
    jq
    nodejs_24
  ];
  buildCommand = ''
    set -euo pipefail
    tree="$TMPDIR/tree"
    install -d "$tree/helmr"
    cp -a ${moduleLoader}/moduleloader "$tree/moduleloader"
    cp -a ${moduleLoader}/share "$tree/share"
    install -m0644 ${../../internal/compiler/program-compiler.mjs} "$tree/helmr/program-compiler.mjs"
    node "$tree/helmr/program-compiler.mjs" --describe >"$TMPDIR/contract.json"
    jq -e --arg typescriptVersion '${typescriptVersion}' '.apiVersion == "helmr.compiler.v0" and .moduleLoader.apiVersion == "helmr.module-loader.v0" and .moduleLoader.typescriptVersion == $typescriptVersion' "$TMPDIR/contract.json" >/dev/null
    program_digest="$(sha256sum "$tree/helmr/program-compiler.mjs" | cut -d' ' -f1)"
    install -d "$out"
    cp -a "$tree" "$out/tree"
    jq -cSj \
      --arg programDigest "sha256:$program_digest" \
      '{apiVersion:.apiVersion,moduleLoader:.moduleLoader,
        programCompiler:{apiVersion:.apiVersion,digest:$programDigest,entrypoint:"/nix/helmr/program-compiler.mjs"}
      }' "$TMPDIR/contract.json" >"$out/compiler.descriptor.json"
  '';
  meta = {
    description = "Helmr native source declaration analysis";
    platforms = [ "x86_64-linux" ];
  };
}
