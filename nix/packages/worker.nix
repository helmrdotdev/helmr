{
  lib,
  buildGoModule,
}:

let
  moduleFiles = lib.fileset.unions [
    ../../cmd/helmr-worker
    ../../capacityapi
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
  pname = "helmr-worker";
  version = "0.0.0";

  src = lib.fileset.toSource {
    root = ../..;
    fileset = runtimeFiles;
  };

  vendorHash = "sha256-jYDWiu8vwEqBTrcJx4Qm4RRexiE3eLreI/aA6xw4iT4=";
  overrideModAttrs = _: {
    src = moduleSource;
  };
  subPackages = [ "cmd/helmr-worker" ];

  preBuild = ''
    export CGO_ENABLED=0
    export GOARCH=amd64
    export GOOS=linux
  '';
  doCheck = false;
  postInstall = ''
    if [ -x "$out/bin/linux_amd64/helmr-worker" ]; then
      install -m 0755 "$out/bin/linux_amd64/helmr-worker" "$out/bin/helmr-worker"
      rm -rf "$out/bin/linux_amd64"
    fi
    test -x "$out/bin/helmr-worker"
  '';
  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Helmr Firecracker worker";
    homepage = "https://helmr.dev";
    license = lib.licenses.asl20;
    mainProgram = "helmr-worker";
    platforms = lib.platforms.all;
  };
}
