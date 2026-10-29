{
  lib,
  buildGoModule,
  platformEntries,
}:

buildGoModule {
  pname = "helmr-bundle-builder";
  version = "0";

  src = lib.fileset.toSource {
    root = ../..;
    fileset =
      lib.fileset.difference
        (lib.fileset.unions [
          ../../cmd/internal/bundle-builder
          ../../go.mod
          ../../go.sum
          ../../internal
          ../../starters
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
  };

  vendorHash = "sha256-6AuLhYY/rYZ1pHaF4EKyjRnZNjfz1GB2raXzz2QtV3I=";
  subPackages = [ "cmd/internal/bundle-builder" ];

  postConfigure = ''
    cp ${platformEntries}/internal/hostconfig/config-evaluator.mjs internal/hostconfig/config-evaluator.mjs
  '';

  # Static: the builder runs unchanged inside user-prepared environments.
  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Helmr canonical installed-tree bundle finalizer";
    license = lib.licenses.asl20;
    mainProgram = "bundle-builder";
    platforms = [ "x86_64-linux" ];
  };
}
