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

  vendorHash = "sha256-7wlmi+xGFQcmwgELqYBb1pz0xdEwHF5F+j1scMk4NOk=";
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
