{
  lib,
  buildGoModule,
  makeBinaryWrapper,
  nodejs_24,
  bun,
  version,
  sourceCommit,
  platformEntries,
}:

let
  moduleFiles =
    lib.fileset.difference
      (lib.fileset.unions [
        ../../cmd/helmr
        ../../go.mod
        ../../go.sum
        ../../internal
      ])
      (
        lib.fileset.unions (
          map lib.fileset.maybeMissing [
            ../../internal/hostconfig/config-evaluator.mjs
            ../../internal/compiler/program-compiler.mjs
            ../../internal/runtime/entry.mjs
            ../../internal/runtime/module-preload.mjs
          ]
        )
      );
  runtimeFiles = lib.fileset.intersection moduleFiles (
    lib.fileset.fileFilter (file: file.type != "regular" || !(lib.hasSuffix "_test.go" file.name)) ../..
  );
  moduleSource = lib.fileset.toSource {
    root = ../..;
    fileset = moduleFiles;
  };
in
buildGoModule {
  pname = "helmr";
  inherit version;

  src = lib.fileset.toSource {
    root = ../..;
    fileset = runtimeFiles;
  };

  vendorHash = "sha256-a80MJ/MA0uh8tI4a7+DyNv8Oq8gUEreJ2NfZud3eztU=";
  overrideModAttrs = _: {
    # Contract checks reuse goModules while compiling package tests. Resolve
    # dependencies from the complete module source even though the shipped CLI
    # source intentionally excludes test files.
    src = moduleSource;
  };
  subPackages = [ "cmd/helmr" ];

  postConfigure = ''
    cp ${platformEntries}/internal/hostconfig/config-evaluator.mjs internal/hostconfig/config-evaluator.mjs
  '';

  ldflags = [
    "-s"
    "-w"
    "-X github.com/helmrdotdev/helmr/internal/version.Version=${version}"
    "-X github.com/helmrdotdev/helmr/internal/version.SourceCommit=${sourceCommit}"
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
