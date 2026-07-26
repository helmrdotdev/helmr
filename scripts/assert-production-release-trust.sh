#!/usr/bin/env bash
set -euo pipefail

if (($# == 0)); then
  echo "usage: scripts/assert-production-release-trust.sh <release-tool>..." >&2
  exit 1
fi

for tool in "$@"; do
  [ -x "$tool" ] || {
    echo "release trust assertion requires an executable: $tool" >&2
    exit 1
  }
  "$tool" trust-policy |
    jq -e \
      --arg issuer "https://token.actions.githubusercontent.com" \
      --arg san_pattern '^https://github\.com/helmrdotdev/helmr/\.github/workflows/release\.yaml@refs/tags/v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:(?:0|[1-9][0-9]*)|(?:[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))(?:\.(?:(?:0|[1-9][0-9]*)|(?:[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)))*)?$' \
      'keys == ["issuer", "mode", "sanPattern"] and
       .mode == "production" and .issuer == $issuer and
       .sanPattern == $san_pattern' >/dev/null
  if strings "$tool" | grep -Fq 'release.yaml@refs/heads/'; then
    echo "production release tool contains the development release identity" >&2
    exit 1
  fi
done
