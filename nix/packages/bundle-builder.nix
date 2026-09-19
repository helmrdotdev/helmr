{
  lib,
  buildGoModule,
}:

buildGoModule {
  pname = "helmr-bundle-builder";
  version = "0";

  src = lib.fileset.toSource {
    root = ../..;
    fileset = lib.fileset.unions [
      ../../cmd/internal/bundle-builder
      ../../go.mod
      ../../go.sum
      ../../internal
    ];
  };

  vendorHash = "sha256-+C7J9qgT3Pdv3y0UkvblgxVdpJiuSMrlnch3No20po8=";
  subPackages = [ "cmd/internal/bundle-builder" ];

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
