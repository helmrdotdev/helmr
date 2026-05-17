# Secret Vault

Declare a task secret and bind it from the Helmr remote vault at run time. Secret
values are injected into the guest and should not be passed through payload.

```bash
helmr secret set api-token "secret-value"
helmr deploy PATH/TO/secret-vault

helmr run use-secret \
  --repo OWNER/REPO \
  --ref main \
  --subpath PATH/TO/secret-vault \
  --secret API_TOKEN=vault:api-token
```
