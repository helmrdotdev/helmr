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
      ../../capacityapi
      ../../go.mod
      ../../go.sum
      ../../internal
    ];
  };

  vendorHash = "sha256-bQKk+qP1RR9hCAAdEPjM1UzZtAPaAQpTnjAPrnLOw9M=";
  subPackages = [ "cmd/internal/bundle-builder" ];

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
