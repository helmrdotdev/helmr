# Workspace Secrets

This Task expects `API_TOKEN` after the Secret has been attached to the selected
Workspace. Values are delivered to an admitted Workspace process and must not
be passed through Task payload.

```bash
helmr secret set API_TOKEN "secret-value"
helmr deploy PATH/TO/task-secrets
```
