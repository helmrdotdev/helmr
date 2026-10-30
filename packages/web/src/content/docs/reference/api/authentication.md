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

The key identifies its Organization, Project and Environment. Current Role
assignments determine allowed operations and Agent targets for
the request. `/v1` callers do not send separate project or environment path
segments. A missing, malformed, expired, revoked, or insufficient credential
is rejected by the Control Plane.

```sh
curl "$HELMR_API_URL/v1/sessions" \
  -H "Authorization: Bearer $HELMR_API_KEY"
```

To guard against using a key from the wrong Environment, send the Project and
Environment IDs you expect:

```http
X-Helmr-Project: acme
X-Helmr-Environment: production
```

Each header is optional and compared with the key's own Project or Environment
ID. A mismatch is rejected with `403` and the code `api_key_scope_mismatch`,
whose message names the key's actual Project and Environment.

Keep keys out of source control and logs. The TypeScript `HelmrClient` accepts
the same key as `apiKey`; the CLI reads `HELMR_API_KEY`.

Browser session authentication, Worker enrollment credentials and Admin authorization are separate contracts and do
not authenticate a Developer API request merely because they are Bearer-like.
