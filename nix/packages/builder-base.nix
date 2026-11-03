{
  lib,
  dockerTools,
  crane,
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
dockerTools.buildLayeredImage {
  name = "helmr-builder-base";
  includeStorePaths = false;
  enableFakechroot = true;
  fakeRootCommands = ''
    ${crane}/bin/crane export - - < ${debian} | tar -xp --numeric-owner --exclude=dev --exclude=proc --exclude=sys
    export DEBIAN_FRONTEND=noninteractive
    PATH=/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/apt-get -o APT::Sandbox::User=root \
      --no-download --no-install-recommends -y install ${archives}/*.deb
    /usr/bin/dpkg --audit
    rm -f /var/log/dpkg.log /var/log/apt/* /var/log/alternatives.log
  '';
}
