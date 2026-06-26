# Hello World

The smallest Helmr task shape: define an image, attach it to a sandbox, accept
payload, and write a file into the run workspace.

```bash
helmr deploy PATH/TO/hello-world

helmr session start hello-world \
  --payload name=Ada
```
