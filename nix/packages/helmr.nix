{
  lib,
  buildGoModule,
  makeBinaryWrapper,
  nodejs_24,
  bun,
  version,
}:

let
  moduleFiles = lib.fileset.unions [
    ../../cmd/helmr
    ../../go.mod
    ../../go.sum
    ../../internal
  ];
  runtimeFiles = lib.fileset.intersection moduleFiles (
    lib.fileset.fileFilter (file: file.type != "regular" || !(lib.hasSuffix "_test.go" file.name)) ../..
  );
in
buildGoModule {
  pname = "helmr";
  inherit version;

  src = lib.fileset.toSource {
    root = ../..;
    fileset = runtimeFiles;
  };

  vendorHash = "sha256-NKFVYiRGUy3a9BRXkUSKEmzEqTzfY7+F/w0H3JntEEA=";
  subPackages = [ "cmd/helmr" ];

  ldflags = [
    "-s"
    "-w"
    "-X github.com/helmrdotdev/helmr/internal/version.Version=${version}"
  ];

  nativeBuildInputs = [
    makeBinaryWrapper
  ];

  postInstall = ''
    wrapProgram "$out/bin/helmr" \
      --prefix PATH : ${
        lib.makeBinPath [
          nodejs_24
          bun
        ]
      }
  '';

  meta = {
    description = "CLI for deploying and running Helmr task projects";
    homepage = "https://helmr.dev";
    license = lib.licenses.asl20;
    mainProgram = "helmr";
    platforms = [
      "aarch64-darwin"
      "x86_64-darwin"
      "aarch64-linux"
      "x86_64-linux"
    ];
  };
}
