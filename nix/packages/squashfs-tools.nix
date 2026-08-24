{
  lib,
  stdenv,
  fetchFromGitHub,
  help2man,
  which,
  zstd,
}:

stdenv.mkDerivation (finalAttrs: {
  pname = "helmr-squashfs-tools";
  version = "4.6.1";

  src = fetchFromGitHub {
    owner = "plougher";
    repo = "squashfs-tools";
    rev = finalAttrs.version;
    hash = "sha256-fJ+Ijg0cj92abGe80+1swVeZamarVpnPYM7+izcPJ+k=";
  };

  strictDeps = true;
  nativeBuildInputs = [
    which
  ]
  ++ lib.optionals (stdenv.buildPlatform == stdenv.hostPlatform) [ help2man ];
  buildInputs = [ zstd ];

  preBuild = ''
    cd squashfs-tools
  '';

  makeFlags = [
    "GZIP_SUPPORT=0"
    "XZ_SUPPORT=0"
    "LZO_SUPPORT=0"
    "LZ4_SUPPORT=0"
    "LZMA_XZ_SUPPORT=0"
    "ZSTD_SUPPORT=1"
    "COMP_DEFAULT=zstd"
    "INSTALL_DIR=${placeholder "out"}/bin"
    "INSTALL_MANPAGES_DIR=${placeholder "out"}/share/man/man1"
  ];

  postInstall = ''
    "$out/bin/mksquashfs" -version 2>&1 | grep -E '^mksquashfs version 4[.]6[.]1([[:space:]]|$)'
  '';

  meta = {
    description = "Helmr contract SquashFS encoder";
    homepage = "https://github.com/plougher/squashfs-tools";
    license = lib.licenses.gpl2Plus;
    mainProgram = "mksquashfs";
    platforms = lib.platforms.unix;
  };
})
