{
  lib,
  buildGoModule,
  makeBinaryWrapper,
  nodejs_24,
  bun,
  version,
}:

let
  packageFiles = lib.fileset.unions [
    ../../cmd/helmr
    ../../go.mod
    ../../go.sum
    ../../internal/adapter
    ../../internal/api
    ../../internal/archive
    ../../internal/cas
    ../../internal/cli/browser
    ../../internal/cli/format
    ../../internal/cli/session
    ../../internal/cli/ui
    ../../internal/client
    ../../internal/compute
    ../../internal/db
    ../../internal/pgvalue
    ../../internal/safepath
    ../../internal/secret
    ../../internal/sha256sum
    ../../internal/version
  ];
  runtimeFiles = lib.fileset.intersection packageFiles (
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

  vendorHash = "sha256-+vT3n2SfCd7FiLIe9mtSaz3S2Th8YkJFexTsyP8n+hw=";
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
