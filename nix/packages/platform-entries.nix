{
  lib,
  stdenvNoCC,
  bun,
  cacert,
  fetchurl,
}:

let
  root = ../..;
  package = builtins.fromJSON (builtins.readFile ../../package.json);
  workspaceDirectories = lib.concatMap (
    workspace:
    if lib.hasSuffix "/*" workspace then
      let
        directory = root + "/${lib.removeSuffix "/*" workspace}";
        children = builtins.readDir directory;
      in
      map (name: directory + "/${name}") (
        builtins.filter (
          name: children.${name} == "directory" && builtins.pathExists (directory + "/${name}/package.json")
        ) (builtins.attrNames children)
      )
    else
      [ (root + "/${workspace}") ]
  ) package.workspaces;
  manifests = lib.fileset.unions (
    [ ../../package.json ] ++ map (directory: directory + "/package.json") workspaceDirectories
  );
  dependencySource = lib.fileset.toSource {
    inherit root;
    fileset = lib.fileset.unions [
      manifests
      ../../bun.lock
      ../../bunfig.toml
    ];
  };
  installCommand = ''bun install --frozen-lockfile --ignore-scripts --omit=optional --cache-dir "$TMPDIR/bun-cache"'';
  installDependencies = ''
    mkdir -p "$out"
    while IFS= read -r -d $'\0' directory; do
      mkdir -p "$out/$(dirname "$directory")"
      cp -R "$directory" "$out/$(dirname "$directory")/"
    done < <(find . -type d -name node_modules -prune -print0)
    # Generation loads JS modules directly and uses the separately fetched binary.
    # Bun's generated command links are unused and can vary between installs.
    find "$out" -type d -path '*/node_modules/.bin' -prune -exec rm -rf {} +
  '';
  dependencyIdentity = builtins.hashString "sha256" (
    builtins.toJSON {
      source = dependencySource;
      tool = bun;
      inherit installCommand installDependencies;
    }
  );
  deps = stdenvNoCC.mkDerivation {
    pname = "helmr-platform-entry-deps-${dependencyIdentity}";
    version = "1";
    src = dependencySource;
    nativeBuildInputs = [ bun ];
    SSL_CERT_FILE = "${cacert}/etc/ssl/certs/ca-bundle.crt";
    dontConfigure = true;
    dontFixup = true;
    buildPhase = installCommand;
    installPhase = installDependencies;
    outputHashMode = "recursive";
    outputHashAlgo = "sha256";
    outputHash = "sha256-iJ/QONXc5byd5C0l2rzH8/2RR27f1TwJqasSMGLvYzE=";
  };
  platforms = {
    aarch64-darwin = {
      name = "darwin-arm64";
      hash = "sha512-n4KqkOQrraxHJcgjM1RvwbigfQKIKJVpM7xp+KsxiyUSrRdIXnt73VhrPAx0fV44hgfmIVKjxMN9J1t5jySVkw==";
    };
    x86_64-darwin = {
      name = "darwin-x64";
      hash = "sha512-uq6suIWYP37qzGddBKPw5QEQPi6HiLGsO7UmkpfyaYNQ3D+rN6w6WfwH+nuqcGXWvawGwxOEroO4YGnFh95azw==";
    };
    aarch64-linux = {
      name = "linux-arm64";
      hash = "sha512-pW4AC0P3it8c7do9MVM4p51FzHzdM/TZrerurgRcHJ2WTa1VQ1CIq18xncfpBJw4ojkiZZrKW2yIBWBP92j6Ug==";
    };
    x86_64-linux = {
      name = "linux-x64";
      hash = "sha512-4xTZr1FUmSoQW4XIWmit3tzQrUTZM+N3P0XV8xROKYF50XfI7xeO90+1bZvNwxIufQ9hDQVRJH5YhgPVF8A/HQ==";
    };
  };
  platform = platforms.${stdenvNoCC.hostPlatform.system};
  esbuildVersion = "0.28.2";
  binary = fetchurl {
    url = "https://registry.npmjs.org/@esbuild/${platform.name}/-/${platform.name}-${esbuildVersion}.tgz";
    inherit (platform) hash;
  };
in
assert package.devDependencies.esbuild == esbuildVersion;
assert package.overrides.esbuild == esbuildVersion;
stdenvNoCC.mkDerivation {
  pname = "helmr-platform-entries";
  version = "1";
  src = lib.fileset.toSource {
    inherit root;
    fileset = lib.fileset.unions [
      manifests
      ../../bun.lock
      ../../bunfig.toml
      ../../tsconfig.json
      ../../compiler/typescript/src
      ../../compiler/typescript/tsconfig.json
      ../../runtime/typescript/src
      ../../runtime/typescript/tsconfig.json
      ../../sdk/typescript/src
      ../../sdk/typescript/tsconfig.json
      ../../proto/typescript/src
      ../../proto/typescript/tsconfig.json
      ../../scripts/build-platform-entries.ts
      ../../scripts/node-version.mjs
      ../../internal/version/runtime-dependencies.json
    ];
  };
  nativeBuildInputs = [ bun ];
  dontConfigure = true;
  dontFixup = true;
  buildPhase = ''
    cp -R ${deps}/. .
    chmod -R u+w node_modules
    mkdir esbuild-binary
    tar -xzf ${binary} --strip-components=1 -C esbuild-binary
    export ESBUILD_BINARY_PATH="$PWD/esbuild-binary/bin/esbuild"
    test -x "$ESBUILD_BINARY_PATH"
    test "$("$ESBUILD_BINARY_PATH" --version)" = '${esbuildVersion}'
    bun scripts/build-platform-entries.ts all --out-dir "$out"
  '';
  installPhase = "true";
  passthru = { inherit deps; };
}
