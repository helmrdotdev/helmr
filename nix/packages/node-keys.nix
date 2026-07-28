{
  stdenvNoCC,
  fetchFromGitHub,
  gnupg,
}:

stdenvNoCC.mkDerivation {
  pname = "helmr-node-release-keys";
  version = "2026-07-28";

  src = fetchFromGitHub {
    owner = "nodejs";
    repo = "release-keys";
    rev = "b28073028e6d6855cfb53bf7fa0137599c01f967";
    hash = "sha256-aL2ZhA9Zt543WsNFKDQQlnfEGEXJVXa+OnBUQKXDXAw=";
  };

  nativeBuildInputs = [ gnupg ];
  dontBuild = true;

  installPhase = ''
    runHook preInstall

    install -d "$out"
    install -m0444 gpg-only-active-keys/pubring.kbx "$out/pubring.kbx"
    GNUPGHOME="$TMPDIR/gnupg"
    install -d -m0700 "$GNUPGHOME"
    gpg \
      --batch \
      --no-default-keyring \
      --keyring "$out/pubring.kbx" \
      --with-colons \
      --fingerprint |
      awk -F: '$1 == "fpr" { print $10 }' |
      LC_ALL=C sort -u >"$out/fingerprints"
    test -s "$out/fingerprints"

    runHook postInstall
  '';
}
