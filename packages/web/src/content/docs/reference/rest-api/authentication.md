---
title: REST authentication
description: Authenticate Environment-scoped requests to /v1.
sidebarLabel: Authentication
---

# REST authentication

Send an Environment-bound Helmr API key as a Bearer credential:

```http
Authorization: Bearer hlmr_sk_...
```

The key determines the Organization, Project, Environment, and permissions for
the request. `/v1` callers do not send separate project or environment path
segments. A missing, malformed, expired, revoked, or insufficient credential
is rejected by the Control Plane.

```sh
curl "$HELMR_API_URL/v1/runs" \
  -H "Authorization: Bearer $HELMR_API_KEY"
```

Keep keys out of source control and logs. The TypeScript `HelmrClient` accepts
the same key as `apiKey`; the CLI reads `HELMR_API_KEY`.

Browser session authentication, Token public-access credentials, Worker
enrollment credentials, and Admin authorization are separate contracts and do
not authenticate a Developer API request merely because they are Bearer-like.
