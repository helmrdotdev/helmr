---
title: helmr secret
description: Manage remote Secret metadata and values.
sidebarLabel: secret
---

# `helmr secret`

```text
helmr secret list [--json]
helmr secret get SECRET_ID [--json]
helmr secret create NAME [VALUE] [--value VALUE] [--idempotency-key KEY] [--json]
helmr secret rotate SECRET_ID [VALUE] [--value VALUE] [--idempotency-key KEY] [--json]
helmr secret revoke SECRET_ID --yes [--idempotency-key KEY]
```

All commands accept project/environment scope. Create and rotate read the value
from stdin when neither positional `VALUE` nor `--value` is supplied. Retrieve
and list return metadata only; secret values are never returned. Revocation is
destructive and requires `--yes`.
