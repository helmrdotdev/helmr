# Computer Secrets

This Agent checks that `API_TOKEN` reached its Computer process without printing
its value. Before deployment, create the `API_TOKEN` Secret in the target Environment. The
Template definition binds the same `API_TOKEN` identifier to a raw environment variable. Secret values
must not be passed as Turn input or committed to source.

```bash
helmr secret create API_TOKEN --project PROJECT --env ENVIRONMENT --json
helmr deploy PATH/TO/agent-secrets --project PROJECT --env ENVIRONMENT
helmr session create use-secret --input-json '[]' --project PROJECT --env ENVIRONMENT --json
```

The Secret command reads its value from standard input when no value argument
is supplied. Provide it through your existing secret-management workflow, before deployment. The same declaration can deploy to each Environment that
has its own active `API_TOKEN` Secret.
