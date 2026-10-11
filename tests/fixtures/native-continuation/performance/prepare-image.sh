#!/bin/sh
set -eu
cd /opt/native-performance/tools
npm ci --ignore-scripts --no-audit --no-fund --update-notifier=false
cd /opt/native-performance/dependencies
npm ci --ignore-scripts --no-audit --no-fund --update-notifier=false --cache /opt/native-performance/npm-cache
rm -rf node_modules
# Native commands read these pre-fetched tarballs and write logs in their own
# Session directory, including under the provider's workspace write boundary.
chmod -R a+rX /opt/native-performance/npm-cache
