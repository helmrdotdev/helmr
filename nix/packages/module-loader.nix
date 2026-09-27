{
  stdenvNoCC,
  fetchurl,
  gnutar,
  gzip,
  typescriptRelease,
}:
let
  typescript = fetchurl {
    url = "https://registry.npmjs.org/typescript/-/typescript-${typescriptRelease.version}.tgz";
    hash = typescriptRelease.integrity;
  };
in
stdenvNoCC.mkDerivation {
  pname = "helmr-module-loader";
  version = "0";
  dontUnpack = true;
  nativeBuildInputs = [
    gnutar
    gzip
  ];
  buildCommand = ''
    mkdir -p "$out/moduleloader" "$out/share/licenses/typescript" upstream
    tar -xzf ${typescript} --strip-components=1 -C upstream
    install -m0644 ${../../internal/moduleloader/loader.mjs} "$out/moduleloader/loader.mjs"
    install -m0644 upstream/lib/typescript.js "$out/moduleloader/typescript.cjs"
    install -m0644 upstream/LICENSE.txt "$out/share/licenses/typescript/LICENSE"
  '';
}
