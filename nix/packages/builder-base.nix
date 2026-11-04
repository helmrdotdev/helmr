{
  lib,
  dockerTools,
  fetchurl,
  linkFarm,
  debian,
}:

let
  source =
    file: snapshot: sha256:
    fetchurl {
      urls = [
        "https://deb.debian.org/debian/pool/main/o/openssh/${file}"
        "https://snapshot.debian.org/file/${snapshot}"
      ];
      inherit sha256;
    };
  upstream =
    source "openssh_10.0p1.orig.tar.gz" "ac4205e827aea383bf316a33a0e2d5b66b85fcf8"
      "021a2e709a0edf4250b1256bd5a9e500411a90dddabea830ed59cef90eb9d85c";
  packaging =
    source "openssh_10.0p1-7+deb13u4.debian.tar.xz" "5322cbd663e2d9e72726ec01a88ebd49c767a517"
      "102e1065030c6002acabd7f896eeba1462bf54b4d7393bac34b0308312868ec6";

  # APT resolves these against the pinned slim base without Recommends.
  # Updates must keep the native toolchain compatible with the managed Runtime.
  archives = linkFarm "builder-debs" (
    map (
      line:
      let
        fields = lib.splitString " " line;
        path = builtins.elemAt fields 2;
      in
      {
        name = baseNameOf path;
        path = fetchurl {
          urls = [
            "https://deb.debian.org/${path}"
            "https://snapshot.debian.org/file/${builtins.elemAt fields 1}"
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
  compressor = "none";
  diskSize = 8192;
  buildVMMemorySize = 2048;
  runAsRoot = ''
    /bin/sh ${./provision-builder.sh} ${archives} ${upstream} ${packaging} ${./openssh-client-rekey.patch}
  '';
}
