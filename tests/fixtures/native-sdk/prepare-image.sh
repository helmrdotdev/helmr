#!/bin/sh
set -eu
cd /workspace/native-sdk
npm ci --ignore-scripts --no-audit --no-fund
# The development build host permits HTTPS egress. Use Node's bundled trust
# roots to bootstrap apt on the slim image, then install the system CA store.
node -e 'require("node:fs").writeFileSync("/tmp/native-sdk-ca.pem", require("node:tls").rootCertificates.join("\n") + "\n")'
sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources
apt-get -o Acquire::https::CaInfo=/tmp/native-sdk-ca.pem update
apt-get -o Acquire::https::CaInfo=/tmp/native-sdk-ca.pem install -y --no-install-recommends ca-certificates iproute2 util-linux tini
rm -rf /var/lib/apt/lists/* /root/.npm /tmp/native-sdk-ca.pem
