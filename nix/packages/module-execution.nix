{
  stdenvNoCC,
  fetchurl,
  gnutar,
  gzip,
}:
let
  typescript = fetchurl {
    url = "https://registry.npmjs.org/typescript/-/typescript-6.0.3.tgz";
    hash = "sha512-y2TvuxSZPDyQakkFRPZHKFm+KKVqIisdg9/CZwm9ftvKXLP8NRWj38/ODjNbr43SsoXqNuAisEf1GdCxqWcdBw==";
  };
in
stdenvNoCC.mkDerivation {
  pname = "helmr-module-execution";
  version = "0";
  dontUnpack = true;
  nativeBuildInputs = [
    gnutar
    gzip
  ];
  buildCommand = ''
    mkdir -p "$out/moduleexecution" "$out/share/licenses/typescript" upstream
    tar -xzf ${typescript} --strip-components=1 -C upstream
    install -m0644 ${../../internal/moduleexecution/loader.mjs} "$out/moduleexecution/loader.mjs"
    install -m0644 upstream/lib/typescript.js "$out/moduleexecution/typescript.cjs"
    install -m0644 upstream/LICENSE.txt "$out/share/licenses/typescript/LICENSE"
  '';
}
