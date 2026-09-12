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

  vendorHash = "sha256-PRNMDjEDiRmI1LFmMNuS2fXeDvWozj7x3xRMS0o/lZk=";
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
