---
title: helmr deployment
description: Inspect and promote Deployment resources.
sidebarLabel: deployment
---

# `helmr deployment`

```text
helmr deployment list [-p PROJECT] [-e ENV] [--json]
helmr deployment get DEPLOYMENT [-p PROJECT] [-e ENV] [--json]
helmr deployment promote DEPLOYMENT [-p PROJECT] [-e ENV]
```

`list` and `get` are read operations. `promote` moves the selected
Environment's current Deployment pointer to an already deployed version. It
prints the version to stdout and its effects to stderr, including each restored
Agent and each trigger that waits for a Slack connection; see
[Deployment promotion](/docs/reference/api/overview#deployment-promotion).
