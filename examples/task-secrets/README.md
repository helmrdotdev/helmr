# Computer Secrets

This Agent checks that `API_TOKEN` reached its Computer process without printing
its value. Before deployment, create the Environment Secret and replace the
example UUID in `tasks/use-secret.ts` with the returned stable Secret ID. The
Computer definition binds that ID to a raw environment variable. Secret values
must not be passed as Turn input or committed to source.

```bash
helmr secret create API_TOKEN --project PROJECT --env ENVIRONMENT --json
helmr deploy PATH/TO/task-secrets --project PROJECT --env ENVIRONMENT
helmr agent start use-secret --input-json '[]' --project PROJECT --env ENVIRONMENT --json
```

The Secret command reads its value from standard input when no value argument
is supplied. Provide it through your existing secret-management workflow, then
edit the declaration before deployment. The checked-in UUID is a placeholder,
not a usable Secret or a name lookup.
