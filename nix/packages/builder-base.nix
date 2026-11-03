{
  lib,
  dockerTools,
  fetchurl,
  linkFarm,
  debian,
}:

let
  source =
    file: sha256:
    fetchurl {
      urls = [
        "https://deb.debian.org/debian/pool/main/o/openssh/${file}"
        "https://snapshot.debian.org/archive/debian/20261103T160000Z/pool/main/o/openssh/${file}"
      ];
      inherit sha256;
    };
  upstream = source "openssh_10.0p1.orig.tar.gz" "021a2e709a0edf4250b1256bd5a9e500411a90dddabea830ed59cef90eb9d85c";
  packaging = source "openssh_10.0p1-7+deb13u4.debian.tar.xz" "102e1065030c6002acabd7f896eeba1462bf54b4d7393bac34b0308312868ec6";

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
  diskSize = 8192;
  buildVMMemorySize = 2048;
  runAsRoot = ''
    /bin/sh ${./provision-builder.sh} ${archives} ${upstream} ${packaging} ${./openssh-client-rekey.patch}
  '';
}
