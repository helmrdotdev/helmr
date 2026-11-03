{
  lib,
  dockerTools,
  fetchurl,
  linkFarm,
  debian,
}:

let
  # APT resolves these against the pinned slim base without Recommends.
  # Updates must keep the native toolchain compatible with the managed Runtime.
  archives = linkFarm "builder-debs" (
    map (
      line:
      let
        fields = lib.splitString " " line;
        path = builtins.elemAt fields 1;
      in
      {
        name = baseNameOf path;
        path = fetchurl {
          urls = [
            "https://deb.debian.org/${path}"
            "https://snapshot.debian.org/archive/${builtins.head (lib.splitString "/" path)}/20261103T160000Z/${lib.concatStringsSep "/" (builtins.tail (lib.splitString "/" path))}"
          ];
          sha256 = builtins.head fields;
        };
      }
    ) (lib.splitString "\n" (lib.removeSuffix "\n" (builtins.readFile ./builder-debs.lock)))
  );
in
dockerTools.buildImage {
  name = "helmr-builder-base";
  fromImage = debian;
  diskSize = 4096;
  buildVMMemorySize = 2048;
  runAsRoot = ''
    export PATH=/usr/sbin:/usr/bin:/sbin:/bin
    export DEBIAN_FRONTEND=noninteractive
    apt-get --no-download --no-install-recommends -y install ${archives}/*.deb
    dpkg --audit
    rm -f /var/log/dpkg.log /var/log/apt/* /var/log/alternatives.log
  '';
}
