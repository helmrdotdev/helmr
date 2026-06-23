# Task Secrets

Declare task secrets in source. Secret values are injected into the guest at run
time and should not be passed through payload.

```bash
helmr secret set API_TOKEN "secret-value"
helmr deploy PATH/TO/task-secrets

helmr task start use-secret
```
