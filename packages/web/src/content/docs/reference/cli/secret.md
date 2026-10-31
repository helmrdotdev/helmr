---
title: helmr secret
description: Manage remote Secret metadata and values.
sidebarLabel: secret
---

# `helmr secret`

```text
helmr secret list [--json]
helmr secret get SECRET [--json]
helmr secret create SECRET [VALUE] [--value VALUE] [--idempotency-key KEY] [--json]
helmr secret rotate SECRET [VALUE] [--value VALUE] [--idempotency-key KEY] [--json]
helmr secret revoke SECRET --yes [--idempotency-key KEY]
```

All commands accept project/environment scope. Create and rotate read the value
from stdin when neither positional `VALUE` nor `--value` is supplied. Retrieve
and list return metadata only; secret values are never returned. Revocation
requires `--yes`.

`create` only makes new names. Creating a name that already exists fails; when
that Secret is revoked, the error is `secret_revoked` and you rotate it instead.
`rotate` sets the next version of the same Secret, and rotating a revoked Secret
reactivates it with its existing bindings.

`revoke` stops Helmr from providing the Secret: protected requests that need it
fail and new work that needs its raw value is refused until it is rotated.
Running work continues and raw values already delivered stay. Revoke the
credential at its provider, then use `helmr computer list --secret SECRET` to
find Computers that received a raw value.

`SECRET` is the permanent, Environment-scoped identifier chosen at creation, such
as `GITHUB_TOKEN`. The same value appears as `secret` in the API and SDK and is
used in Template bindings. No UUID lookup is needed.
