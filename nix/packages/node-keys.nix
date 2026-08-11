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
    export GNUPGHOME="$TMPDIR/gnupg"
    install -d -m0700 "$GNUPGHOME"

    gpg \
      --batch \
      --no-options \
      --no-default-keyring \
      --keyring gpg-only-active-keys/pubring.kbx \
      --export >"$TMPDIR/node-release-keyring-a.gpg"
    gpg \
      --batch \
      --no-options \
      --no-default-keyring \
      --keyring gpg-only-active-keys/pubring.kbx \
      --export >"$TMPDIR/node-release-keyring-b.gpg"
    test -s "$TMPDIR/node-release-keyring-a.gpg"
    cmp "$TMPDIR/node-release-keyring-a.gpg" "$TMPDIR/node-release-keyring-b.gpg"
    install -m0444 "$TMPDIR/node-release-keyring-a.gpg" "$out/node-release-keyring.gpg"

    gpg \
      --batch \
      --no-options \
      --no-default-keyring \
      --keyring "$out/node-release-keyring.gpg" \
      --with-colons \
      --with-subkey-fingerprint \
      --fingerprint |
      awk -F: '$1 == "fpr" { print $10 }' |
      LC_ALL=C sort -u >"$TMPDIR/fingerprints"
    cmp "$TMPDIR/fingerprints" ${./node-release-key-fingerprints.txt}
    install -m0444 "$TMPDIR/fingerprints" "$out/fingerprints"

    runHook postInstall
  '';
}
