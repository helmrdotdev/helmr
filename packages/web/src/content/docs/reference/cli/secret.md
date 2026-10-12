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
and list return metadata only; secret values are never returned. Revocation is
destructive and requires `--yes`.

`SECRET` is the permanent, Environment-scoped identifier chosen at creation, such
as `GITHUB_TOKEN`. The same value appears as `secret` in the API and SDK and is
used in Template bindings. No UUID lookup is needed.
