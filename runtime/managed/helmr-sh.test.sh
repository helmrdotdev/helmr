#!/usr/bin/env bash
set -euo pipefail

root="$TMPDIR/helmr-sh-test"
mkdir -p "$root"

"$CC" -std=c11 -Wall -Wextra -Werror -O2 \
  -DHELMR_NODE_PATH="\"$root/fake-node\"" \
  "$src/runtime/managed/helmr-sh.c" -o "$root/helmr-sh"

"$CC" -std=c11 -Wall -Wextra -Werror -O2 -x c -o "$root/fake-node" - <<'EOF'
#include <stdio.h>
#include <stdlib.h>

int main(int argc, char **argv) {
  const char *names[] = {
    "NODE_OPTIONS", "NODE_PATH", "NODE_EXTRA_CA_CERTS", "NODE_ICU_DATA",
    "SSL_CERT_FILE", "SSL_CERT_DIR", "OPENSSL_CONF", "OPENSSL_MODULES",
    "OPENSSL_ENGINES", "GCONV_PATH", "LOCPATH", "LD_PRELOAD", "NODE_ENV"
  };
  for (size_t index = 0; index < sizeof(names) / sizeof(names[0]); index++) {
    const char *value = getenv(names[index]);
    printf("%s=%s\n", names[index], value == NULL ? "<absent>" : value);
  }
  for (int index = 0; index < argc; index++) {
    printf("argv[%d]=<%s>\n", index, argv[index]);
  }
  return 0;
}
EOF

shim="$root/shim"
printf '%s\n%s\n' \
  '#!/opt/helmr/runtime/bin/helmr-sh' \
  'exec /opt/helmr/runtime/bin/node --import=file:///opt/helmr/runtime/helmr/preload.mjs '\''/opt/helmr/program/pkg/it'\''\'\'''\''s.js'\'' "$@"' \
  >"$shim"

env \
  NODE_OPTIONS=hostile \
  NODE_PATH=hostile \
  NODE_EXTRA_CA_CERTS=hostile \
  NODE_ICU_DATA=hostile \
  SSL_CERT_FILE=hostile \
  SSL_CERT_DIR=hostile \
  OPENSSL_CONF=hostile \
  OPENSSL_MODULES=hostile \
  OPENSSL_ENGINES=hostile \
  GCONV_PATH=hostile \
  LOCPATH=hostile \
  LD_PRELOAD= \
  NODE_ENV=production \
  "$root/helmr-sh" "$shim" "with space" "" >"$root/output"

grep -Fx 'NODE_OPTIONS=<absent>' "$root/output"
grep -Fx 'NODE_PATH=<absent>' "$root/output"
grep -Fx 'NODE_EXTRA_CA_CERTS=<absent>' "$root/output"
grep -Fx 'NODE_ICU_DATA=<absent>' "$root/output"
grep -Fx 'SSL_CERT_FILE=<absent>' "$root/output"
grep -Fx 'SSL_CERT_DIR=<absent>' "$root/output"
grep -Fx 'OPENSSL_ENGINES=<absent>' "$root/output"
grep -Fx 'LD_PRELOAD=<absent>' "$root/output"
grep -Fx 'GCONV_PATH=/opt/helmr/runtime/lib/gconv' "$root/output"
grep -Fx 'LOCPATH=/opt/helmr/runtime/lib/locale' "$root/output"
grep -Fx 'OPENSSL_CONF=/opt/helmr/runtime/lib/openssl/openssl.cnf' "$root/output"
grep -Fx 'OPENSSL_MODULES=/opt/helmr/runtime/lib/ossl-modules' "$root/output"
grep -Fx 'NODE_ENV=production' "$root/output"
grep -Fx "argv[2]=</opt/helmr/program/pkg/it's.js>" "$root/output"
grep -Fx 'argv[3]=<with space>' "$root/output"
grep -Fx 'argv[4]=<>' "$root/output"

long_target="/opt/helmr/program"
"$CC" -std=c11 -Wall -Wextra -Werror -O2 -x c -o "$root/path-max" - <<'EOF'
#include <limits.h>
#include <stdio.h>

int main(void) {
  printf("%d\n", PATH_MAX);
  return 0;
}
EOF
path_max="$("$root/path-max")"
quote_component="$(printf "%100s" "" | tr ' ' "'")"
while [ "$((${#long_target} + ${#quote_component} + 1))" -lt "$((path_max - 1))" ]; do
  long_target="$long_target/$quote_component"
done
escaped_target="$(printf '%s' "$long_target" | sed "s/'/'\\\\''/g")"
printf '%s\n%s\n' \
  '#!/opt/helmr/runtime/bin/helmr-sh' \
  "exec /opt/helmr/runtime/bin/node --import=file:///opt/helmr/runtime/helmr/preload.mjs '$escaped_target' \"\$@\"" \
  >"$root/long-shim"
"$root/helmr-sh" "$root/long-shim" >"$root/long-output"
grep -Fx "argv[2]=<$long_target>" "$root/long-output"

reject() {
  printf '%s' "$1" >"$root/bad-shim"
  if "$root/helmr-sh" "$root/bad-shim" >/dev/null 2>&1; then
    echo "accepted malformed shim" >&2
    exit 1
  fi
}

reject $'#!/opt/helmr/runtime/bin/helmr-sh\r\nexec /opt/helmr/runtime/bin/node --import=file:///opt/helmr/runtime/helmr/preload.mjs \047/opt/helmr/program/a.js\047 "$@"\n'
reject $'#!/opt/helmr/runtime/bin/helmr-sh\nexec /opt/helmr/runtime/bin/node --import=file:///opt/helmr/runtime/helmr/preload.mjs \047/opt/helmr/program/../a.js\047 "$@"\n'
reject $'#!/opt/helmr/runtime/bin/helmr-sh\nexec /opt/helmr/runtime/bin/node --import=file:///opt/helmr/runtime/helmr/preload.mjs \047/tmp/a.js\047 "$@"\n'
reject $'#!/opt/helmr/runtime/bin/helmr-sh\nexec /opt/helmr/runtime/bin/node --import=file:///opt/helmr/runtime/helmr/preload.mjs \047/opt/helmr/program/a.js\047 "$@"\nextra\n'
reject $'#!/opt/helmr/runtime/bin/helmr-sh\nexec /opt/helmr/runtime/bin/node --import=file:///opt/helmr/runtime/helmr/preload.mjs \047/opt/helmr/program/a\047b.js\047 "$@"\n'

if "$root/helmr-sh" >/dev/null 2>&1; then
  echo "accepted missing shim operand" >&2
  exit 1
fi
