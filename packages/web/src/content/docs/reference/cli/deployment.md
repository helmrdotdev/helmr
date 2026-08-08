---
title: helmr deployment
description: Inspect and promote Deployment resources.
sidebarLabel: deployment
---

# `helmr deployment`

```text
helmr deployment list [-p PROJECT] [-e ENV] [--json]
helmr deployment get DEPLOYMENT [-p PROJECT] [-e ENV] [--json]
helmr deployment promote DEPLOYMENT [-p PROJECT] [-e ENV] [--reason TEXT]
```

`list` and `get` are read operations. `promote` moves the selected
Environment's current Deployment pointer to an already deployed version;
`--reason` records an optional promotion reason.
