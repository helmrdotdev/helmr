{
  lib,
  stdenv,
  stdenvNoCC,
  patchelf,
  glibc,
  version,
  src,
}:
stdenvNoCC.mkDerivation {
  pname = "nodejs";
  inherit version src;
  strictDeps = true;
  nativeBuildInputs = lib.optionals stdenv.hostPlatform.isLinux [ patchelf ];
  dontConfigure = true;
  dontBuild = true;
  # Preserve official Mach-O bytes/signatures; Linux relocation is explicit below.
  dontStrip = true;
  dontPatchELF = true;
  installPhase = ''
    mkdir -p "$out"
    cp -R . "$out/"
    chmod -R u+w "$out"
  '';
  postFixup =
    lib.optionalString stdenv.hostPlatform.isLinux ''
      patchelf --set-interpreter ${stdenv.cc.bintools.dynamicLinker} \
        --set-rpath ${
          lib.makeLibraryPath [
            glibc
            (lib.getLib stdenv.cc.cc)
          ]
        } \
        "$out/bin/node"
    ''
    + ''
      HOST_PATH="$out/bin" patchShebangs --host "$out/lib/node_modules"
    '';
  meta = {
    description = "Product-selected official Node.js release";
    license = lib.licenses.mit;
    mainProgram = "node";
    platforms = [
      "aarch64-darwin"
      "x86_64-darwin"
      "aarch64-linux"
      "x86_64-linux"
    ];
  };
}
