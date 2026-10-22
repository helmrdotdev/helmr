#!/bin/sh
set -eu
# This directory is a UID-13-owned VOLUME in the pinned image. ECS copies its
# ownership when the task mount matches the image's declared volume path.
printf '%s' "$SQUID_CONFIG" | base64 -d > /var/spool/squid/squid.conf
exec /usr/sbin/squid -N -f /var/spool/squid/squid.conf
