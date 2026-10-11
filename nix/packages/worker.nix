{
  lib,
  buildGoModule,
}:

let
  moduleFiles = lib.fileset.unions [
    ../../cmd/worker
    ../../go.mod
    ../../go.sum
    ../../internal
  ];
  runtimeFiles = lib.fileset.intersection moduleFiles (
    lib.fileset.fileFilter (file: file.type != "regular" || !(lib.hasSuffix "_test.go" file.name)) ../..
  );
  moduleSource = lib.fileset.toSource {
    root = ../..;
    fileset = moduleFiles;
  };
in
buildGoModule {
  name = "worker";

  src = lib.fileset.toSource {
    root = ../..;
    fileset = runtimeFiles;
  };

  vendorHash = "sha256-+SjISp+F7A8ZQb5Ry+gfhRRittmg70G7ZJ15dlKxj2Q=";
  overrideModAttrs = _: {
    src = moduleSource;
  };
  subPackages = [ "cmd/worker" ];

  preBuild = ''
    export CGO_ENABLED=0
    export GOARCH=amd64
    export GOOS=linux
  '';
  doCheck = false;
  postInstall = ''
    if [ -x "$out/bin/linux_amd64/worker" ]; then
      install -m 0755 "$out/bin/linux_amd64/worker" "$out/bin/worker"
      rm -rf "$out/bin/linux_amd64"
    fi
    test -x "$out/bin/worker"
  '';
  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Helmr Firecracker worker";
    homepage = "https://helmr.dev";
    license = lib.licenses.asl20;
    mainProgram = "worker";
    platforms = lib.platforms.all;
  };
}
