{
  lib,
  stdenvNoCC,
  fetchurl,
  coreutils,
  gnutar,
  gzip,
  jq,
  nodejs_24,
  python3,
}:

let
  version = "0.28.1";
  esbuildPackage = fetchurl {
    url = "https://registry.npmjs.org/esbuild/-/esbuild-${version}.tgz";
    hash = "sha512-HrJrvZv5ayxBzPfwphOoNzkzOIIlifzk0KJrGK2c8R4+LKpMtpYLQeUdjnwjWv/LZlkH2laZk+4w78pi99D4Vw==";
  };
  esbuildLinuxX64 = fetchurl {
    url = "https://registry.npmjs.org/@esbuild/linux-x64/-/linux-x64-${version}.tgz";
    hash = "sha512-u/anNYF2mmVOEDwLtnQ1wOr3EZ9sTNGLWrsYGYwHWzGA3Si84IOkHXlbWTD1NB+9/1lcnweYKO54uhxZydNzfA==";
  };
in
stdenvNoCC.mkDerivation {
  pname = "helmr-compiler";
  inherit version;
  dontUnpack = true;
  dontPatchELF = true;
  dontStrip = true;
  strictDeps = true;

  nativeBuildInputs = [
    coreutils
    gnutar
    gzip
    jq
    nodejs_24
    python3
  ];

  buildCommand = ''
    set -euo pipefail

    tree="$TMPDIR/tree"
    package_root="$tree/node_modules"
    install -d \
      "$package_root/esbuild" \
      "$package_root/@esbuild/linux-x64" \
      "$tree/helmr"
    tar -xzf ${esbuildPackage} \
      --strip-components=1 \
      --directory "$package_root/esbuild"
    tar -xzf ${esbuildLinuxX64} \
      --strip-components=1 \
      --directory "$package_root/@esbuild/linux-x64"
    chmod 0755 "$package_root/@esbuild/linux-x64/bin/esbuild"
    ln -s \
      ../node_modules/@esbuild/linux-x64/bin/esbuild \
      "$tree/helmr/esbuild"
    install -m0644 \
      ${../../internal/compiler/config-evaluator.mjs} \
      "$tree/helmr/config-evaluator.mjs"
    install -m0644 \
      ${../../internal/compiler/program-compiler.mjs} \
      "$tree/helmr/program-compiler.mjs"

    ESBUILD_BINARY_PATH="$package_root/@esbuild/linux-x64/bin/esbuild" \
      node "$tree/helmr/program-compiler.mjs" --describe \
      >"$TMPDIR/contract.json"
    jq -e --arg version "${version}" \
      '.apiVersion == "helmr.compiler.v0" and
       .esbuildVersion == $version and
       .output.aggregate == "analysis-only" and
       .output.finalModules == "independent" and
       .output.sharedChunks == false and
       .source.declarationExtensions == [".cjs", ".cts", ".js", ".jsx", ".mjs", ".mts", ".ts", ".tsx"] and
       .source.packageDependencies == "external" and
       .source.semantics == "pinned-esbuild" and
       .source.workspaceDependencies == "bundled"' \
      "$TMPDIR/contract.json" >/dev/null
    [ "$("$tree/helmr/esbuild" --version)" = "${version}" ]

    find "$tree" -type d -exec chmod 0755 {} +
    find "$tree" -type f -exec touch -d '@0' {} +
    find "$tree" -type l -exec touch -h -d '@0' {} +
    find "$tree" -type d -exec touch -d '@0' {} +

    install -d "$out"
    cp -a "$tree" "$out/tree"
    binary_digest="$(sha256sum \
      "$package_root/@esbuild/linux-x64/bin/esbuild" |
      cut -d' ' -f1)"
    api_digest="$(python - "$package_root/esbuild" <<'PY'
    import hashlib
    import json
    import os
    import stat
    import sys

    root = sys.argv[1]
    entries = []
    for directory, names, files in os.walk(root):
        names.sort()
        files.sort()
        for name in files:
            path = os.path.join(directory, name)
            metadata = os.lstat(path)
            if not stat.S_ISREG(metadata.st_mode):
                raise SystemExit(f"unsupported esbuild API path: {path}")
            with open(path, "rb") as source:
                digest = hashlib.sha256(source.read()).hexdigest()
            entries.append({
                "digest": f"sha256:{digest}",
                "mode": stat.S_IMODE(metadata.st_mode),
                "path": os.path.relpath(path, root).replace(os.sep, "/"),
                "sizeBytes": metadata.st_size,
            })
    encoded = json.dumps(
        entries,
        ensure_ascii=False,
        separators=(",", ":"),
        sort_keys=True,
    ).encode()
    print(hashlib.sha256(encoded).hexdigest())
    PY
    )"
    config_digest="$(sha256sum "$tree/helmr/config-evaluator.mjs" |
      cut -d' ' -f1)"
    program_digest="$(sha256sum "$tree/helmr/program-compiler.mjs" |
      cut -d' ' -f1)"
    jq -cSj \
      --arg apiPackageDigest "sha256:$api_digest" \
      --arg binaryDigest "sha256:$binary_digest" \
      --arg configEvaluatorDigest "sha256:$config_digest" \
      --arg programCompilerDigest "sha256:$program_digest" \
      '{
        apiVersion:.apiVersion,
        configEvaluator:{
          apiVersion:"helmr.config-evaluator.v0",
          digest:$configEvaluatorDigest,
          entrypoint:"/nix/helmr/config-evaluator.mjs"
        },
        esbuild:{
          apiPackageDigest:$apiPackageDigest,
          binaryDigest:$binaryDigest,
          binaryPath:"/nix/helmr/esbuild",
          packagePath:"/nix/node_modules/esbuild",
          version:.esbuildVersion
        },
        optionsContractDigest:.optionsContractDigest,
        output:.output,
        programCompiler:{
          apiVersion:.apiVersion,
          digest:$programCompilerDigest,
          entrypoint:"/nix/helmr/program-compiler.mjs"
        },
        source:.source
      }' "$TMPDIR/contract.json" >"$out/compiler.descriptor.json"
  '';

  meta = {
    description = "Pinned Helmr Config and Program compiler";
    platforms = [ "x86_64-linux" ];
  };
}
