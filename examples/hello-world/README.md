# Hello World

The smallest Helmr task shape: define an image, attach it to a sandbox, accept
payload, and write a file into the GitHub checkout workspace.

```bash
helmr deploy PATH/TO/hello-world

helmr run hello-world \
  --repo OWNER/REPO \
  --ref main \
  --subpath PATH/TO/hello-world \
  --payload name=Ada
```
