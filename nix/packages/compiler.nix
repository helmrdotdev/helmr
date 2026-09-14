{
  lib,
  stdenvNoCC,
  coreutils,
  jq,
  nodejs_24,
  moduleExecution,
}:
stdenvNoCC.mkDerivation {
  pname = "helmr-compiler";
  version = "1";
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
    cp -a ${moduleExecution}/moduleexecution "$tree/moduleexecution"
    cp -a ${moduleExecution}/share "$tree/share"
    install -m0644 ${../../internal/compiler/config-evaluator.mjs} "$tree/helmr/config-evaluator.mjs"
    install -m0644 ${../../internal/compiler/program-compiler.mjs} "$tree/helmr/program-compiler.mjs"
    node "$tree/helmr/program-compiler.mjs" --describe >"$TMPDIR/contract.json"
    jq -e '.apiVersion == "helmr.compiler.v1" and .language.apiVersion == "helmr.module-execution.v1" and .language.typescriptVersion == "6.0.3"' "$TMPDIR/contract.json" >/dev/null
    config_digest="$(sha256sum "$tree/helmr/config-evaluator.mjs" | cut -d' ' -f1)"
    program_digest="$(sha256sum "$tree/helmr/program-compiler.mjs" | cut -d' ' -f1)"
    install -d "$out"
    cp -a "$tree" "$out/tree"
    jq -cSj \
      --arg configDigest "sha256:$config_digest" \
      --arg programDigest "sha256:$program_digest" \
      '{apiVersion:.apiVersion,language:.language,
        configEvaluator:{apiVersion:"helmr.config-evaluator.v1",digest:$configDigest,entrypoint:"/nix/helmr/config-evaluator.mjs"},
        programCompiler:{apiVersion:.apiVersion,digest:$programDigest,entrypoint:"/nix/helmr/program-compiler.mjs"}
      }' "$TMPDIR/contract.json" >"$out/compiler.descriptor.json"
  '';
  meta = {
    description = "Helmr native source config and declaration analysis";
    platforms = [ "x86_64-linux" ];
  };
}
