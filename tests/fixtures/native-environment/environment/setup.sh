#!/bin/sh
# Build environment preparation, run once as root by build.builder before the
# dependency install. jq is not part of Helmr's builder image; the project's
# postinstall needs it. (SQLite's development package, which the native addon
# compiles against, already ships in the builder image.)
set -eu
# The copied bytes are the literally named ones.
[ "$(cat /opt/native-environment/literal-file.txt)" = "literal bracket file" ]
[ "$(ls /opt/native-environment/literal-directory)" = "name[1].txt" ]
[ "$(cat '/opt/native-environment/literal-directory/name[1].txt')" = "literal bracket file" ]
[ "$(cat '/opt/native-environment/$TMPDIR/dollar.txt')" = "literal dollar file" ]
[ ! -e /opt/native-environment/tmp ]
apt-get update
apt-get install -y --no-install-recommends jq
rm -rf /var/lib/apt/lists/*
printf 'prepared with: %s\n' "$(cat /etc/helmr-recipe-input)" >/etc/helmr-recipe-note
# Different on every execution. The install and the later declaration analysis
# must both see this one value: the environment is prepared once, not per phase.
date +%s%N >/etc/helmr-environment-stamp
dpkg --audit
