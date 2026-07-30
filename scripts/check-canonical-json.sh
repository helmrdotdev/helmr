#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: scripts/check-canonical-json.sh FILE" >&2
  exit 2
fi

input=$1
canonical=
jq -e -s 'length == 1' "$input" >/dev/null ||
  {
    echo "$input must contain exactly one JSON value" >&2
    exit 1
  }
canonical=$(jq -cS -s '.[0]' "$input")
printf '%s' "$canonical" | cmp -s - "$input" ||
  {
    echo "$input is not canonical JSON" >&2
    exit 1
  }
