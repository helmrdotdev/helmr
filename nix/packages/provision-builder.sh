# Run inside the pinned Debian root, with only the locked archives available.
set -eu
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
export DEBIAN_FRONTEND=noninteractive
work=$(mktemp -d)
cd "$work"
apt-mark showmanual > original-manual
apt-get --no-download --no-install-recommends -y install "$1"/*.deb

tar xf "$2"
cd openssh-10.0p1
tar xf "$3"
cp "$4" debian/patches/helmr-client-rekey.patch
echo helmr-client-rekey.patch >> debian/patches/series
sed -i '1iopenssh (1:10.0p1-7+deb13u4+helmr1) trixie; urgency=medium\n\n  * Backport the client rekey lifetime fix for CVE-2026-60002.\n\n -- Helmr Build <build@helmr.dev>  Tue, 03 Nov 2026 00:00:00 +0000\n' debian/changelog
DEB_BUILD_OPTIONS="nocheck parallel=2" dpkg-buildpackage --build=binary -Ppkg.openssh.nognome -us -uc
cd "$work"
dpkg -i openssh-client_*+helmr1_amd64.deb

# APT treats each archive supplied on the command line as manually requested.
# Retain the slim base's original roots and only the tools we promise to ship.
apt-mark showmanual | sort > installed-manual
sort -o original-manual original-manual
comm -13 original-manual installed-manual | xargs -r apt-mark auto
apt-mark manual build-essential ca-certificates git libsqlite3-dev openssh-client pkg-config python3 zlib1g-dev
apt-get -y --purge autoremove
apt-get check
test -z "$(dpkg --audit)"
test "$(dpkg-query -W -f='${Version}' openssh-client)" = '1:10.0p1-7+deb13u4+helmr1'
cd /
rm -rf "$work"
rm -f /var/log/dpkg.log /var/log/apt/* /var/log/alternatives.log
